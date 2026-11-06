package sql

import (
	"reflect"
	"strings"
	"testing"
)

func TestLex(t *testing.T) {
	toks, err := Lex(`select Name, age FROM students WHERE age >= 21 AND name != 'it''s' -- comment`)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, tk := range toks {
		got = append(got, tk.Text)
	}
	want := []string{"SELECT", "Name", ",", "age", "FROM", "students", "WHERE", "age", ">=", "21",
		"AND", "name", "!=", "it's", ""}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("tokens\n got %q\nwant %q", got, want)
	}
	if toks[1].Kind != TokIdent || toks[0].Kind != TokKeyword || toks[9].Kind != TokNumber || toks[13].Kind != TokString {
		t.Fatal("wrong token kinds")
	}
}

func TestLexErrors(t *testing.T) {
	for _, q := range []string{`SELECT 'unclosed`, `SELECT # FROM t`} {
		if _, err := Lex(q); err == nil {
			t.Errorf("Lex(%q) should fail", q)
		}
	}
}

func col(n string) ColumnRef { return ColumnRef{Name: n} }
func lit(v any) Literal      { return Literal{Value: v} }
func bin(op string, l, r Expr) Binary {
	return Binary{Op: op, Left: l, Right: r}
}

func TestParse(t *testing.T) {
	cases := []struct {
		q    string
		want Statement
	}{
		{`CREATE TABLE Students (id INT PRIMARY KEY, name TEXT, gpa FLOAT, active BOOL);`,
			CreateTable{Table: "students", Columns: []ColumnDef{
				{"id", TypeInt, true}, {"name", TypeText, false}, {"gpa", TypeFloat, false}, {"active", TypeBool, false}}}},
		{`DROP TABLE students`, DropTable{Table: "students"}},
		{`show tables`, ShowTables{}},
		{`INSERT INTO students VALUES (1, 'Ali', 3.5, TRUE), (2, 'Sara', -1.25, NULL)`,
			Insert{Table: "students", Rows: [][]Expr{
				{lit(int64(1)), lit("Ali"), lit(3.5), lit(true)},
				{lit(int64(2)), lit("Sara"), lit(-1.25), lit(nil)}}}},
		{`INSERT INTO students (name, id) VALUES ('Raiyaan', 3)`,
			Insert{Table: "students", Columns: []string{"name", "id"}, Rows: [][]Expr{{lit("Raiyaan"), lit(int64(3))}}}},
		{`SELECT * FROM students`, Select{Table: "students", Limit: -1}},
		{`SELECT COUNT(*) FROM students WHERE active = TRUE`,
			Select{Count: true, Table: "students", Where: bin("=", col("active"), lit(true)), Limit: -1}},
		{`SELECT name, gpa FROM students WHERE gpa > 3 ORDER BY gpa DESC LIMIT 5`,
			Select{Columns: []string{"name", "gpa"}, Table: "students", Where: bin(">", col("gpa"), lit(int64(3))),
				OrderBy: "gpa", Desc: true, Limit: 5}},
		{`SELECT * FROM t ORDER BY a ASC`, Select{Table: "t", OrderBy: "a", Limit: -1}},
		{`UPDATE students SET gpa = 4.0, active = FALSE WHERE id = 1`,
			Update{Table: "students", Set: []Assignment{{"gpa", lit(4.0)}, {"active", lit(false)}},
				Where: bin("=", col("id"), lit(int64(1)))}},
		{`DELETE FROM students`, Delete{Table: "students"}},
		{`DELETE FROM students WHERE name IS NULL`,
			Delete{Table: "students", Where: IsNull{Expr: col("name")}}},
		{`SELECT * FROM t WHERE x IS NOT NULL`, Select{Table: "t", Where: IsNull{Expr: col("x"), Negate: true}, Limit: -1}},
		{`SELECT * FROM t WHERE a <> 1`, Select{Table: "t", Where: bin("!=", col("a"), lit(int64(1))), Limit: -1}},
	}
	for _, c := range cases {
		got, err := Parse(c.q)
		if err != nil {
			t.Errorf("Parse(%q): %v", c.q, err)
			continue
		}
		if !reflect.DeepEqual(got, c.want) {
			t.Errorf("Parse(%q)\n got %#v\nwant %#v", c.q, got, c.want)
		}
	}
}

// AND binds tighter than OR, NOT tighter than AND, and brackets override both.
func TestPrecedence(t *testing.T) {
	a, b, c := bin("=", col("a"), lit(int64(1))), bin("=", col("b"), lit(int64(2))), bin("=", col("c"), lit(int64(3)))
	cases := map[string]Expr{
		`a = 1 OR b = 2 AND c = 3`:   bin("OR", a, bin("AND", b, c)),
		`(a = 1 OR b = 2) AND c = 3`: bin("AND", bin("OR", a, b), c),
		`NOT a = 1 AND b = 2`:        bin("AND", Not{Expr: a}, b),
		`a = 1 AND b = 2 AND c = 3`:  bin("AND", bin("AND", a, b), c), // left to right
	}
	for where, want := range cases {
		st, err := Parse("SELECT * FROM t WHERE " + where)
		if err != nil {
			t.Fatalf("%s: %v", where, err)
		}
		if got := st.(Select).Where; !reflect.DeepEqual(got, want) {
			t.Errorf("WHERE %s\n got %#v\nwant %#v", where, got, want)
		}
	}
}

func TestParseErrors(t *testing.T) {
	cases := map[string]string{
		``:                                  "must start with",
		`SELEC * FROM t`:                    "must start with",
		`SELECT * students`:                 "expected FROM",
		`SELECT * FROM`:                     "expected table name",
		`CREATE TABLE t (id NUMBER)`:        "expected a type",
		`CREATE TABLE t (id INT`:            `expected ")"`,
		`INSERT INTO t VALUES (1, )`:        "expected a value",
		`SELECT * FROM t LIMIT ten`:         "LIMIT needs a whole number",
		`SELECT * FROM t WHERE a = 1 extra`: "after the end",
		`SELECT * FROM t WHERE a = 99999999999999999999`: "too big",
		`UPDATE t SET a 1`: `expected "="`,
	}
	for q, wantMsg := range cases {
		_, err := Parse(q)
		if err == nil || !strings.Contains(err.Error(), wantMsg) {
			t.Errorf("Parse(%q) error = %v, want it to mention %q", q, err, wantMsg)
		}
	}
}
