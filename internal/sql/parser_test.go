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

// items builds a select list: "name" is a plain column, "AVG(gpa)" or
// "COUNT(*)" an aggregate.
func items(specs ...string) []SelectItem {
	var out []SelectItem
	for _, sp := range specs {
		open := strings.IndexByte(sp, '(')
		if open < 0 {
			out = append(out, SelectItem{Column: sp})
			continue
		}
		a := Aggregate{Func: sp[:open], Column: sp[open+1 : len(sp)-1]}
		if a.Column == "*" {
			a.Column = ""
		}
		out = append(out, SelectItem{Agg: &a})
	}
	return out
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
			Select{Items: items("COUNT(*)"), Table: "students", Where: bin("=", col("active"), lit(true)), Limit: -1}},
		{`select count(name), Sum(gpa), avg(gpa), MIN(id), max(id) FROM students`,
			Select{Items: items("COUNT(name)", "SUM(gpa)", "AVG(gpa)", "MIN(id)", "MAX(id)"),
				Table: "students", Limit: -1}},
		// Without a "(" after it, max is just a column name.
		{`SELECT min, max FROM t`, Select{Items: items("min", "max"), Table: "t", Limit: -1}},
		{`SELECT dept, COUNT(*), AVG(gpa) FROM students WHERE active = TRUE GROUP BY dept`,
			Select{Items: items("dept", "COUNT(*)", "AVG(gpa)"), Table: "students",
				Where: bin("=", col("active"), lit(true)), GroupBy: []string{"dept"}, Limit: -1}},
		{`SELECT COUNT(*), dept, year FROM t GROUP BY dept, year ORDER BY COUNT(*) DESC LIMIT 3`,
			Select{Items: items("COUNT(*)", "dept", "year"), Table: "t", GroupBy: []string{"dept", "year"},
				OrderAgg: &Aggregate{Func: "COUNT"}, Desc: true, Limit: 3}},
		{`SELECT dept, COUNT(*) FROM t GROUP BY dept HAVING COUNT(*) > 1 AND dept != 'hr' ORDER BY COUNT(*) DESC`,
			Select{Items: items("dept", "COUNT(*)"), Table: "t", GroupBy: []string{"dept"},
				Having: bin("AND", bin(">", AggregateRef{Agg: Aggregate{Func: "COUNT"}}, lit(int64(1))),
					bin("!=", col("dept"), lit("hr"))),
				OrderAgg: &Aggregate{Func: "COUNT"}, Desc: true, Limit: -1}},
		// HAVING may use an aggregate that is not selected, and works
		// without GROUP BY when the select list has aggregates.
		{`SELECT COUNT(*) FROM t HAVING AVG(gpa) >= 3.5`,
			Select{Items: items("COUNT(*)"), Table: "t",
				Having: bin(">=", AggregateRef{Agg: Aggregate{Func: "AVG", Column: "gpa"}}, lit(3.5)), Limit: -1}},
		{`SELECT dept FROM t GROUP BY dept ORDER BY dept`,
			Select{Items: items("dept"), Table: "t", GroupBy: []string{"dept"}, OrderBy: "dept", Limit: -1}},
		{`SELECT name, gpa FROM students WHERE gpa > 3 ORDER BY gpa DESC LIMIT 5`,
			Select{Items: items("name", "gpa"), Table: "students", Where: bin(">", col("gpa"), lit(int64(3))),
				OrderBy: "gpa", Desc: true, Limit: 5}},
		{`SELECT * FROM t ORDER BY a ASC`, Select{Table: "t", OrderBy: "a", Limit: -1}},
		{`SELECT DISTINCT dept, year FROM t ORDER BY year`,
			Select{Distinct: true, Items: items("dept", "year"), Table: "t", OrderBy: "year", Limit: -1}},
		{`select distinct * from t order by a`, Select{Distinct: true, Table: "t", OrderBy: "a", Limit: -1}},
		{`SELECT dept, count(distinct year) FROM t GROUP BY dept HAVING COUNT(DISTINCT year) > 1`,
			Select{Items: []SelectItem{{Column: "dept"}, {Agg: &Aggregate{Func: "COUNT", Column: "year", Distinct: true}}},
				Table: "t", GroupBy: []string{"dept"},
				Having: bin(">", AggregateRef{Agg: Aggregate{Func: "COUNT", Column: "year", Distinct: true}}, lit(int64(1))),
				Limit:  -1}},
		// SELECT DISTINCT can be sorted by an aggregate it shows.
		{`SELECT DISTINCT COUNT(*) FROM t GROUP BY dept ORDER BY COUNT(*)`,
			Select{Distinct: true, Items: items("COUNT(*)"), Table: "t", GroupBy: []string{"dept"},
				OrderAgg: &Aggregate{Func: "COUNT"}, Limit: -1}},
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
		`UPDATE t SET a 1`:                                                               `expected "="`,
		`SELECT SUM(*) FROM t`:                                                           "only COUNT can take *",
		`SELECT name, COUNT(*) FROM t`:                                                   "name must be listed in GROUP BY",
		`SELECT AVG(gpa), name FROM t`:                                                   "name must be listed in GROUP BY",
		`SELECT name, COUNT(*) FROM t GROUP BY dept`:                                     "name must be listed in GROUP BY or used inside an aggregate",
		`SELECT * FROM t GROUP BY dept`:                                                  "SELECT * cannot be used with GROUP BY",
		`SELECT dept FROM t GROUP BY dept, dept`:                                         "listed twice in GROUP BY",
		`SELECT dept FROM t GROUP dept`:                                                  "expected BY",
		`SELECT dept FROM t GROUP BY`:                                                    "expected column name",
		`SELECT dept FROM t ORDER BY SUM(*)`:                                             "only COUNT can take *",
		`SELECT MAX(gpa FROM t`:                                                          `expected ")"`,
		`SELECT COUNT() FROM t`:                                                          "expected column name",
		`SELECT * FROM t WHERE COUNT(*) > 1`:                                             "not allowed here; use HAVING",
		`SELECT * FROM t WHERE max(a) > 1`:                                               "not allowed here; use HAVING",
		`UPDATE t SET a = SUM(b)`:                                                        "not allowed here",
		`SELECT * FROM t HAVING a > 1`:                                                   "HAVING needs GROUP BY or aggregate functions",
		`SELECT dept FROM t GROUP BY dept HAVING gpa > 3`:                                "gpa in HAVING must be listed in GROUP BY",
		`SELECT COUNT(*) FROM t HAVING dept = 'cs'`:                                      "dept in HAVING must be listed in GROUP BY",
		`SELECT dept FROM t GROUP BY dept HAVING`:                                        "expected a value",
		`SELECT COUNT(DISTINCT *) FROM t`:                                                "COUNT(DISTINCT *) is not allowed",
		`SELECT DISTINCT FROM t`:                                                         "expected column name",
		`SELECT DISTINCT name FROM t ORDER BY gpa`:                                       "ORDER BY gpa must be in the select list of a SELECT DISTINCT",
		`SELECT DISTINCT dept FROM t GROUP BY dept ORDER BY COUNT(*)`:                    "ORDER BY count must be in the select list",
		`SELECT DISTINCT COUNT(year) FROM t GROUP BY dept ORDER BY COUNT(DISTINCT year)`: "ORDER BY count(distinct year) must be in the select list",
	}
	for q, wantMsg := range cases {
		_, err := Parse(q)
		if err == nil || !strings.Contains(err.Error(), wantMsg) {
			t.Errorf("Parse(%q) error = %v, want it to mention %q", q, err, wantMsg)
		}
	}
}
