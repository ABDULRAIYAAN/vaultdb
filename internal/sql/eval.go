package sql

import (
	"fmt"
	"strings"
)

// eval works out the value of an expression for one row. Values are nil
// (NULL), int64, float64, string or bool. s and row may be nil when there is
// no row (for example the values in INSERT), and then column names are not
// allowed.
//
// NULL means "unknown". Comparing anything with NULL gives NULL, and WHERE
// only keeps rows where the condition is exactly true, so "WHERE age > 20"
// skips rows whose age is NULL. Use IS NULL to find those rows.
func eval(ex Expr, s *schema, row []any) (any, error) {
	switch x := ex.(type) {
	case Literal:
		return x.Value, nil

	case ColumnRef:
		if s == nil {
			return nil, fmt.Errorf("column %s cannot be used here", x.Name)
		}
		i := s.index(x.Name)
		if i < 0 {
			return nil, fmt.Errorf("unknown column %s", x.Name)
		}
		return row[i], nil

	case Not:
		v, err := eval(x.Expr, s, row)
		if err != nil || v == nil {
			return nil, err
		}
		b, ok := v.(bool)
		if !ok {
			return nil, fmt.Errorf("NOT needs TRUE or FALSE, got %s", typeName(v))
		}
		return !b, nil

	case IsNull:
		v, err := eval(x.Expr, s, row)
		if err != nil {
			return nil, err
		}
		return (v == nil) != x.Negate, nil

	case Binary:
		l, err := eval(x.Left, s, row)
		if err != nil {
			return nil, err
		}
		r, err := eval(x.Right, s, row)
		if err != nil {
			return nil, err
		}
		switch x.Op {
		case "AND", "OR":
			return logic(x.Op, l, r)
		}
		if l == nil || r == nil {
			return nil, nil
		}
		c, err := compare(l, r)
		if err != nil {
			return nil, err
		}
		switch x.Op {
		case "=":
			return c == 0, nil
		case "!=":
			return c != 0, nil
		case "<":
			return c < 0, nil
		case "<=":
			return c <= 0, nil
		case ">":
			return c > 0, nil
		case ">=":
			return c >= 0, nil
		}
		return nil, fmt.Errorf("unknown operator %s", x.Op)

	case In:
		v, err := eval(x.Expr, s, row)
		if err != nil {
			return nil, err
		}
		return evalIn(v, x, s, row)

	case Like:
		v, err := eval(x.Expr, s, row)
		if err != nil {
			return nil, err
		}
		pat, err := eval(x.Pattern, s, row)
		if err != nil || v == nil || pat == nil {
			return nil, err
		}
		text, ok1 := v.(string)
		ps, ok2 := pat.(string)
		if !ok1 || !ok2 {
			return nil, fmt.Errorf("LIKE needs TEXT on both sides, got %s LIKE %s", typeName(v), typeName(pat))
		}
		m, err := likeMatch(text, ps)
		if err != nil {
			return nil, err
		}
		return m != x.Negate, nil

	case Between:
		return evalBetween(x, s, row)

	case AggregateRef:
		// groupRows swaps these for their values before calling eval, so
		// reaching one here means an aggregate slipped in somewhere else.
		return nil, fmt.Errorf("aggregate function %s can only be used in HAVING", x.Agg.Name())
	}
	return nil, fmt.Errorf("cannot evaluate %T", ex)
}

// logic joins two conditions with AND or OR using SQL's three-valued logic,
// where NULL means "unknown":
//
//	AND: FALSE if either side is FALSE, else NULL if either is NULL, else TRUE
//	OR:  TRUE if either side is TRUE, else NULL if either is NULL, else FALSE
//
// So "FALSE AND NULL" is FALSE (the answer is false whatever the unknown is),
// but "TRUE AND NULL" is NULL. This matters under NOT: with y NULL,
// NOT (x > 1 AND y > 1) must be NULL, not TRUE, or WHERE would keep the row.
func logic(op string, l, r any) (any, error) {
	for _, v := range []any{l, r} {
		if _, ok := v.(bool); v != nil && !ok {
			return nil, fmt.Errorf("%s needs TRUE or FALSE, got %s", op, typeName(v))
		}
	}
	decides := op == "OR" // the value that settles the answer on its own
	if l == decides || r == decides {
		return decides, nil
	}
	if l == nil || r == nil {
		return nil, nil
	}
	return !decides, nil
}

// evalIn works out "v IN (list)" once v is known. Each list value is compared
// with v exactly like "=" would. The answer is TRUE on the first match. With
// no match it is FALSE, unless v or some list value was NULL: then nobody
// knows whether they would have matched, so the answer is NULL. That is why
// "x NOT IN (1, NULL)" never keeps a row, the classic SQL surprise.
func evalIn(v any, x In, s *schema, row []any) (any, error) {
	if v == nil {
		return nil, nil
	}
	sawNull := false
	for _, item := range x.List {
		w, err := eval(item, s, row)
		if err != nil {
			return nil, err
		}
		if w == nil {
			sawNull = true
			continue
		}
		c, err := compare(v, w)
		if err != nil {
			return nil, err
		}
		if c == 0 {
			return !x.Negate, nil
		}
	}
	if sawNull {
		return nil, nil
	}
	return x.Negate, nil
}

// evalBetween works out "v BETWEEN low AND high" as the two comparisons
// v >= low and v <= high, joined with SQL's three-valued AND: FALSE if either
// side is FALSE, otherwise NULL if either side is NULL, otherwise TRUE. So a
// NULL end doesn't always give NULL: 9 BETWEEN NULL AND 5 is FALSE, because
// 9 <= 5 already fails whatever the missing end would have been. NOT BETWEEN
// flips TRUE and FALSE and leaves NULL alone.
//
// All three values are worked out first, so a type mistake such as
// 5 BETWEEN 'a' AND 10 is reported even when the other end decides alone.
func evalBetween(x Between, s *schema, row []any) (any, error) {
	var vals [3]any
	for i, e := range []Expr{x.Expr, x.Low, x.High} {
		v, err := eval(e, s, row)
		if err != nil {
			return nil, err
		}
		vals[i] = v
	}
	v, low, high := vals[0], vals[1], vals[2]
	side := func(a, b any) (any, error) { // a <= b, or NULL
		if a == nil || b == nil {
			return nil, nil
		}
		c, err := compare(a, b)
		if err != nil {
			return nil, err
		}
		return c <= 0, nil
	}
	aboveLow, err := side(low, v)
	if err != nil {
		return nil, err
	}
	belowHigh, err := side(v, high)
	if err != nil {
		return nil, err
	}
	switch {
	case aboveLow == false || belowHigh == false:
		return x.Negate, nil
	case aboveLow == nil || belowHigh == nil:
		return nil, nil
	}
	return !x.Negate, nil
}

// likeMatch reports whether text matches a LIKE pattern: % is any run of
// characters, _ is one character, and \ makes the next character literal.
// It works on characters (runes), so _ matches one 'é', not half of it.
//
// The matcher is the classic two-pointer one used for shell globs: walk both
// strings, and when a % is found remember where it was. On a mismatch, go
// back to just after the last %, let it swallow one more character of text,
// and try again. Only the most recent % ever needs revisiting, so this takes
// at most len(text) * len(pattern) steps and never blows up the way naive
// recursion does on patterns like '%a%a%a%a%b'.
func likeMatch(text, pattern string) (bool, error) {
	// Compile the pattern into tokens first so escapes are handled once.
	type tok struct {
		r    rune
		kind byte // 'c' literal character, '_' one character, '%' any run
	}
	var pat []tok
	pr := []rune(pattern)
	for i := 0; i < len(pr); i++ {
		switch pr[i] {
		case '\\':
			if i+1 == len(pr) {
				return false, fmt.Errorf("LIKE pattern %s ends with a lone backslash; use \\\\ to match a backslash", Format(pattern))
			}
			i++
			pat = append(pat, tok{pr[i], 'c'})
		case '%':
			if len(pat) == 0 || pat[len(pat)-1].kind != '%' { // %% is the same as %
				pat = append(pat, tok{kind: '%'})
			}
		case '_':
			pat = append(pat, tok{kind: '_'})
		default:
			pat = append(pat, tok{pr[i], 'c'})
		}
	}

	tr := []rune(text)
	ti, pi := 0, 0
	star, starText := -1, 0 // last % seen in the pattern, and where text was then
	for ti < len(tr) {
		switch {
		case pi < len(pat) && pat[pi].kind == '%':
			star, starText = pi, ti
			pi++
		case pi < len(pat) && (pat[pi].kind == '_' || pat[pi].r == tr[ti]):
			ti++
			pi++
		case star >= 0:
			starText++
			ti, pi = starText, star+1
		default:
			return false, nil
		}
	}
	// Text used up: whatever is left of the pattern must be %s.
	for pi < len(pat) && pat[pi].kind == '%' {
		pi++
	}
	return pi == len(pat), nil
}

// compare returns -1, 0 or 1. Whole numbers and decimals can be compared with
// each other; otherwise both sides must be the same type.
func compare(a, b any) (int, error) {
	if fa, ok := toFloat(a); ok {
		if fb, ok := toFloat(b); ok {
			// Compare int64s exactly; float64 can't hold every big int64.
			ia, aInt := a.(int64)
			ib, bInt := b.(int64)
			if aInt && bInt {
				return cmp3(ia < ib, ia > ib), nil
			}
			return cmp3(fa < fb, fa > fb), nil
		}
	}
	switch av := a.(type) {
	case string:
		if bv, ok := b.(string); ok {
			return strings.Compare(av, bv), nil
		}
	case bool:
		if bv, ok := b.(bool); ok {
			return cmp3(!av && bv, av && !bv), nil
		}
	}
	return 0, fmt.Errorf("cannot compare %s with %s", typeName(a), typeName(b))
}

func cmp3(less, greater bool) int {
	switch {
	case less:
		return -1
	case greater:
		return 1
	}
	return 0
}

// compareForSort orders values for ORDER BY, with NULLs first.
func compareForSort(a, b any) int {
	switch {
	case a == nil && b == nil:
		return 0
	case a == nil:
		return -1
	case b == nil:
		return 1
	}
	c, _ := compare(a, b)
	return c
}

func toFloat(v any) (float64, bool) {
	switch n := v.(type) {
	case int64:
		return float64(n), true
	case float64:
		return n, true
	}
	return 0, false
}

// coerce checks that v fits the column's type, converting whole numbers to
// decimals for FLOAT columns.
func coerce(v any, c column) (any, error) {
	if v == nil {
		return nil, nil
	}
	switch c.Type {
	case TypeInt:
		if n, ok := v.(int64); ok {
			return n, nil
		}
	case TypeFloat:
		if f, ok := toFloat(v); ok {
			return f, nil
		}
	case TypeText:
		if s, ok := v.(string); ok {
			return s, nil
		}
	case TypeBool:
		if b, ok := v.(bool); ok {
			return b, nil
		}
	}
	return nil, fmt.Errorf("column %s is %s, but got %s %s", c.Name, c.Type, typeName(v), Format(v))
}

func typeName(v any) string {
	switch v.(type) {
	case nil:
		return "NULL"
	case int64:
		return "INT"
	case float64:
		return "FLOAT"
	case string:
		return "TEXT"
	case bool:
		return "BOOL"
	}
	return fmt.Sprintf("%T", v)
}

// Format shows a value the way SQL would write it.
func Format(v any) string {
	switch x := v.(type) {
	case nil:
		return "NULL"
	case string:
		return "'" + strings.ReplaceAll(x, "'", "''") + "'"
	case bool:
		if x {
			return "TRUE"
		}
		return "FALSE"
	case float64:
		s := fmt.Sprintf("%g", x)
		if !strings.ContainsAny(s, ".eE") {
			s += ".0"
		}
		return s
	}
	return fmt.Sprint(v)
}

// FormatTable draws a result as a text table:
//
//	+----+-------+
//	| id | name  |
//	+----+-------+
//	| 1  | 'Ali' |
//	+----+-------+
//	(1 row)
func FormatTable(r Result) string {
	if r.Columns == nil {
		return r.Message
	}
	cells := make([][]string, len(r.Rows))
	widths := make([]int, len(r.Columns))
	for i, c := range r.Columns {
		widths[i] = len(c)
	}
	for n, row := range r.Rows {
		cells[n] = make([]string, len(row))
		for i, v := range row {
			cells[n][i] = Format(v)
			if len([]rune(cells[n][i])) > widths[i] {
				widths[i] = len([]rune(cells[n][i]))
			}
		}
	}
	var b strings.Builder
	line := func() {
		for _, w := range widths {
			b.WriteString("+" + strings.Repeat("-", w+2))
		}
		b.WriteString("+\n")
	}
	rowOut := func(vals []string) {
		for i, v := range vals {
			b.WriteString("| " + v + strings.Repeat(" ", widths[i]-len([]rune(v))) + " ")
		}
		b.WriteString("|\n")
	}
	line()
	rowOut(r.Columns)
	line()
	for _, c := range cells {
		rowOut(c)
	}
	if len(cells) > 0 {
		line()
	}
	fmt.Fprintf(&b, "(%d %s)", len(r.Rows), plural(len(r.Rows), "row"))
	return b.String()
}
