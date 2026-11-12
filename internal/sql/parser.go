package sql

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// Parse turns one SQL statement into its AST.
//
// It is a "recursive descent" parser: one small function per grammar rule,
// each reading the tokens it understands and calling the others for the
// parts inside. The grammar it accepts:
//
//	CREATE TABLE t (col TYPE [PRIMARY KEY], ...)        TYPE: INT FLOAT TEXT BOOL
//	DROP TABLE t
//	SHOW TABLES
//	INSERT INTO t [(col, ...)] VALUES (expr, ...), ...
//	SELECT * | item, ... FROM t [WHERE expr] [GROUP BY col, ...]
//	       [HAVING expr] [ORDER BY col|agg [ASC|DESC]] [LIMIT n]
//	       item: col | agg
//	       agg:  COUNT(*) COUNT(col) SUM(col) AVG(col) MIN(col) MAX(col)
//	       (HAVING's expr may use aggs; other exprs may not)
//	UPDATE t SET col = expr, ... [WHERE expr]
//	DELETE FROM t [WHERE expr]
//
// Names are case-insensitive: Students and students are the same table.
func Parse(query string) (Statement, error) {
	toks, err := Lex(query)
	if err != nil {
		return nil, err
	}
	p := &parser{toks: toks}
	st, err := p.statement()
	if err != nil {
		return nil, err
	}
	p.acceptSymbol(";")
	if p.peek().Kind != TokEOF {
		return nil, p.errorf("unexpected %s after the end of the statement", describe(p.peek()))
	}
	return st, nil
}

type parser struct {
	toks []Token
	pos  int
	// inHaving is set while reading a HAVING condition, the only place an
	// aggregate may appear inside an expression.
	inHaving bool
}

func (p *parser) peek() Token { return p.toks[p.pos] }

func (p *parser) next() Token {
	t := p.toks[p.pos]
	if t.Kind != TokEOF {
		p.pos++
	}
	return t
}

func (p *parser) errorf(format string, args ...any) error {
	return fmt.Errorf("syntax error at position %d: %s", p.peek().Pos, fmt.Sprintf(format, args...))
}

func describe(t Token) string {
	switch t.Kind {
	case TokEOF:
		return "end of query"
	case TokString:
		return fmt.Sprintf("'%s'", t.Text)
	default:
		return fmt.Sprintf("%q", t.Text)
	}
}

func (p *parser) isKeyword(k string) bool {
	t := p.peek()
	return t.Kind == TokKeyword && t.Text == k
}

func (p *parser) acceptKeyword(k string) bool {
	if p.isKeyword(k) {
		p.next()
		return true
	}
	return false
}

func (p *parser) expectKeyword(k string) error {
	if !p.acceptKeyword(k) {
		return p.errorf("expected %s, found %s", k, describe(p.peek()))
	}
	return nil
}

func (p *parser) acceptSymbol(s string) bool {
	t := p.peek()
	if t.Kind == TokSymbol && t.Text == s {
		p.next()
		return true
	}
	return false
}

func (p *parser) expectSymbol(s string) error {
	if !p.acceptSymbol(s) {
		return p.errorf("expected %q, found %s", s, describe(p.peek()))
	}
	return nil
}

func (p *parser) expectIdent(what string) (string, error) {
	t := p.peek()
	if t.Kind != TokIdent {
		return "", p.errorf("expected %s name, found %s", what, describe(t))
	}
	p.next()
	return strings.ToLower(t.Text), nil
}

func (p *parser) statement() (Statement, error) {
	t := p.peek()
	if t.Kind != TokKeyword {
		return nil, p.errorf("a statement must start with SELECT, INSERT, UPDATE, DELETE, CREATE, DROP or SHOW, found %s", describe(t))
	}
	switch t.Text {
	case "SELECT":
		return p.selectStmt()
	case "INSERT":
		return p.insertStmt()
	case "UPDATE":
		return p.updateStmt()
	case "DELETE":
		return p.deleteStmt()
	case "CREATE":
		return p.createStmt()
	case "DROP":
		p.next()
		if err := p.expectKeyword("TABLE"); err != nil {
			return nil, err
		}
		name, err := p.expectIdent("table")
		return DropTable{Table: name}, err
	case "SHOW":
		p.next()
		return ShowTables{}, p.expectKeyword("TABLES")
	}
	return nil, p.errorf("unknown statement %s", t.Text)
}

func (p *parser) createStmt() (Statement, error) {
	p.next() // CREATE
	if err := p.expectKeyword("TABLE"); err != nil {
		return nil, err
	}
	name, err := p.expectIdent("table")
	if err != nil {
		return nil, err
	}
	if err := p.expectSymbol("("); err != nil {
		return nil, err
	}
	st := CreateTable{Table: name}
	for {
		col, err := p.expectIdent("column")
		if err != nil {
			return nil, err
		}
		typ := p.peek()
		if typ.Kind != TokKeyword || (typ.Text != "INT" && typ.Text != "FLOAT" && typ.Text != "TEXT" && typ.Text != "BOOL") {
			return nil, p.errorf("expected a type (INT, FLOAT, TEXT or BOOL) for column %s, found %s", col, describe(typ))
		}
		p.next()
		def := ColumnDef{Name: col, Type: Type(typ.Text)}
		if p.acceptKeyword("PRIMARY") {
			if err := p.expectKeyword("KEY"); err != nil {
				return nil, err
			}
			def.PrimaryKey = true
		}
		st.Columns = append(st.Columns, def)
		if !p.acceptSymbol(",") {
			break
		}
	}
	return st, p.expectSymbol(")")
}

func (p *parser) insertStmt() (Statement, error) {
	p.next() // INSERT
	if err := p.expectKeyword("INTO"); err != nil {
		return nil, err
	}
	name, err := p.expectIdent("table")
	if err != nil {
		return nil, err
	}
	st := Insert{Table: name}
	if p.acceptSymbol("(") {
		if st.Columns, err = p.identList("column"); err != nil {
			return nil, err
		}
		if err := p.expectSymbol(")"); err != nil {
			return nil, err
		}
	}
	if err := p.expectKeyword("VALUES"); err != nil {
		return nil, err
	}
	for {
		if err := p.expectSymbol("("); err != nil {
			return nil, err
		}
		var row []Expr
		for {
			e, err := p.expr()
			if err != nil {
				return nil, err
			}
			row = append(row, e)
			if !p.acceptSymbol(",") {
				break
			}
		}
		if err := p.expectSymbol(")"); err != nil {
			return nil, err
		}
		st.Rows = append(st.Rows, row)
		if !p.acceptSymbol(",") {
			return st, nil
		}
	}
}

func (p *parser) selectStmt() (Statement, error) {
	p.next() // SELECT
	st := Select{Limit: -1}
	if !p.acceptSymbol("*") {
		if err := p.selectList(&st); err != nil {
			return nil, err
		}
	}
	if err := p.expectKeyword("FROM"); err != nil {
		return nil, err
	}
	var err error
	if st.Table, err = p.expectIdent("table"); err != nil {
		return nil, err
	}
	if st.Where, err = p.optionalWhere(); err != nil {
		return nil, err
	}
	if p.acceptKeyword("GROUP") {
		if err := p.expectKeyword("BY"); err != nil {
			return nil, err
		}
		if st.GroupBy, err = p.identList("column"); err != nil {
			return nil, err
		}
	}
	if p.acceptKeyword("HAVING") {
		p.inHaving = true
		st.Having, err = p.expr()
		p.inHaving = false
		if err != nil {
			return nil, err
		}
	}
	if err := checkGrouping(st); err != nil {
		return nil, err
	}
	if p.acceptKeyword("ORDER") {
		if err := p.expectKeyword("BY"); err != nil {
			return nil, err
		}
		if p.atAggregate() {
			agg, err := p.aggregate()
			if err != nil {
				return nil, err
			}
			st.OrderAgg = &agg
		} else if st.OrderBy, err = p.expectIdent("column"); err != nil {
			return nil, err
		}
		if p.acceptKeyword("DESC") {
			st.Desc = true
		} else {
			p.acceptKeyword("ASC")
		}
	}
	if p.acceptKeyword("LIMIT") {
		t := p.peek()
		n, convErr := strconv.Atoi(t.Text)
		if t.Kind != TokNumber || convErr != nil || n < 0 {
			return nil, p.errorf("LIMIT needs a whole number, found %s", describe(t))
		}
		p.next()
		st.Limit = n
	}
	return st, nil
}

// aggregateFuncs are the functions allowed in a select list. Only COUNT is a
// keyword; SUM, AVG, MIN and MAX are recognised by the "(" after them, so
// they can still be used as column names (a column called max is fine).
var aggregateFuncs = map[string]bool{"COUNT": true, "SUM": true, "AVG": true, "MIN": true, "MAX": true}

func (p *parser) atAggregate() bool {
	t := p.peek()
	if t.Kind == TokKeyword && t.Text == "COUNT" {
		return true
	}
	if t.Kind != TokIdent || !aggregateFuncs[strings.ToUpper(t.Text)] {
		return false
	}
	after := p.toks[p.pos+1] // safe: the last token is always TokEOF
	return after.Kind == TokSymbol && after.Text == "("
}

// selectList reads "name, gpa", "COUNT(*), AVG(gpa)" or a mix of both into st.
func (p *parser) selectList(st *Select) error {
	for {
		if p.atAggregate() {
			agg, err := p.aggregate()
			if err != nil {
				return err
			}
			st.Items = append(st.Items, SelectItem{Agg: &agg})
		} else {
			col, err := p.expectIdent("column")
			if err != nil {
				return err
			}
			st.Items = append(st.Items, SelectItem{Column: col})
		}
		if !p.acceptSymbol(",") {
			return nil
		}
	}
}

// aggregate reads one function call such as COUNT(*) or AVG(gpa).
func (p *parser) aggregate() (Aggregate, error) {
	agg := Aggregate{Func: strings.ToUpper(p.next().Text)}
	if err := p.expectSymbol("("); err != nil {
		return agg, err
	}
	if p.acceptSymbol("*") {
		if agg.Func != "COUNT" {
			return agg, p.errorf("%s(*) is not allowed; only COUNT can take *", agg.Func)
		}
	} else {
		col, err := p.expectIdent("column")
		if err != nil {
			return agg, err
		}
		agg.Column = col
	}
	return agg, p.expectSymbol(")")
}

// checkGrouping enforces the one GROUP BY rule that needs no table schema:
// a plain column in the select list of a grouped or aggregated query must be
// one of the GROUP BY columns, because every row of a group shares its value.
func checkGrouping(st Select) error {
	if st.Having != nil && len(st.GroupBy) == 0 && !st.HasAggregates() {
		return errors.New("HAVING needs GROUP BY or aggregate functions in the select list; use WHERE to filter rows")
	}
	if err := checkHavingColumns(st.Having, st.GroupBy); err != nil {
		return err
	}
	if len(st.GroupBy) == 0 {
		if st.HasAggregates() {
			for _, it := range st.Items {
				if it.Agg == nil {
					return fmt.Errorf("column %s must be listed in GROUP BY to be selected next to an aggregate function", it.Column)
				}
			}
		}
		return nil
	}
	if len(st.Items) == 0 {
		return errors.New("SELECT * cannot be used with GROUP BY; list the GROUP BY columns and aggregates instead")
	}
	grouped := map[string]bool{}
	for _, g := range st.GroupBy {
		if grouped[g] {
			return fmt.Errorf("column %s is listed twice in GROUP BY", g)
		}
		grouped[g] = true
	}
	for _, it := range st.Items {
		if it.Agg == nil && !grouped[it.Column] {
			return fmt.Errorf("column %s must be listed in GROUP BY or used inside an aggregate function", it.Column)
		}
	}
	return nil
}

// checkHavingColumns makes sure every plain column in a HAVING condition is
// a GROUP BY column. Anything else must sit inside an aggregate, for the same
// reason as in the select list: a group has no single value for it.
func checkHavingColumns(ex Expr, groupBy []string) error {
	switch x := ex.(type) {
	case ColumnRef:
		for _, g := range groupBy {
			if g == x.Name {
				return nil
			}
		}
		return fmt.Errorf("column %s in HAVING must be listed in GROUP BY or used inside an aggregate function", x.Name)
	case Binary:
		if err := checkHavingColumns(x.Left, groupBy); err != nil {
			return err
		}
		return checkHavingColumns(x.Right, groupBy)
	case Not:
		return checkHavingColumns(x.Expr, groupBy)
	case IsNull:
		return checkHavingColumns(x.Expr, groupBy)
	}
	return nil
}

func (p *parser) updateStmt() (Statement, error) {
	p.next() // UPDATE
	name, err := p.expectIdent("table")
	if err != nil {
		return nil, err
	}
	if err := p.expectKeyword("SET"); err != nil {
		return nil, err
	}
	st := Update{Table: name}
	for {
		col, err := p.expectIdent("column")
		if err != nil {
			return nil, err
		}
		if err := p.expectSymbol("="); err != nil {
			return nil, err
		}
		val, err := p.expr()
		if err != nil {
			return nil, err
		}
		st.Set = append(st.Set, Assignment{Column: col, Value: val})
		if !p.acceptSymbol(",") {
			break
		}
	}
	st.Where, err = p.optionalWhere()
	return st, err
}

func (p *parser) deleteStmt() (Statement, error) {
	p.next() // DELETE
	if err := p.expectKeyword("FROM"); err != nil {
		return nil, err
	}
	name, err := p.expectIdent("table")
	if err != nil {
		return nil, err
	}
	where, err := p.optionalWhere()
	return Delete{Table: name, Where: where}, err
}

func (p *parser) optionalWhere() (Expr, error) {
	if !p.acceptKeyword("WHERE") {
		return nil, nil
	}
	return p.expr()
}

func (p *parser) identList(what string) ([]string, error) {
	var out []string
	for {
		name, err := p.expectIdent(what)
		if err != nil {
			return nil, err
		}
		out = append(out, name)
		if !p.acceptSymbol(",") {
			return out, nil
		}
	}
}

// Expressions, from loosest to tightest binding:
//
//	OR  <  AND  <  NOT  <  comparison (= != <> < <= > >=, IS [NOT] NULL)  <  value
//
// So "a = 1 OR b = 2 AND c = 3" means "a = 1 OR (b = 2 AND c = 3)", exactly
// like in real SQL. Each level calls the next tighter one for its operands.

func (p *parser) expr() (Expr, error) { return p.orExpr() }

func (p *parser) orExpr() (Expr, error) {
	left, err := p.andExpr()
	for err == nil && p.acceptKeyword("OR") {
		var right Expr
		if right, err = p.andExpr(); err == nil {
			left = Binary{Op: "OR", Left: left, Right: right}
		}
	}
	return left, err
}

func (p *parser) andExpr() (Expr, error) {
	left, err := p.notExpr()
	for err == nil && p.acceptKeyword("AND") {
		var right Expr
		if right, err = p.notExpr(); err == nil {
			left = Binary{Op: "AND", Left: left, Right: right}
		}
	}
	return left, err
}

func (p *parser) notExpr() (Expr, error) {
	if p.acceptKeyword("NOT") {
		e, err := p.notExpr()
		return Not{Expr: e}, err
	}
	return p.comparison()
}

func (p *parser) comparison() (Expr, error) {
	left, err := p.value()
	if err != nil {
		return nil, err
	}
	if p.acceptKeyword("IS") {
		negate := p.acceptKeyword("NOT")
		if err := p.expectKeyword("NULL"); err != nil {
			return nil, err
		}
		return IsNull{Expr: left, Negate: negate}, nil
	}
	t := p.peek()
	if t.Kind == TokSymbol {
		switch t.Text {
		case "=", "!=", "<>", "<", "<=", ">", ">=":
			p.next()
			right, err := p.value()
			if err != nil {
				return nil, err
			}
			op := t.Text
			if op == "<>" {
				op = "!="
			}
			return Binary{Op: op, Left: left, Right: right}, nil
		}
	}
	return left, nil
}

func (p *parser) value() (Expr, error) {
	if p.atAggregate() {
		if !p.inHaving {
			return nil, p.errorf("aggregate functions such as %s are not allowed here; use HAVING to filter groups", strings.ToUpper(p.peek().Text))
		}
		agg, err := p.aggregate()
		if err != nil {
			return nil, err
		}
		return AggregateRef{Agg: agg}, nil
	}
	t := p.peek()
	switch {
	case t.Kind == TokNumber:
		p.next()
		return parseNumber(t.Text, false, p)
	case t.Kind == TokSymbol && t.Text == "-":
		p.next()
		n := p.peek()
		if n.Kind != TokNumber {
			return nil, p.errorf("expected a number after '-', found %s", describe(n))
		}
		p.next()
		return parseNumber(n.Text, true, p)
	case t.Kind == TokString:
		p.next()
		return Literal{Value: t.Text}, nil
	case t.Kind == TokIdent:
		p.next()
		return ColumnRef{Name: strings.ToLower(t.Text)}, nil
	case t.Kind == TokKeyword && (t.Text == "TRUE" || t.Text == "FALSE"):
		p.next()
		return Literal{Value: t.Text == "TRUE"}, nil
	case t.Kind == TokKeyword && t.Text == "NULL":
		p.next()
		return Literal{Value: nil}, nil
	case t.Kind == TokSymbol && t.Text == "(":
		p.next()
		e, err := p.expr()
		if err != nil {
			return nil, err
		}
		return e, p.expectSymbol(")")
	}
	return nil, p.errorf("expected a value, found %s", describe(t))
}

func parseNumber(text string, negative bool, p *parser) (Expr, error) {
	if negative {
		text = "-" + text
	}
	if strings.Contains(text, ".") {
		f, err := strconv.ParseFloat(text, 64)
		if err != nil {
			return nil, p.errorf("bad number %s", text)
		}
		return Literal{Value: f}, nil
	}
	n, err := strconv.ParseInt(text, 10, 64)
	if err != nil {
		return nil, p.errorf("number %s is too big", text)
	}
	return Literal{Value: n}, nil
}
