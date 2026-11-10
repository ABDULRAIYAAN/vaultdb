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
// A select list is either plain columns (or *) or aggregate functions, never
// both: without GROUP BY there is no way to say which row's name should sit
// next to SUM(gpa).
type Select struct {
	Columns    []string    // empty (with no Aggregates) means *
	Aggregates []Aggregate // SELECT COUNT(*), AVG(gpa): one result row
	Table      string
	Where      Expr // nil means every row
	OrderBy    string
	Desc       bool
	Limit      int // -1 means no limit
}

// Aggregate is one function in a select list that boils many rows down to
// one value: COUNT(*), COUNT(col), SUM(col), AVG(col), MIN(col) or MAX(col).
type Aggregate struct {
	Func   string // COUNT, SUM, AVG, MIN or MAX
	Column string // "" means * (only for COUNT)
}

// Name is the aggregate's column heading in a result: "count" for COUNT(*),
// otherwise like "avg(gpa)".
func (a Aggregate) Name() string {
	if a.Column == "" {
		return "count"
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

func (Literal) expr()   {}
func (ColumnRef) expr() {}
func (Binary) expr()    {}
func (Not) expr()       {}
func (IsNull) expr()    {}
