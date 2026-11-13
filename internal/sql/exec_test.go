package sql

import (
	"fmt"
	"math"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/ABDULRAIYAAN/vaultdb/internal/store"
)

func mustExec(t *testing.T, e *Executor, q string) Result {
	t.Helper()
	r, err := e.Exec(q)
	if err != nil {
		t.Fatalf("%s\n  error: %v", q, err)
	}
	return r
}

func rows(t *testing.T, e *Executor, q string) [][]any {
	t.Helper()
	return mustExec(t, e, q).Rows
}

func setup(t *testing.T) *Executor {
	t.Helper()
	e := NewExecutor(store.New())
	mustExec(t, e, `CREATE TABLE students (id INT PRIMARY KEY, name TEXT, gpa FLOAT, active BOOL)`)
	mustExec(t, e, `INSERT INTO students VALUES
		(3, 'Raiyaan', 3.9, TRUE),
		(1, 'Ali', 3.2, TRUE),
		(2, 'Sara', 3.6, FALSE),
		(-5, 'Old', NULL, FALSE)`)
	return e
}

func TestSelect(t *testing.T) {
	e := setup(t)
	cases := []struct {
		q    string
		want [][]any
	}{
		// Rows come back in primary key order, negatives first.
		{`SELECT id FROM students`, [][]any{{int64(-5)}, {int64(1)}, {int64(2)}, {int64(3)}}},
		{`SELECT name FROM students WHERE id = 2`, [][]any{{"Sara"}}},
		{`SELECT name FROM students WHERE 2 = id`, [][]any{{"Sara"}}},
		{`SELECT name FROM students WHERE id = 99`, nil},
		{`SELECT name FROM students WHERE gpa > 3.5 ORDER BY gpa DESC`, [][]any{{"Raiyaan"}, {"Sara"}}},
		{`SELECT name FROM students WHERE active = TRUE AND gpa < 3.5`, [][]any{{"Ali"}}},
		{`SELECT name FROM students WHERE name = 'Ali' OR name = 'Sara'`, [][]any{{"Ali"}, {"Sara"}}},
		{`SELECT name FROM students WHERE NOT active = TRUE`, [][]any{{"Old"}, {"Sara"}}},
		{`SELECT name FROM students WHERE gpa IS NULL`, [][]any{{"Old"}}},
		{`SELECT name FROM students WHERE gpa > 0`, [][]any{{"Ali"}, {"Sara"}, {"Raiyaan"}}}, // NULL row skipped
		{`SELECT name, gpa FROM students ORDER BY gpa`, [][]any{{"Old", nil}, {"Ali", 3.2}, {"Sara", 3.6}, {"Raiyaan", 3.9}}},
		{`SELECT name FROM students ORDER BY name LIMIT 2`, [][]any{{"Ali"}, {"Old"}}},
		{`SELECT COUNT(*) FROM students WHERE active = TRUE`, [][]any{{int64(2)}}},
		{`SELECT name FROM students WHERE gpa >= 3`, [][]any{{"Ali"}, {"Sara"}, {"Raiyaan"}}}, // INT vs FLOAT
	}
	for _, c := range cases {
		if got := rows(t, e, c.q); !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s\n got %v\nwant %v", c.q, got, c.want)
		}
	}
}

func TestAggregates(t *testing.T) {
	e := setup(t) // gpa: 3.9, 3.2, 3.6 and one NULL
	cases := []struct {
		q    string
		cols []string
		want [][]any
	}{
		{`SELECT COUNT(*), COUNT(gpa) FROM students`, []string{"count", "count(gpa)"}, [][]any{{int64(4), int64(3)}}},
		{`SELECT SUM(id), MIN(id), MAX(id) FROM students`, []string{"sum(id)", "min(id)", "max(id)"},
			[][]any{{int64(1), int64(-5), int64(3)}}}, // -5 + 1 + 2 + 3
		{`SELECT MIN(gpa), MAX(gpa) FROM students`, []string{"min(gpa)", "max(gpa)"}, [][]any{{3.2, 3.9}}}, // NULL skipped
		{`SELECT MIN(name), MAX(name) FROM students`, []string{"min(name)", "max(name)"}, [][]any{{"Ali", "Sara"}}},
		{`SELECT COUNT(*), AVG(id) FROM students WHERE active = TRUE`, []string{"count", "avg(id)"}, [][]any{{int64(2), 2.0}}},
		// No matching rows: COUNT is 0, everything else is NULL.
		{`SELECT COUNT(*), SUM(gpa), AVG(gpa), MIN(name) FROM students WHERE id > 100`,
			[]string{"count", "sum(gpa)", "avg(gpa)", "min(name)"}, [][]any{{int64(0), nil, nil, nil}}},
		// Only NULLs: same as no values.
		{`SELECT COUNT(gpa), SUM(gpa) FROM students WHERE id = -5`, []string{"count(gpa)", "sum(gpa)"}, [][]any{{int64(0), nil}}},
		// LIMIT applies to the one result row, not to the rows being counted.
		{`SELECT COUNT(*) FROM students LIMIT 1`, []string{"count"}, [][]any{{int64(4)}}},
		{`SELECT COUNT(*) FROM students LIMIT 0`, []string{"count"}, nil},
	}
	for _, c := range cases {
		r := mustExec(t, e, c.q)
		if !reflect.DeepEqual(r.Columns, c.cols) || !reflect.DeepEqual(r.Rows, c.want) {
			t.Errorf("%s\n got %v %v\nwant %v %v", c.q, r.Columns, r.Rows, c.cols, c.want)
		}
	}

	// Floating-point sums are not exact, so check them with a tolerance.
	r := mustExec(t, e, `SELECT SUM(gpa), AVG(gpa) FROM students`)
	sum, avg := r.Rows[0][0].(float64), r.Rows[0][1].(float64)
	if math.Abs(sum-10.7) > 1e-9 || math.Abs(avg-10.7/3) > 1e-9 {
		t.Errorf("SUM, AVG of gpa = %v, %v; want 10.7, %v", sum, avg, 10.7/3)
	}

	bad := map[string]string{
		`SELECT SUM(name) FROM students`:               "SUM needs an INT or FLOAT column, but name is TEXT",
		`SELECT AVG(active) FROM students`:             "AVG needs an INT or FLOAT column",
		`SELECT MAX(nope) FROM students`:               "no column nope",
		`SELECT MAX(nope) FROM students WHERE id > 99`: "no column nope", // reported even with no rows
		`SELECT COUNT(*) FROM students ORDER BY id`:    "ORDER BY cannot be used with aggregate",
	}
	for q, msg := range bad {
		if _, err := e.Exec(q); err == nil || !strings.Contains(err.Error(), msg) {
			t.Errorf("%s\n error %v, want %q", q, err, msg)
		}
	}
}

func setupGroups(t *testing.T) *Executor {
	t.Helper()
	e := NewExecutor(store.New())
	mustExec(t, e, `CREATE TABLE staff (id INT PRIMARY KEY, name TEXT, dept TEXT, year INT, salary INT)`)
	mustExec(t, e, `INSERT INTO staff VALUES
		(1, 'Ali',     'eng',   2024, 100),
		(2, 'Sara',    'eng',   2025, 120),
		(3, 'Raiyaan', 'eng',   2025, 140),
		(4, 'Mei',     'sales', 2024, 80),
		(5, 'Tom',     'sales', 2024, NULL),
		(6, 'Zed',     NULL,    2025, 50)`)
	return e
}

func TestGroupBy(t *testing.T) {
	e := setupGroups(t)
	cases := []struct {
		q    string
		cols []string
		want [][]any
	}{
		// Groups come back sorted by their GROUP BY values, NULL first. All
		// rows with a NULL dept form one group.
		{`SELECT dept, COUNT(*), SUM(salary) FROM staff GROUP BY dept`,
			[]string{"dept", "count", "sum(salary)"},
			[][]any{{nil, int64(1), int64(50)}, {"eng", int64(3), int64(360)}, {"sales", int64(2), int64(80)}}},
		// The select list keeps the order it was written in.
		{`SELECT AVG(salary), dept FROM staff WHERE dept IS NOT NULL GROUP BY dept`,
			[]string{"avg(salary)", "dept"}, [][]any{{120.0, "eng"}, {80.0, "sales"}}},
		// Several GROUP BY columns: one group per combination.
		{`SELECT dept, year, COUNT(*) FROM staff WHERE dept IS NOT NULL GROUP BY dept, year`,
			[]string{"dept", "year", "count"},
			[][]any{{"eng", int64(2024), int64(1)}, {"eng", int64(2025), int64(2)}, {"sales", int64(2024), int64(2)}}},
		// GROUP BY without aggregates lists each distinct value once.
		{`SELECT year FROM staff GROUP BY year`, []string{"year"}, [][]any{{int64(2024)}, {int64(2025)}}},
		// A GROUP BY column does not have to be selected.
		{`SELECT COUNT(*) FROM staff GROUP BY year`, []string{"count"}, [][]any{{int64(3)}, {int64(3)}}},
		// ORDER BY an aggregate, then LIMIT the number of groups.
		{`SELECT dept, MAX(salary) FROM staff GROUP BY dept ORDER BY MAX(salary) DESC LIMIT 2`,
			[]string{"dept", "max(salary)"}, [][]any{{"eng", int64(140)}, {"sales", int64(80)}}},
		// ORDER BY an aggregate that is not in the select list.
		{`SELECT dept FROM staff GROUP BY dept ORDER BY COUNT(*)`,
			[]string{"dept"}, [][]any{{nil}, {"sales"}, {"eng"}}},
		{`SELECT dept, COUNT(*) FROM staff GROUP BY dept ORDER BY dept DESC`,
			[]string{"dept", "count"}, [][]any{{"sales", int64(2)}, {"eng", int64(3)}, {nil, int64(1)}}},
		// A group whose salaries are all NULL: COUNT(col) 0, SUM NULL.
		{`SELECT name, COUNT(salary), SUM(salary) FROM staff WHERE id = 5 GROUP BY name`,
			[]string{"name", "count(salary)", "sum(salary)"}, [][]any{{"Tom", int64(0), nil}}},
		// No matching rows means no groups (unlike an aggregate without
		// GROUP BY, which always returns one row).
		{`SELECT dept, COUNT(*) FROM staff WHERE id > 100 GROUP BY dept`, []string{"dept", "count"}, nil},
		{`SELECT COUNT(*) FROM staff WHERE id > 100`, []string{"count"}, [][]any{{int64(0)}}},
		{`SELECT dept, COUNT(*) FROM staff GROUP BY dept LIMIT 0`, []string{"dept", "count"}, nil},
	}
	for _, c := range cases {
		r := mustExec(t, e, c.q)
		if !reflect.DeepEqual(r.Columns, c.cols) || !reflect.DeepEqual(r.Rows, c.want) {
			t.Errorf("%s\n got %v %v\nwant %v %v", c.q, r.Columns, r.Rows, c.cols, c.want)
		}
	}

	bad := map[string]string{
		`SELECT nope, COUNT(*) FROM staff GROUP BY nope`:                 "no column nope",
		`SELECT COUNT(*) FROM staff WHERE id > 99 GROUP BY nope`:         "no column nope", // reported even with no rows
		`SELECT dept, SUM(name) FROM staff GROUP BY dept`:                "SUM needs an INT or FLOAT column",
		`SELECT dept, COUNT(*) FROM staff GROUP BY dept ORDER BY salary`: "only be sorted by a GROUP BY column or an aggregate",
		`SELECT dept FROM staff GROUP BY dept ORDER BY AVG(name)`:        "AVG needs an INT or FLOAT column",
		`SELECT COUNT(*) FROM staff ORDER BY COUNT(*)`:                   "ORDER BY cannot be used with aggregate",
		`SELECT name FROM staff ORDER BY COUNT(*)`:                       "needs GROUP BY",
	}
	for q, msg := range bad {
		if _, err := e.Exec(q); err == nil || !strings.Contains(err.Error(), msg) {
			t.Errorf("%s\n error %v, want %q", q, err, msg)
		}
	}
}

func TestHaving(t *testing.T) {
	e := setupGroups(t)
	cases := []struct {
		q    string
		cols []string
		want [][]any
	}{
		// Keep only groups with more than one row.
		{`SELECT dept, COUNT(*) FROM staff GROUP BY dept HAVING COUNT(*) > 1`,
			[]string{"dept", "count"}, [][]any{{"eng", int64(3)}, {"sales", int64(2)}}},
		// The aggregate in HAVING does not have to be selected.
		{`SELECT dept FROM staff GROUP BY dept HAVING AVG(salary) > 90`,
			[]string{"dept"}, [][]any{{"eng"}}},
		// GROUP BY columns can be used too, mixed with aggregates.
		{`SELECT dept, year, COUNT(*) FROM staff GROUP BY dept, year HAVING year = 2024 AND COUNT(*) >= 1`,
			[]string{"dept", "year", "count"},
			[][]any{{"eng", int64(2024), int64(1)}, {"sales", int64(2024), int64(2)}}},
		{`SELECT dept FROM staff GROUP BY dept HAVING dept IS NULL OR MIN(salary) < 90`,
			[]string{"dept"}, [][]any{{nil}, {"sales"}}},
		// A NULL condition drops the group, as in WHERE: for the NULL dept,
		// dept = 'eng' is NULL, and NOT NULL is still NULL.
		{`SELECT dept FROM staff GROUP BY dept HAVING NOT dept = 'eng'`,
			[]string{"dept"}, [][]any{{"sales"}}},
		// WHERE runs first, then HAVING, then ORDER BY and LIMIT.
		{`SELECT dept, SUM(salary) FROM staff WHERE year = 2025 GROUP BY dept HAVING SUM(salary) > 0 ORDER BY SUM(salary) DESC LIMIT 1`,
			[]string{"dept", "sum(salary)"}, [][]any{{"eng", int64(260)}}},
		// Without GROUP BY the whole table is one group, so HAVING keeps
		// that one row or returns none.
		{`SELECT COUNT(*) FROM staff HAVING COUNT(*) > 5`, []string{"count"}, [][]any{{int64(6)}}},
		{`SELECT COUNT(*) FROM staff HAVING COUNT(*) > 6`, []string{"count"}, nil},
		// Every group filtered out.
		{`SELECT dept FROM staff GROUP BY dept HAVING COUNT(*) > 100`, []string{"dept"}, nil},
	}
	for _, c := range cases {
		r := mustExec(t, e, c.q)
		if !reflect.DeepEqual(r.Columns, c.cols) || !reflect.DeepEqual(r.Rows, c.want) {
			t.Errorf("%s\n got %v %v\nwant %v %v", c.q, r.Columns, r.Rows, c.cols, c.want)
		}
	}

	bad := map[string]string{
		`SELECT dept FROM staff GROUP BY dept HAVING SUM(name) > 1`:               "SUM needs an INT or FLOAT column",
		`SELECT dept FROM staff WHERE id > 99 GROUP BY dept HAVING MAX(nope) > 1`: "no column nope", // reported even with no rows
		`SELECT dept FROM staff GROUP BY dept HAVING COUNT(*) > 'many'`:           "cannot compare INT with TEXT",
		`SELECT dept FROM staff GROUP BY dept HAVING salary > 1`:                  "must be listed in GROUP BY",
		`SELECT * FROM staff WHERE COUNT(*) > 1`:                                  "use HAVING",
	}
	for q, msg := range bad {
		if _, err := e.Exec(q); err == nil || !strings.Contains(err.Error(), msg) {
			t.Errorf("%s\n error %v, want %q", q, err, msg)
		}
	}
}

func TestDistinct(t *testing.T) {
	e := setupGroups(t)
	mustExec(t, e, `CREATE TABLE tags (tag TEXT, n INT)`) // no primary key, so rows can repeat
	mustExec(t, e, `INSERT INTO tags VALUES ('a', 1), ('a', 1), ('b', NULL), ('b', NULL), ('a', 2)`)
	cases := []struct {
		q    string
		cols []string
		want [][]any
	}{
		// The first copy of each row is kept, in the order rows were read.
		{`SELECT DISTINCT dept FROM staff`, []string{"dept"}, [][]any{{"eng"}, {"sales"}, {nil}}},
		{`SELECT DISTINCT dept FROM staff ORDER BY dept`, []string{"dept"}, [][]any{{nil}, {"eng"}, {"sales"}}},
		{`SELECT DISTINCT dept, year FROM staff`, []string{"dept", "year"},
			[][]any{{"eng", int64(2024)}, {"eng", int64(2025)}, {"sales", int64(2024)}, {nil, int64(2025)}}},
		// LIMIT counts rows after duplicates are gone: two different depts,
		// not the first two rows (which are both eng).
		{`SELECT DISTINCT dept FROM staff LIMIT 2`, []string{"dept"}, [][]any{{"eng"}, {"sales"}}},
		{`SELECT DISTINCT year FROM staff WHERE salary > 60 ORDER BY year DESC`, []string{"year"},
			[][]any{{int64(2025)}, {int64(2024)}}},
		// DISTINCT * compares whole rows; NULLs count as equal.
		{`SELECT DISTINCT * FROM tags`, []string{"tag", "n"}, [][]any{{"a", int64(1)}, {"b", nil}, {"a", int64(2)}}},
		{`SELECT DISTINCT tag FROM tags WHERE n IS NULL`, []string{"tag"}, [][]any{{"b"}}},

		// Aggregates over distinct values only. NULLs are still skipped.
		{`SELECT COUNT(DISTINCT dept), COUNT(dept), COUNT(DISTINCT year) FROM staff`,
			[]string{"count(distinct dept)", "count(dept)", "count(distinct year)"},
			[][]any{{int64(2), int64(5), int64(2)}}},
		{`SELECT SUM(DISTINCT n), SUM(n), AVG(DISTINCT n), MAX(DISTINCT n) FROM tags`,
			[]string{"sum(distinct n)", "sum(n)", "avg(distinct n)", "max(distinct n)"},
			[][]any{{int64(3), int64(4), 1.5, int64(2)}}},
		{`SELECT COUNT(DISTINCT n) FROM tags WHERE n IS NULL`, []string{"count(distinct n)"}, [][]any{{int64(0)}}},
		// Per group, and in HAVING.
		{`SELECT dept, COUNT(DISTINCT year) FROM staff GROUP BY dept`,
			[]string{"dept", "count(distinct year)"},
			[][]any{{nil, int64(1)}, {"eng", int64(2)}, {"sales", int64(1)}}},
		{`SELECT dept FROM staff GROUP BY dept HAVING COUNT(DISTINCT year) > 1`, []string{"dept"}, [][]any{{"eng"}}},

		// DISTINCT on a grouped result: both years have 3 people.
		{`SELECT DISTINCT COUNT(*) FROM staff GROUP BY year`, []string{"count"}, [][]any{{int64(3)}}},
		{`SELECT DISTINCT COUNT(DISTINCT year) FROM staff GROUP BY dept ORDER BY COUNT(DISTINCT year) DESC`,
			[]string{"count(distinct year)"}, [][]any{{int64(2)}, {int64(1)}}},
		{`SELECT DISTINCT COUNT(*) FROM staff GROUP BY dept LIMIT 2`, []string{"count"}, [][]any{{int64(1)}, {int64(3)}}},
	}
	for _, c := range cases {
		r := mustExec(t, e, c.q)
		if !reflect.DeepEqual(r.Columns, c.cols) || !reflect.DeepEqual(r.Rows, c.want) {
			t.Errorf("%s\n got %v %v\nwant %v %v", c.q, r.Columns, r.Rows, c.cols, c.want)
		}
	}

	bad := map[string]string{
		`SELECT DISTINCT nope FROM staff`:                      "no column nope",
		`SELECT COUNT(DISTINCT nope) FROM staff WHERE id > 99`: "no column nope", // reported even with no rows
		`SELECT SUM(DISTINCT name) FROM staff`:                 "SUM needs an INT or FLOAT column",
		`SELECT DISTINCT dept FROM staff ORDER BY salary`:      "must be in the select list",
	}
	for q, msg := range bad {
		if _, err := e.Exec(q); err == nil || !strings.Contains(err.Error(), msg) {
			t.Errorf("%s\n error %v, want %q", q, err, msg)
		}
	}
}

// TestDistinctNegativeZero checks that 0.0 and -0.0, which are equal, are
// not kept apart by DISTINCT, COUNT(DISTINCT) or GROUP BY.
func TestDistinctNegativeZero(t *testing.T) {
	e := NewExecutor(store.New())
	mustExec(t, e, `CREATE TABLE f (x FLOAT)`)
	mustExec(t, e, `INSERT INTO f VALUES (0.0), (-0.0), (1.5)`)
	for q, want := range map[string]int{
		`SELECT DISTINCT x FROM f`:             2,
		`SELECT x, COUNT(*) FROM f GROUP BY x`: 2,
	} {
		if got := rows(t, e, q); len(got) != want {
			t.Errorf("%s: %d rows %v, want %d", q, len(got), got, want)
		}
	}
	if got := rows(t, e, `SELECT COUNT(DISTINCT x) FROM f`); got[0][0] != int64(2) {
		t.Errorf("COUNT(DISTINCT x) = %v, want 2", got[0][0])
	}
}

// TestDistinctMatchesManualCount checks DISTINCT and COUNT(DISTINCT) against
// a Go map on a bigger table with many repeated values.
func TestDistinctMatchesManualCount(t *testing.T) {
	e := NewExecutor(store.New())
	mustExec(t, e, `CREATE TABLE events (id INT PRIMARY KEY, kind INT, user TEXT)`)
	kinds, pairs := map[int64]bool{}, map[string]bool{}
	var b strings.Builder
	b.WriteString(`INSERT INTO events VALUES `)
	for i := 0; i < 400; i++ {
		kind, user := int64(i*5%11-3), fmt.Sprintf("u%d", i%7)
		kinds[kind] = true
		pairs[fmt.Sprint(kind, user)] = true
		if i > 0 {
			b.WriteString(", ")
		}
		fmt.Fprintf(&b, "(%d, %d, '%s')", i, kind, user)
	}
	mustExec(t, e, b.String())

	if got := rows(t, e, `SELECT DISTINCT kind FROM events`); len(got) != len(kinds) {
		t.Errorf("DISTINCT kind: %d rows, want %d", len(got), len(kinds))
	}
	if got := rows(t, e, `SELECT DISTINCT kind, user FROM events`); len(got) != len(pairs) {
		t.Errorf("DISTINCT kind, user: %d rows, want %d", len(got), len(pairs))
	}
	if got := rows(t, e, `SELECT COUNT(DISTINCT kind) FROM events`); got[0][0] != int64(len(kinds)) {
		t.Errorf("COUNT(DISTINCT kind) = %v, want %d", got[0][0], len(kinds))
	}
}

// TestGroupByMatchesManualCount checks GROUP BY against counting by hand on
// a bigger table with many repeated values.
func TestGroupByMatchesManualCount(t *testing.T) {
	e := NewExecutor(store.New())
	mustExec(t, e, `CREATE TABLE events (id INT PRIMARY KEY, kind INT, score FLOAT)`)
	want := map[int64]int64{}
	var b strings.Builder
	b.WriteString(`INSERT INTO events VALUES `)
	for i := 0; i < 500; i++ {
		kind := int64(i*7%13 - 6) // repeats, includes negatives
		want[kind]++
		if i > 0 {
			b.WriteString(", ")
		}
		fmt.Fprintf(&b, "(%d, %d, %d.5)", i, kind, i%10)
	}
	mustExec(t, e, b.String())

	got := rows(t, e, `SELECT kind, COUNT(*) FROM events GROUP BY kind`)
	if len(got) != len(want) {
		t.Fatalf("%d groups, want %d", len(got), len(want))
	}
	prev := int64(math.MinInt64)
	for _, r := range got {
		kind, n := r[0].(int64), r[1].(int64)
		if n != want[kind] {
			t.Errorf("kind %d: count %d, want %d", kind, n, want[kind])
		}
		if kind <= prev {
			t.Errorf("groups not in order: %d after %d", kind, prev)
		}
		prev = kind
	}
}

func TestSumOverflow(t *testing.T) {
	e := NewExecutor(store.New())
	mustExec(t, e, `CREATE TABLE big (n INT)`)
	mustExec(t, e, `INSERT INTO big VALUES (9223372036854775807), (1)`)
	if _, err := e.Exec(`SELECT SUM(n) FROM big`); err == nil || !strings.Contains(err.Error(), "too large") {
		t.Fatalf("overflowing SUM: error %v", err)
	}
	// AVG adds up in floating point, so it does not overflow.
	if got := rows(t, e, `SELECT AVG(n) FROM big`); got[0][0].(float64) < 4e18 {
		t.Fatalf("AVG = %v", got)
	}
}

func TestInsertRules(t *testing.T) {
	e := setup(t)
	bad := map[string]string{
		`INSERT INTO students VALUES (1, 'Dup', 1.0, TRUE)`:                    "already exists",
		`INSERT INTO students VALUES (9, 'X', 1.0, TRUE), (9, 'Y', 1.0, TRUE)`: "already exists",
		`INSERT INTO students VALUES (NULL, 'X', 1.0, TRUE)`:                   "cannot be NULL",
		`INSERT INTO students VALUES ('nine', 'X', 1.0, TRUE)`:                 "column id is INT, but got TEXT",
		`INSERT INTO students VALUES (9, 'X')`:                                 "2 values but 4 columns",
		`INSERT INTO students (id, nope) VALUES (9, 1)`:                        "no column nope",
		`INSERT INTO nobody VALUES (1)`:                                        "does not exist",
		`INSERT INTO students VALUES (9, name, 1.0, TRUE)`:                     "cannot be used here",
	}
	for q, msg := range bad {
		if _, err := e.Exec(q); err == nil || !strings.Contains(err.Error(), msg) {
			t.Errorf("%s\n error %v, want %q", q, err, msg)
		}
	}
	// A failing multi-row INSERT writes nothing.
	if got := rows(t, e, `SELECT COUNT(*) FROM students`); got[0][0] != int64(4) {
		t.Fatalf("count after failed inserts = %v", got)
	}
	// Named columns, any order; missing ones become NULL; INT into FLOAT is fine.
	mustExec(t, e, `INSERT INTO students (name, id, gpa) VALUES ('New', 10, 4)`)
	if got := rows(t, e, `SELECT * FROM students WHERE id = 10`); !reflect.DeepEqual(got, [][]any{{int64(10), "New", 4.0, nil}}) {
		t.Fatalf("named insert = %v", got)
	}
}

func TestTableWithoutPrimaryKey(t *testing.T) {
	e := NewExecutor(store.New())
	mustExec(t, e, `CREATE TABLE logs (msg TEXT)`)
	mustExec(t, e, `INSERT INTO logs VALUES ('a'), ('a'), ('b')`) // duplicates allowed
	if got := rows(t, e, `SELECT msg FROM logs`); !reflect.DeepEqual(got, [][]any{{"a"}, {"a"}, {"b"}}) {
		t.Fatalf("insertion order not kept: %v", got)
	}
}

func TestUpdateAndDelete(t *testing.T) {
	e := setup(t)
	if r := mustExec(t, e, `UPDATE students SET gpa = 4.0, active = TRUE WHERE id = 2`); r.Message != "OK, 1 row updated" {
		t.Fatal(r.Message)
	}
	if got := rows(t, e, `SELECT gpa, active FROM students WHERE id = 2`); !reflect.DeepEqual(got, [][]any{{4.0, true}}) {
		t.Fatalf("after update: %v", got)
	}
	mustExec(t, e, `UPDATE students SET name = name WHERE active = TRUE`) // right side reads the row
	if _, err := e.Exec(`UPDATE students SET id = 7 WHERE id = 1`); err == nil {
		t.Fatal("changing the primary key should fail")
	}
	if _, err := e.Exec(`UPDATE students SET gpa = 'high'`); err == nil {
		t.Fatal("wrong type in UPDATE should fail")
	}
	if r := mustExec(t, e, `DELETE FROM students WHERE active = FALSE`); r.Message != "OK, 1 row deleted" {
		t.Fatal(r.Message)
	}
	if r := mustExec(t, e, `DELETE FROM students`); r.Message != "OK, 3 rows deleted" {
		t.Fatal(r.Message)
	}
}

func TestSchemaErrors(t *testing.T) {
	e := setup(t)
	bad := map[string]string{
		`CREATE TABLE students (x INT)`:                         "already exists",
		`CREATE TABLE t (a INT, a TEXT)`:                        "listed twice",
		`CREATE TABLE t (a INT PRIMARY KEY, b INT PRIMARY KEY)`: "only one PRIMARY KEY",
		`CREATE TABLE t (a FLOAT PRIMARY KEY)`:                  "must be INT or TEXT",
		`SELECT nope FROM students`:                             "no column nope",
		`SELECT * FROM students WHERE nope = 1`:                 "unknown column nope",
		`SELECT * FROM students ORDER BY nope`:                  "no column nope",
		`SELECT * FROM students WHERE name > 3`:                 "cannot compare TEXT with INT",
		`DROP TABLE nobody`:                                     "does not exist",
	}
	for q, msg := range bad {
		if _, err := e.Exec(q); err == nil || !strings.Contains(err.Error(), msg) {
			t.Errorf("%s\n error %v, want %q", q, err, msg)
		}
	}
}

func TestShowAndDrop(t *testing.T) {
	e := setup(t)
	mustExec(t, e, `CREATE TABLE courses (code TEXT PRIMARY KEY)`)
	if got := rows(t, e, `SHOW TABLES`); !reflect.DeepEqual(got, [][]any{{"courses"}, {"students"}}) {
		t.Fatalf("SHOW TABLES = %v", got)
	}
	if r := mustExec(t, e, `DROP TABLE students`); r.Message != "OK, table students dropped (4 rows deleted)" {
		t.Fatal(r.Message)
	}
	if got := rows(t, e, `SHOW TABLES`); !reflect.DeepEqual(got, [][]any{{"courses"}}) {
		t.Fatalf("after drop: %v", got)
	}
}

// Tables are just keys, so they survive a restart like everything else.
func TestTablesSurviveRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "vaultdb.log")
	st, _, err := store.Open(path, store.DefaultOptions())
	if err != nil {
		t.Fatal(err)
	}
	e := NewExecutor(st)
	mustExec(t, e, `CREATE TABLE notes (id INT PRIMARY KEY, body TEXT)`)
	mustExec(t, e, `INSERT INTO notes VALUES (1, 'remember the milk'), (2, 'it''s done')`)
	st.Close()

	st2, _, err := store.Open(path, store.DefaultOptions())
	if err != nil {
		t.Fatal(err)
	}
	defer st2.Close()
	got := rows(t, NewExecutor(st2), `SELECT body FROM notes ORDER BY id DESC`)
	if !reflect.DeepEqual(got, [][]any{{"it's done"}, {"remember the milk"}}) {
		t.Fatalf("after restart: %v", got)
	}
}

func TestFormatTable(t *testing.T) {
	e := setup(t)
	got := FormatTable(mustExec(t, e, `SELECT id, name, gpa FROM students WHERE id <= 1`))
	want := `+----+-------+------+
| id | name  | gpa  |
+----+-------+------+
| -5 | 'Old' | NULL |
| 1  | 'Ali' | 3.2  |
+----+-------+------+
(2 rows)`
	if got != want {
		t.Fatalf("got\n%s\nwant\n%s", got, want)
	}
	if got := FormatTable(mustExec(t, e, `SELECT name FROM students WHERE id = 42`)); got != "+------+\n| name |\n+------+\n(0 rows)" {
		t.Fatalf("empty result:\n%s", got)
	}
}
