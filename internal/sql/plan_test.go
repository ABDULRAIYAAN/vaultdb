package sql

import (
	"fmt"
	"math"
	"math/rand"
	"reflect"
	"strings"
	"testing"

	"github.com/ABDULRAIYAAN/vaultdb/internal/store"
)

// planFor parses "SELECT * FROM table WHERE where" and returns its plan.
func planFor(t *testing.T, e *Executor, table, where string) access {
	t.Helper()
	st, err := Parse("SELECT * FROM " + table + " WHERE " + where)
	if err != nil {
		t.Fatalf("%s: %v", where, err)
	}
	s, err := e.loadSchema(table)
	if err != nil {
		t.Fatal(err)
	}
	return planAccess(table, s, st.(Select).Where)
}

func TestPlanAccess(t *testing.T) {
	e := setup(t) // students: INT primary key id
	mustExec(t, e, `CREATE TABLE nopk (a INT)`)
	cases := []struct {
		table, where string
		kind         accessKind
		describe     string
	}{
		{"students", `id = 2`, keyLookup, "lookup students by primary key: id = 2 (1 key)"},
		{"students", `2 = id`, keyLookup, "lookup students by primary key: id = 2 (1 key)"},
		{"students", `id IN (5, 1, 5)`, keyLookup, "lookup students by primary key: id IN (1, 5) (2 keys)"},
		{"students", `id > 1`, rangeScan, "range scan of students by primary key: id > 1"},
		{"students", `1 < id`, rangeScan, "range scan of students by primary key: id > 1"},
		{"students", `3 >= id`, rangeScan, "range scan of students by primary key: id <= 3"},
		{"students", `id >= 1 AND id < 3`, rangeScan, "range scan of students by primary key: id >= 1 AND id < 3"},
		{"students", `id BETWEEN -5 AND 2`, rangeScan, "range scan of students by primary key: id >= -5 AND id <= 2"},
		// The tightest bound on each side wins, wherever it is in the AND chain.
		{"students", `id > 0 AND name LIKE 'A%' AND id >= 1 AND (id < 9 AND id <= 3)`, rangeScan,
			"range scan of students by primary key: id >= 1 AND id <= 3"},
		{"students", `id > 1 AND id >= 1`, rangeScan, "range scan of students by primary key: id > 1"},
		// Keys and bounds together: only the keys inside the bounds are read.
		{"students", `id IN (1, 2, 3, 9) AND id > 1 AND id < 9`, keyLookup,
			"lookup students by primary key: id IN (2, 3) (2 keys)"},
		{"students", `id IN (1, 2, 3) AND id IN (3, 2, 7)`, keyLookup,
			"lookup students by primary key: id IN (2, 3) (2 keys)"},
		{"students", `id = 1 AND gpa > 3`, keyLookup, "lookup students by primary key: id = 1 (1 key)"},
		// Conditions that can never be TRUE read nothing.
		{"students", `id = 1 AND id = 2`, noRows, ""},
		{"students", `id > 5 AND id < 5`, noRows, ""},
		{"students", `id > 5 AND id <= 5`, noRows, ""},
		{"students", `id BETWEEN 9 AND 1`, noRows, ""},
		{"students", `id = NULL`, noRows, ""},
		{"students", `gpa > 1 AND id < NULL`, noRows, ""},
		{"students", `id BETWEEN NULL AND 5`, noRows, ""},
		{"students", `id IN (NULL)`, noRows, ""},
		{"students", `id IN (1, 2) AND id > 2`, noRows, ""},
		// No help: these read the whole table.
		{"students", `gpa > 3`, fullScan, "full scan of students"},
		{"students", `id > 1 OR id < -1`, fullScan, "full scan of students"},
		{"students", `NOT id > 1`, fullScan, "full scan of students"},
		{"students", `id != 1`, fullScan, "full scan of students"},
		{"students", `id NOT IN (1, 2)`, fullScan, "full scan of students"},
		{"students", `id NOT BETWEEN 1 AND 2`, fullScan, "full scan of students"},
		{"students", `id > gpa`, fullScan, "full scan of students"},
		{"students", `id IN (1, gpa)`, fullScan, "full scan of students"},
		{"students", `id > 2.5`, fullScan, "full scan of students"},   // not an INT, left to the per-row check
		{"students", `id = 'one'`, fullScan, "full scan of students"}, // the per-row check reports the type error
		{"nopk", `a = 1`, fullScan, "full scan of nopk"},
	}
	for _, c := range cases {
		a := planFor(t, e, c.table, c.where)
		if a.kind != c.kind {
			t.Errorf("%s: kind %d, want %d", c.where, a.kind, c.kind)
			continue
		}
		if c.describe != "" && a.describe(c.table) != c.describe {
			t.Errorf("%s\n got %q\nwant %q", c.where, a.describe(c.table), c.describe)
		}
	}
}

// The planner must give exactly the rows a full table read gives, for any
// mix of conditions. "NOT NOT (...)" hides the conditions from the planner
// (it never looks inside NOT), so it is the full-read answer to compare with.
func TestPlannerMatchesFullScan(t *testing.T) {
	st := &scanCounter{Store: store.New()}
	e := NewExecutor(st)
	mustExec(t, e, `CREATE TABLE n (id INT PRIMARY KEY, v INT)`)
	mustExec(t, e, `CREATE TABLE s (k TEXT PRIMARY KEY, v INT)`)
	// Neighbours whose names start like "s" and keys that would fall in a
	// range if the table prefix weren't part of the bounds.
	mustExec(t, e, `CREATE TABLE sa (k TEXT PRIMARY KEY, v INT)`)
	mustExec(t, e, `CREATE TABLE s_ (k TEXT PRIMARY KEY, v INT)`)
	rng := rand.New(rand.NewSource(7))
	for _, v := range []int64{math.MinInt64, math.MaxInt64, 0, -1} {
		mustExec(t, e, fmt.Sprintf(`INSERT INTO n VALUES (%d, 0)`, v))
	}
	for i := 0; i < 150; i++ {
		e.Exec(fmt.Sprintf(`INSERT INTO n VALUES (%d, %d)`, rng.Int63n(401)-200, rng.Intn(5))) // repeats are refused, fine
	}
	word := func() string {
		b := make([]byte, rng.Intn(4))
		for i := range b {
			b[i] = "ab~ "[rng.Intn(4)]
		}
		return string(b)
	}
	for _, table := range []string{"s", "sa", "s_"} {
		for i := 0; i < 60; i++ {
			e.Exec(fmt.Sprintf(`INSERT INTO %s VALUES ('%s', %d)`, table, word(), rng.Intn(5)))
		}
	}

	intVal := func() string {
		switch rng.Intn(12) {
		case 0:
			return "NULL"
		case 1:
			return fmt.Sprint(int64(math.MinInt64))
		case 2:
			return fmt.Sprint(int64(math.MaxInt64))
		}
		return fmt.Sprint(rng.Int63n(461) - 230)
	}
	textVal := func() string {
		if rng.Intn(12) == 0 {
			return "NULL"
		}
		return "'" + word() + "'"
	}
	ops := []string{"=", "<", "<=", ">", ">=", "!="}
	cond := func(pk string, val func() string) string {
		switch rng.Intn(8) {
		case 0:
			return fmt.Sprintf("%s IN (%s, %s, %s)", pk, val(), val(), val())
		case 1:
			return fmt.Sprintf("%s BETWEEN %s AND %s", pk, val(), val())
		case 2:
			return fmt.Sprintf("v %s %d", ops[rng.Intn(len(ops))], rng.Intn(5))
		case 3:
			return fmt.Sprintf("(%s %s %s OR v = 1)", pk, ops[rng.Intn(len(ops))], val())
		case 4:
			return fmt.Sprintf("%s %s %s", val(), ops[rng.Intn(len(ops))], pk) // literal first
		}
		return fmt.Sprintf("%s %s %s", pk, ops[rng.Intn(len(ops))], val())
	}

	for i := 0; i < 2000; i++ {
		table, pk, val := "n", "id", intVal
		if i%2 == 1 {
			table, pk, val = "s", "k", textVal
		}
		parts := make([]string, 1+rng.Intn(3))
		for j := range parts {
			parts[j] = cond(pk, val)
		}
		where := strings.Join(parts, " AND ")

		before := st.prefixReads
		got := rows(t, e, fmt.Sprintf(`SELECT * FROM %s WHERE %s`, table, where))
		if kind := planFor(t, e, table, where).kind; kind != fullScan && st.prefixReads != before {
			t.Fatalf("%s: plan %d still read the whole table", where, kind)
		}
		before = st.prefixReads
		want := rows(t, e, fmt.Sprintf(`SELECT * FROM %s WHERE NOT NOT (%s)`, table, where))
		if st.prefixReads != before+1 {
			t.Fatalf("NOT NOT (%s) did not read the whole table", where)
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("%s WHERE %s\n got %v\nwant %v", table, where, got, want)
		}
	}
}

// UPDATE and DELETE go through the same planner.
func TestPlannedUpdateAndDelete(t *testing.T) {
	st := &scanCounter{Store: store.New()}
	e := NewExecutor(st)
	mustExec(t, e, `CREATE TABLE n (id INT PRIMARY KEY, v INT)`)
	for i := 1; i <= 10; i++ {
		mustExec(t, e, fmt.Sprintf(`INSERT INTO n VALUES (%d, 0)`, i))
	}
	if r := mustExec(t, e, `UPDATE n SET v = 1 WHERE id > 3 AND id <= 6`); r.Message != "OK, 3 rows updated" {
		t.Fatal(r.Message)
	}
	if r := mustExec(t, e, `DELETE FROM n WHERE 8 <= id`); r.Message != "OK, 3 rows deleted" {
		t.Fatal(r.Message)
	}
	if st.prefixReads != 0 {
		t.Fatalf("%d full table reads, want 0", st.prefixReads)
	}
	got := rows(t, e, `SELECT id FROM n WHERE v = 1`)
	if want := [][]any{{int64(4)}, {int64(5)}, {int64(6)}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("updated %v, want %v", got, want)
	}
	if got := rows(t, e, `SELECT COUNT(*) FROM n`); got[0][0] != int64(7) {
		t.Fatalf("%v rows left, want 7", got[0][0])
	}
}

func TestExplain(t *testing.T) {
	e := setup(t)
	cases := []struct{ q, want string }{
		{`EXPLAIN SELECT * FROM students`, "1. full scan of students"},
		{`explain select name from students where id = 2`,
			"1. lookup students by primary key: id = 2 (1 key)\n2. filter rows: id = 2"},
		{`EXPLAIN SELECT name FROM students WHERE id > 1 AND (gpa > 3 OR name LIKE 'A%') ORDER BY gpa DESC LIMIT 2`,
			"1. range scan of students by primary key: id > 1\n" +
				"2. filter rows: id > 1 AND (gpa > 3 OR name LIKE 'A%')\n" +
				"3. sort by gpa DESC\n" +
				"4. keep the first 2"},
		{`EXPLAIN SELECT DISTINCT active FROM students WHERE id BETWEEN 1 AND 3`,
			"1. range scan of students by primary key: id >= 1 AND id <= 3\n" +
				"2. filter rows: id BETWEEN 1 AND 3\n" +
				"3. drop duplicate rows"},
		{`EXPLAIN SELECT active, COUNT(*) FROM students GROUP BY active HAVING COUNT(*) > 1 ORDER BY COUNT(*) DESC`,
			"1. full scan of students\n2. group by active\n3. filter groups: COUNT(*) > 1\n4. sort by count DESC"},
		{`EXPLAIN SELECT AVG(gpa) FROM students WHERE id IN (1, 2)`,
			"1. lookup students by primary key: id IN (1, 2) (2 keys)\n2. filter rows: id IN (1, 2)\n3. aggregate all rows into one"},
		{`EXPLAIN SELECT * FROM students WHERE id = 1 AND id = 2 ORDER BY name`,
			"1. read nothing from students: no row can match the primary key condition"},
		{`EXPLAIN UPDATE students SET gpa = 4.0 WHERE id <= 2`,
			"1. range scan of students by primary key: id <= 2\n2. filter rows: id <= 2\n3. update each matching row"},
		{`EXPLAIN DELETE FROM students WHERE name = 'Ali'`,
			"1. full scan of students\n2. filter rows: name = 'Ali'\n3. delete each matching row"},
	}
	for _, c := range cases {
		if got := mustExec(t, e, c.q).Message; got != c.want {
			t.Errorf("%s\n got:\n%s\nwant:\n%s", c.q, got, c.want)
		}
	}

	// EXPLAIN never runs the statement.
	mustExec(t, e, `EXPLAIN DELETE FROM students`)
	mustExec(t, e, `EXPLAIN UPDATE students SET name = 'x'`)
	if got := rows(t, e, `SELECT name FROM students WHERE id = 1`); got[0][0] != "Ali" {
		t.Fatalf("EXPLAIN changed the table: %v", got)
	}
	if got := rows(t, e, `SELECT COUNT(*) FROM students`); got[0][0] != int64(4) {
		t.Fatalf("EXPLAIN deleted rows: %v", got)
	}

	errs := []struct{ q, want string }{
		{`EXPLAIN SELECT * FROM missing`, "table missing does not exist"},
		{`EXPLAIN SELECT * FROM students WHERE nope = 1`, "unknown column nope"},
		{`EXPLAIN SELECT nope FROM students`, "table students has no column nope"},
		{`EXPLAIN SELECT SUM(nope) FROM students`, "table students has no column nope"},
		{`EXPLAIN SELECT * FROM students ORDER BY nope`, "table students has no column nope"},
		{`EXPLAIN UPDATE students SET nope = 1`, "table students has no column nope"},
		{`EXPLAIN INSERT INTO students VALUES (9, 'x', 1.0, TRUE)`, "EXPLAIN works with SELECT, UPDATE and DELETE"},
		{`EXPLAIN EXPLAIN SELECT * FROM students`, "EXPLAIN works with SELECT, UPDATE and DELETE"},
		{`EXPLAIN`, "EXPLAIN works with SELECT, UPDATE and DELETE"},
	}
	for _, c := range errs {
		_, err := e.Exec(c.q)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: error %v, want %q", c.q, err, c.want)
		}
	}
}

// exprString must write SQL that parses back into the same tree, so what
// EXPLAIN shows is exactly what runs.
func TestExprStringRoundTrip(t *testing.T) {
	wheres := []string{
		`a = 1`,
		`a = 1 AND b = 2 AND c = 3`,
		`a = 1 OR b = 2 AND c = 3`,
		`(a = 1 OR b = 2) AND c = 3`,
		`NOT (a = 1 AND b = 2)`,
		`NOT a = 1 AND b = 2`,
		`NOT NOT a IS NULL`,
		`a IS NOT NULL OR b IS NULL`,
		`a NOT IN (1, -2, 'x''y', NULL, TRUE) AND b IN (c, 2.5)`,
		`a NOT LIKE 'A\%_' OR a LIKE b`,
		`a BETWEEN 1 AND c AND d NOT BETWEEN -1.5 AND 2`,
		`(a = 1) = TRUE`,
		`(a > b) != (c < d)`,
		`a <= -0.5`,
	}
	for _, w := range wheres {
		st, err := Parse("SELECT * FROM t WHERE " + w)
		if err != nil {
			t.Fatalf("%s: %v", w, err)
		}
		ex := st.(Select).Where
		out := exprString(ex)
		back, err := Parse("SELECT * FROM t WHERE " + out)
		if err != nil {
			t.Fatalf("%s printed as %s, which doesn't parse: %v", w, out, err)
		}
		if !reflect.DeepEqual(back.(Select).Where, ex) {
			t.Errorf("%s printed as %s, which means something else", w, out)
		}
	}

	st, err := Parse(`SELECT a, COUNT(*) FROM t GROUP BY a HAVING COUNT(DISTINCT b) > 1 AND MAX(c) < 3`)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := exprString(st.(Select).Having), "COUNT(DISTINCT b) > 1 AND MAX(c) < 3"; got != want {
		t.Errorf("HAVING printed as %s, want %s", got, want)
	}
}
