package sql

import (
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
