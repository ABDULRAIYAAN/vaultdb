package sql

import "strings"

// The AST ("abstract syntax tree") is what a query means, with the spelling
// thrown away. "select NAME  from Students" and "SELECT name FROM students"
// both become Select{Columns: [name], Table: students}.

// Statement is any complete SQL command.
type Statement interface{ stmt() }

// Type is a column's data type.
type Type string

const (
	TypeInt   Type = "INT"
	TypeFloat Type = "FLOAT"
	TypeText  Type = "TEXT"
	TypeBool  Type = "BOOL"
)

// ColumnDef is one column in CREATE TABLE.
type ColumnDef struct {
	Name       string
	Type       Type
	PrimaryKey bool
}

// CreateTable: CREATE TABLE students (id INT PRIMARY KEY, name TEXT)
type CreateTable struct {
	Table   string
	Columns []ColumnDef
}

// DropTable: DROP TABLE students
type DropTable struct{ Table string }

// ShowTables: SHOW TABLES
type ShowTables struct{}

// Insert: INSERT INTO students (id, name) VALUES (1, 'Ali'), (2, 'Sara')
type Insert struct {
	Table   string
	Columns []string // empty means "all columns, in table order"
	Rows    [][]Expr
}

// Select: SELECT name, age FROM students WHERE age > 20 ORDER BY age DESC LIMIT 5
//
// The select list can hold plain columns, aggregate functions, or both. With
// aggregates and no GROUP BY, all matching rows become one result row. With
// GROUP BY, rows that share the same values in the GROUP BY columns become
// one result row each:
//
//	SELECT dept, COUNT(*), AVG(gpa) FROM students GROUP BY dept
//
// A plain column next to an aggregate must be listed in GROUP BY, otherwise
// there is no single value to show for it (which row's name goes next to
// SUM(gpa)?). The parser enforces that.
//
// HAVING filters groups the way WHERE filters rows, and can use aggregates:
//
//	SELECT dept, COUNT(*) FROM students GROUP BY dept HAVING COUNT(*) >= 2
//
// DISTINCT drops result rows that are exact copies of an earlier one:
//
//	SELECT DISTINCT dept FROM students
type Select struct {
	Distinct bool         // SELECT DISTINCT: keep only the first of identical result rows
	Items    []SelectItem // empty means *
	Table    string
	Where    Expr       // nil means every row
	GroupBy  []string   // empty means no grouping
	Having   Expr       // nil means keep every group
	OrderBy  string     // a column to sort by, or ""
	OrderAgg *Aggregate // ORDER BY COUNT(*): sort groups by an aggregate instead
	Desc     bool
	Limit    int // -1 means no limit
}

// SelectItem is one entry in a select list: either a plain column or an
// aggregate function (Agg is set and Column is empty).
type SelectItem struct {
	Column string
	Agg    *Aggregate
}

// Name is the item's column heading in a result.
func (it SelectItem) Name() string {
	if it.Agg != nil {
		return it.Agg.Name()
	}
	return it.Column
}

// HasAggregates reports whether any select list entry is an aggregate.
func (s Select) HasAggregates() bool {
	for _, it := range s.Items {
		if it.Agg != nil {
			return true
		}
	}
	return false
}

// Aggregate is one function in a select list that boils many rows down to
// one value: COUNT(*), COUNT(col), SUM(col), AVG(col), MIN(col) or MAX(col).
// With Distinct, each different value is only used once, so
// COUNT(DISTINCT dept) is the number of different departments.
type Aggregate struct {
	Func     string // COUNT, SUM, AVG, MIN or MAX
	Column   string // "" means * (only for COUNT)
	Distinct bool   // COUNT(DISTINCT col): skip values already seen
}

// Name is the aggregate's column heading in a result: "count" for COUNT(*),
// otherwise like "avg(gpa)" or "count(distinct dept)".
func (a Aggregate) Name() string {
	if a.Column == "" {
		return "count"
	}
	if a.Distinct {
		return strings.ToLower(a.Func) + "(distinct " + a.Column + ")"
	}
	return strings.ToLower(a.Func) + "(" + a.Column + ")"
}

// Assignment is one "col = value" inside UPDATE ... SET.
type Assignment struct {
	Column string
	Value  Expr
}

// Update: UPDATE students SET age = 21 WHERE id = 1
type Update struct {
	Table string
	Set   []Assignment
	Where Expr
}

// Delete: DELETE FROM students WHERE id = 1
type Delete struct {
	Table string
	Where Expr
}

func (CreateTable) stmt() {}
func (DropTable) stmt()   {}
func (ShowTables) stmt()  {}
func (Insert) stmt()      {}
func (Select) stmt()      {}
func (Update) stmt()      {}
func (Delete) stmt()      {}

// Expr is anything that produces a value: a literal, a column, or a
// comparison like age > 20 AND name = 'Ali'.
type Expr interface{ expr() }

// Literal is a fixed value written in the query. Value is one of:
// nil (NULL), int64, float64, string or bool.
type Literal struct{ Value any }

// ColumnRef is a column name used inside an expression.
type ColumnRef struct{ Name string }

// Binary is "Left Op Right". Op is one of = != < <= > >= AND OR.
type Binary struct {
	Op          string
	Left, Right Expr
}

// Not is NOT Expr.
type Not struct{ Expr Expr }

// IsNull is "Expr IS NULL" or, with Negate, "Expr IS NOT NULL".
type IsNull struct {
	Expr   Expr
	Negate bool
}

// AggregateRef is an aggregate used inside an expression, like COUNT(*) in
// "HAVING COUNT(*) > 1". The parser only allows it in HAVING; the executor
// replaces it with the group's value before evaluating.
type AggregateRef struct{ Agg Aggregate }

func (Literal) expr()      {}
func (ColumnRef) expr()    {}
func (Binary) expr()       {}
func (Not) expr()          {}
func (IsNull) expr()       {}
func (AggregateRef) expr() {}
