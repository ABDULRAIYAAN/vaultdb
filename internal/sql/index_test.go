package sql

import (
	"errors"
	"fmt"
	"math"
	"math/rand"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/ABDULRAIYAAN/vaultdb/internal/store"
)

func TestParseIndexStatements(t *testing.T) {
	good := []struct {
		q    string
		want Statement
	}{
		{`CREATE INDEX by_dept ON staff (dept)`, CreateIndex{Name: "by_dept", Table: "staff", Column: "dept"}},
		{`create index By_Dept on Staff(Dept);`, CreateIndex{Name: "by_dept", Table: "staff", Column: "dept"}},
		{`DROP INDEX by_dept`, DropIndex{Name: "by_dept"}},
		{`show indexes`, ShowIndexes{}},
		{`SHOW TABLES`, ShowTables{}},
		{`DROP TABLE staff`, DropTable{Table: "staff"}},
	}
	for _, c := range good {
		got, err := Parse(c.q)
		if err != nil {
			t.Errorf("%s: %v", c.q, err)
			continue
		}
		if !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s: got %#v, want %#v", c.q, got, c.want)
		}
	}
	bad := []struct{ q, want string }{
		{`CREATE INDEX ON staff (dept)`, "expected ON"}, // "on" is read as the name
		{`CREATE INDEX by_dept staff (dept)`, "expected ON"},
		{`CREATE INDEX by_dept ON staff dept`, `expected "("`},
		{`CREATE INDEX by_dept ON staff (dept, year)`, "one column"},
		{`CREATE INDEX by_dept ON staff ()`, "expected column name"},
		{`CREATE VIEW v`, "expected TABLE or INDEX"},
		{`DROP INDEX`, "expected index name"},
		{`DROP VIEW v`, "expected TABLE or INDEX"},
		{`SHOW COLUMNS`, "expected TABLES or INDEXES"},
	}
	for _, c := range bad {
		_, err := Parse(c.q)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: error %v, want %q", c.q, err, c.want)
		}
	}
}

// INDEX and ON are not reserved words, so tables made before indexes
// existed, with columns called "index" or "on", still work.
func TestIndexWordsAreNotReserved(t *testing.T) {
	e := NewExecutor(store.New())
	mustExec(t, e, `CREATE TABLE index (on INT PRIMARY KEY, index TEXT)`)
	mustExec(t, e, `INSERT INTO index VALUES (1, 'a')`)
	mustExec(t, e, `CREATE INDEX indexes ON index (index)`)
	got := rows(t, e, `SELECT on FROM index WHERE index = 'a'`)
	if !reflect.DeepEqual(got, [][]any{{int64(1)}}) {
		t.Fatalf("got %v", got)
	}
}

func TestIndexLifecycle(t *testing.T) {
	st := &scanCounter{Store: store.New()}
	e := NewExecutor(st)
	mustExec(t, e, `CREATE TABLE staff (id INT PRIMARY KEY, name TEXT, dept TEXT, year INT)`)
	mustExec(t, e, `INSERT INTO staff VALUES
		(1, 'Ali', 'eng', 2024), (2, 'Sara', 'eng', 2025), (3, 'Raiyaan', 'eng', 2025),
		(4, 'Mei', 'sales', 2024), (5, 'Tom', NULL, 2024)`)

	// Built from the rows already there; the NULL dept gets no entry.
	if r := mustExec(t, e, `CREATE INDEX by_dept ON staff (dept)`); r.Message != "OK, index by_dept created on staff (dept) with 4 entries" {
		t.Fatal(r.Message)
	}
	mustExec(t, e, `CREATE INDEX by_year ON staff (year)`)
	got := rows(t, e, `SHOW INDEXES`)
	want := [][]any{{"staff", "by_dept", "dept"}, {"staff", "by_year", "year"}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("SHOW INDEXES: %v", got)
	}

	q := func(sql string) [][]any {
		t.Helper()
		before := st.prefixReads
		r := rows(t, e, sql)
		if st.prefixReads != before {
			t.Fatalf("%s read the whole table", sql)
		}
		return r
	}
	ids := func(sql string) []int64 {
		t.Helper()
		var out []int64
		for _, r := range q(sql) {
			out = append(out, r[0].(int64))
		}
		return out
	}
	check := func(sql string, want ...int64) {
		t.Helper()
		if got := ids(sql); !reflect.DeepEqual(got, want) {
			t.Errorf("%s: got %v, want %v", sql, got, want)
		}
	}

	check(`SELECT id FROM staff WHERE dept = 'eng'`, 1, 2, 3)
	check(`SELECT id FROM staff WHERE dept IN ('sales', 'ops') AND year = 2024`, 4)
	check(`SELECT id FROM staff WHERE year > 2024`, 2, 3)
	check(`SELECT id FROM staff WHERE year BETWEEN 2020 AND 2024 AND name != 'Ali'`, 4, 5)
	check(`SELECT id FROM staff WHERE dept >= 'f'`, 4)
	check(`SELECT id FROM staff WHERE dept = NULL`)
	if got := q(`SELECT COUNT(*) FROM staff WHERE dept = 'eng' GROUP BY year ORDER BY COUNT(*) DESC`); !reflect.DeepEqual(got, [][]any{{int64(2)}, {int64(1)}}) {
		t.Errorf("grouped: %v", got)
	}

	// INSERT, UPDATE and DELETE keep the index up to date.
	mustExec(t, e, `INSERT INTO staff VALUES (6, 'Zed', 'sales', 2026)`)
	check(`SELECT id FROM staff WHERE dept = 'sales'`, 4, 6)
	mustExec(t, e, `UPDATE staff SET dept = 'sales' WHERE id = 1`)
	mustExec(t, e, `UPDATE staff SET dept = 'ops' WHERE dept = 'sales' AND year = 2024`) // planned by the index it changes
	check(`SELECT id FROM staff WHERE dept = 'eng'`, 2, 3)
	check(`SELECT id FROM staff WHERE dept = 'sales'`, 6)
	check(`SELECT id FROM staff WHERE dept = 'ops'`, 1, 4)
	mustExec(t, e, `UPDATE staff SET dept = NULL WHERE id = 6`)
	mustExec(t, e, `UPDATE staff SET dept = 'eng' WHERE id = 5`)
	check(`SELECT id FROM staff WHERE dept = 'sales'`)
	check(`SELECT id FROM staff WHERE dept = 'eng'`, 2, 3, 5)
	mustExec(t, e, `DELETE FROM staff WHERE dept = 'eng' AND year = 2025`)
	check(`SELECT id FROM staff WHERE dept = 'eng'`, 5)
	check(`SELECT id FROM staff WHERE year = 2025`)
	checkIndexes(t, e, st.Store, "staff")

	// DROP INDEX removes its entries; the other index keeps working.
	if r := mustExec(t, e, `DROP INDEX by_dept`); r.Message != "OK, index by_dept dropped" {
		t.Fatal(r.Message)
	}
	if pairs, _ := st.Store.Prefix(indexPrefix+"staff:by_dept:", 0); len(pairs) != 0 {
		t.Fatalf("%d entries left after DROP INDEX", len(pairs))
	}
	check(`SELECT id FROM staff WHERE year = 2024`, 1, 4, 5)
	if got := rows(t, e, `SELECT id FROM staff WHERE dept = 'ops'`); len(got) != 2 {
		t.Fatalf("after DROP INDEX: %v", got)
	}

	// DROP TABLE removes every index entry of the table.
	mustExec(t, e, `DROP TABLE staff`)
	if pairs, _ := st.Store.Prefix(indexPrefix, 0); len(pairs) != 0 {
		t.Fatalf("%d entries left after DROP TABLE", len(pairs))
	}
	if got := rows(t, e, `SHOW INDEXES`); len(got) != 0 {
		t.Fatalf("SHOW INDEXES after DROP TABLE: %v", got)
	}
	// The name is free again.
	mustExec(t, e, `CREATE TABLE staff (id INT PRIMARY KEY, dept TEXT)`)
	mustExec(t, e, `CREATE INDEX by_dept ON staff (dept)`)
}

func TestIndexErrors(t *testing.T) {
	e := setup(t)
	mustExec(t, e, `CREATE TABLE other (a INT)`)
	mustExec(t, e, `CREATE INDEX by_name ON students (name)`)
	cases := []struct{ q, want string }{
		{`CREATE INDEX x ON missing (a)`, "table missing does not exist"},
		{`CREATE INDEX x ON students (nope)`, "table students has no column nope"},
		{`CREATE INDEX x ON students (id)`, "column id is the primary key, which is already indexed"},
		{`CREATE INDEX by_name ON other (a)`, "index by_name already exists (on table students)"},
		{`CREATE INDEX by_name2 ON students (name)`, "column name already has index by_name"},
		{`DROP INDEX nope`, "index nope does not exist"},
	}
	for _, c := range cases {
		_, err := e.Exec(c.q)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: error %v, want %q", c.q, err, c.want)
		}
	}
	// A table without a primary key can have indexes too.
	mustExec(t, e, `INSERT INTO other VALUES (5), (7), (5)`)
	mustExec(t, e, `CREATE INDEX by_a ON other (a)`)
	if got := rows(t, e, `SELECT COUNT(*) FROM other WHERE a = 5`); got[0][0] != int64(2) {
		t.Fatalf("got %v", got)
	}
}

func TestExplainIndex(t *testing.T) {
	e := setup(t)
	mustExec(t, e, `CREATE INDEX by_name ON students (name)`)
	mustExec(t, e, `CREATE INDEX by_gpa ON students (gpa)`)
	cases := []struct{ where, want string }{
		{`name = 'Ali'`, "lookup students by index by_name: name = 'Ali' (1 value)"},
		{`name IN ('Sara', 'Ali')`, "lookup students by index by_name: name IN ('Ali', 'Sara') (2 values)"},
		{`gpa >= 3.5`, "range scan of students by index by_gpa: gpa >= 3.5"},
		{`gpa > 3 AND gpa < 4`, "range scan of students by index by_gpa: gpa > 3.0 AND gpa < 4.0"},
		{`gpa = NULL`, "read nothing from students: no row can match the condition on gpa"},
		{`gpa > 4 AND gpa < 3`, "read nothing from students: no row can match the condition on gpa"},
		// Exact values beat a range, whichever column they're on...
		{`gpa > 3 AND name = 'Ali'`, "lookup students by index by_name: name = 'Ali' (1 value)"},
		{`id > 1 AND name = 'Ali'`, "lookup students by index by_name: name = 'Ali' (1 value)"},
		// ...and on a tie the primary key wins.
		{`id = 1 AND name = 'Ali'`, "lookup students by primary key: id = 1 (1 key)"},
		{`id > 1 AND gpa > 3`, "range scan of students by primary key: id > 1"},
		{`gpa > 3 OR name = 'Ali'`, "full scan of students"},
		{`active = TRUE`, "full scan of students"},
		{`gpa > 'x'`, "full scan of students"},
	}
	for _, c := range cases {
		if got := planFor(t, e, "students", c.where).describe("students"); got != c.want {
			t.Errorf("%s\n got %q\nwant %q", c.where, got, c.want)
		}
	}
	got := mustExec(t, e, `EXPLAIN SELECT name FROM students WHERE gpa BETWEEN 3.5 AND 4 ORDER BY gpa`).Message
	want := "1. range scan of students by index by_gpa: gpa >= 3.5 AND gpa <= 4.0\n2. filter rows: gpa BETWEEN 3.5 AND 4\n3. sort by gpa"
	if got != want {
		t.Errorf("EXPLAIN\n got:\n%s\nwant:\n%s", got, want)
	}
}

// Index keys must sort exactly the way compare sorts the values, or range
// scans over an index would skip rows.
func TestIndexValueOrder(t *testing.T) {
	rng := rand.New(rand.NewSource(3))
	ints := []any{int64(math.MinInt64), int64(math.MaxInt64), int64(0), int64(-1), int64(1)}
	floats := []any{math.Inf(-1), math.Inf(1), -math.MaxFloat64, math.MaxFloat64, math.Copysign(0, -1), 0.0,
		math.SmallestNonzeroFloat64, -math.SmallestNonzeroFloat64, 1.5, -1.5}
	texts := []any{"", "\x00", "\x00\x00", "\x00\x01", "\x00\xff", "\x01", "a", "a\x00", "a\x00b", "a\x01", "ab", "\xff", "\xff\x00"}
	for i := 0; i < 300; i++ {
		ints = append(ints, rng.Int63()-rng.Int63())
		floats = append(floats, rng.NormFloat64()*math.Pow(10, float64(rng.Intn(40)-20)))
		b := make([]byte, rng.Intn(4))
		for j := range b {
			b[j] = "\x00\x01\x02a\xfe\xff"[rng.Intn(6)]
		}
		texts = append(texts, string(b))
	}
	for _, vals := range [][]any{ints, floats, texts, {false, true}} {
		for _, a := range vals {
			for _, b := range vals {
				want, _ := compare(a, b)
				ka, kb := indexValueStart("t", "i", a), indexValueStart("t", "i", b)
				if got := strings.Compare(ka, kb); got != want {
					t.Fatalf("compare(%q, %q) = %d, but their keys compare %d", a, b, want, got)
				}
				// Every entry for a lies in [start(a), after(a)), and
				// nothing of b's lies there unless a = b.
				entry := indexEntry("t", "i", b, rowKey("t", int64(7)))
				inside := entry >= ka && entry < indexValueAfter("t", "i", a)
				if inside != (want == 0) {
					t.Fatalf("entry for %q inside the range of %q: %v", b, a, inside)
				}
			}
		}
	}
}

// checkIndexes makes sure every index of table holds exactly one entry per
// row with a non-NULL value, pointing at that row, and nothing else.
func checkIndexes(t *testing.T, e *Executor, st *store.Store, table string) {
	t.Helper()
	s, err := e.loadSchema(table)
	if err != nil {
		t.Fatal(err)
	}
	rowsKV, _ := st.Prefix(rowPrefix+table+":", 0)
	for _, ix := range s.Indexes {
		want := map[string]string{}
		for _, kv := range rowsKV {
			vals, err := decodeRow(kv.Value, s)
			if err != nil {
				t.Fatal(err)
			}
			if v := vals[s.index(ix.Column)]; v != nil {
				want[indexEntry(table, ix.Name, v, kv.Key)] = kv.Key
			}
		}
		from, to := indexKeys(table, ix.Name)
		pairs, _ := st.Scan(from, to, 0)
		got := map[string]string{}
		for _, kv := range pairs {
			got[kv.Key] = kv.Value
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("index %s has %d entries, want %d", ix.Name, len(got), len(want))
		}
	}
}

// Random INSERTs, UPDATEs and DELETEs on a table with an index on every
// column, checked after every statement: queries through the indexes must
// give the same rows as a full table read, and the indexes must match the
// rows exactly.
func TestIndexMatchesFullScan(t *testing.T) {
	st := &scanCounter{Store: store.New()}
	e := NewExecutor(st)
	mustExec(t, e, `CREATE TABLE t (id INT PRIMARY KEY, i INT, f FLOAT, s TEXT, b BOOL)`)
	mustExec(t, e, `CREATE TABLE t2 (id INT PRIMARY KEY, i INT)`) // a neighbour, to catch ranges leaking
	mustExec(t, e, `INSERT INTO t2 VALUES (1, 0), (2, 1)`)
	mustExec(t, e, `CREATE INDEX t2_i ON t2 (i)`)
	rng := rand.New(rand.NewSource(11))

	val := map[string]func() string{
		"i": func() string { return fmt.Sprint(rng.Intn(11) - 5) },
		"f": func() string { return []string{"-1.5", "-0.0", "0", "0.25", "1", "2.5", "3"}[rng.Intn(7)] },
		"s": func() string { return []string{"''", "'a'", "'ab'", "'b'"}[rng.Intn(4)] },
		"b": func() string { return []string{"TRUE", "FALSE"}[rng.Intn(2)] },
	}
	cols := []string{"i", "f", "s", "b"}
	maybeNull := func(c string) string {
		if rng.Intn(6) == 0 {
			return "NULL"
		}
		return val[c]()
	}
	ops := []string{"=", "<", "<=", ">", ">=", "!="}
	cond := func() string {
		c := cols[rng.Intn(len(cols))]
		switch rng.Intn(7) {
		case 0:
			return fmt.Sprintf("%s IN (%s, %s)", c, maybeNull(c), maybeNull(c))
		case 1:
			return fmt.Sprintf("%s BETWEEN %s AND %s", c, maybeNull(c), maybeNull(c))
		case 2:
			return fmt.Sprintf("%s %s %s", maybeNull(c), ops[rng.Intn(len(ops))], c)
		case 3:
			return fmt.Sprintf("id %s %d", ops[rng.Intn(5)], rng.Intn(40))
		case 4:
			return fmt.Sprintf("(%s = %s OR id = 3)", c, maybeNull(c))
		}
		return fmt.Sprintf("%s %s %s", c, ops[rng.Intn(len(ops))], maybeNull(c))
	}
	where := func() string {
		parts := make([]string, 1+rng.Intn(3))
		for j := range parts {
			parts[j] = cond()
		}
		return strings.Join(parts, " AND ")
	}

	for id := 0; id < 40; id++ {
		mustExec(t, e, fmt.Sprintf(`INSERT INTO t VALUES (%d, %s, %s, %s, %s)`, id, maybeNull("i"), maybeNull("f"), maybeNull("s"), maybeNull("b")))
		if id == 20 {
			// Half the rows exist before the indexes, half are added after.
			for _, c := range cols {
				mustExec(t, e, fmt.Sprintf(`CREATE INDEX t_%s ON t (%s)`, c, c))
			}
		}
	}
	nextID := 40
	planned := 0
	for i := 0; i < 1500; i++ {
		switch rng.Intn(4) {
		case 0:
			c := cols[rng.Intn(len(cols))]
			mustExec(t, e, fmt.Sprintf(`UPDATE t SET %s = %s WHERE %s`, c, maybeNull(c), where()))
		case 1:
			if rng.Intn(3) == 0 {
				mustExec(t, e, `DELETE FROM t WHERE `+where())
			} else {
				mustExec(t, e, fmt.Sprintf(`INSERT INTO t VALUES (%d, %s, %s, %s, %s)`, nextID, maybeNull("i"), maybeNull("f"), maybeNull("s"), maybeNull("b")))
				nextID++
			}
		}
		w := where()
		a := planFor(t, e, "t", w)
		before := st.prefixReads
		got := rows(t, e, `SELECT * FROM t WHERE `+w)
		if a.kind != fullScan {
			planned++
			if st.prefixReads != before {
				t.Fatalf("%s: plan %d still read the whole table", w, a.kind)
			}
		}
		want := rows(t, e, `SELECT * FROM t WHERE NOT NOT (`+w+`)`)
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("WHERE %s (%s)\n got %v\nwant %v", w, a.describe("t"), got, want)
		}
		if i%50 == 0 {
			checkIndexes(t, e, st.Store, "t")
		}
	}
	checkIndexes(t, e, st.Store, "t")
	checkIndexes(t, e, st.Store, "t2")
	if planned < 500 {
		t.Fatalf("only %d of 1500 queries used a plan; the test isn't testing much", planned)
	}
	if got := rows(t, e, `SELECT id FROM t2 WHERE i >= 0`); len(got) != 2 {
		t.Fatalf("t2 changed: %v", got)
	}
}

// Indexes are ordinary keys and the list of them is in the schema, so they
// survive a restart.
func TestIndexSurvivesRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "db.log")
	st, _, err := store.Open(path, store.DefaultOptions())
	if err != nil {
		t.Fatal(err)
	}
	e := NewExecutor(st)
	mustExec(t, e, `CREATE TABLE p (id INT PRIMARY KEY, city TEXT)`)
	mustExec(t, e, `INSERT INTO p VALUES (1, 'Singapore'), (2, 'Chennai'), (3, 'Singapore')`)
	mustExec(t, e, `CREATE INDEX by_city ON p (city)`)
	st.Close()

	st, _, err = store.Open(path, store.DefaultOptions())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	e = NewExecutor(st)
	if got := mustExec(t, e, `EXPLAIN SELECT * FROM p WHERE city = 'Singapore'`).Message; !strings.HasPrefix(got, "1. lookup p by index by_city") {
		t.Fatalf("after restart: %s", got)
	}
	if got := rows(t, e, `SELECT id FROM p WHERE city = 'Singapore'`); !reflect.DeepEqual(got, [][]any{{int64(1)}, {int64(3)}}) {
		t.Fatalf("after restart: %v", got)
	}
}

// failingStore stops accepting writes after a set number, like a crash
// partway through a statement.
type failingStore struct {
	*store.Store
	writesLeft int
}

var errCrash = errors.New("crash")

func (f *failingStore) Set(key, value string) error {
	if f.writesLeft == 0 {
		return errCrash
	}
	f.writesLeft--
	return f.Store.Set(key, value)
}

func (f *failingStore) Delete(key string) (bool, error) {
	if f.writesLeft == 0 {
		return false, errCrash
	}
	f.writesLeft--
	return f.Store.Delete(key)
}

// However a statement is cut short, the index may end up with extra
// entries but never be missing one, so queries through it still give the
// right rows.
func TestIndexCrashLeavesNoGaps(t *testing.T) {
	stmts := []string{
		`INSERT INTO t VALUES (10, 'x', 1), (11, 'y', 2)`,
		`UPDATE t SET a = 'z', b = 9 WHERE id <= 2`,
		`UPDATE t SET a = NULL WHERE a = 'p'`,
		`DELETE FROM t WHERE b >= 2`,
	}
	for _, q := range stmts {
		for budget := 0; ; budget++ {
			fs := &failingStore{Store: store.New(), writesLeft: -1}
			e := NewExecutor(fs)
			mustExec(t, e, `CREATE TABLE t (id INT PRIMARY KEY, a TEXT, b INT)`)
			mustExec(t, e, `CREATE INDEX by_a ON t (a)`)
			mustExec(t, e, `CREATE INDEX by_b ON t (b)`)
			mustExec(t, e, `INSERT INTO t VALUES (1, 'p', 1), (2, 'q', 2), (3, 'p', 3)`)
			fs.writesLeft = budget
			_, err := e.Exec(q)
			fs.writesLeft = -1
			if err != nil && !errors.Is(err, errCrash) {
				t.Fatalf("%s: %v", q, err)
			}
			// Every row with a value must have its entry.
			s, _ := e.loadSchema("t")
			all, _ := fs.Store.Prefix(rowPrefix+"t:", 0)
			for _, kv := range all {
				vals, _ := decodeRow(kv.Value, s)
				for _, k := range indexEntries("t", s, kv.Key, vals) {
					if _, err := fs.Store.Get(k); err != nil {
						t.Fatalf("%s cut after %d writes: row %v has no entry in an index", q, budget, vals)
					}
				}
			}
			for _, w := range []string{`a = 'p'`, `a IN ('x', 'z')`, `b > 1`, `b = 9`} {
				got := rows(t, e, `SELECT * FROM t WHERE `+w)
				want := rows(t, e, `SELECT * FROM t WHERE NOT NOT (`+w+`)`)
				if !reflect.DeepEqual(got, want) {
					t.Fatalf("%s cut after %d writes, WHERE %s: got %v, want %v", q, budget, w, got, want)
				}
			}
			if err == nil {
				break // the statement finished: every cut point has been tried
			}
		}
	}
}
