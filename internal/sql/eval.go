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
		case "AND":
			return l == true && r == true, nil
		case "OR":
			return l == true || r == true, nil
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

	case AggregateRef:
		// groupRows swaps these for their values before calling eval, so
		// reaching one here means an aggregate slipped in somewhere else.
		return nil, fmt.Errorf("aggregate function %s can only be used in HAVING", x.Agg.Name())
	}
	return nil, fmt.Errorf("cannot evaluate %T", ex)
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
