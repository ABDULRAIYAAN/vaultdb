package sql

import (
	"fmt"
	"sort"
	"strings"
)

// The query planner
//
// Before reading anything, the executor asks planAccess which rows of the
// table could possibly pass WHERE, and reads only those. The rest of WHERE is
// still checked on every row it reads, so a plan only has to be safe: it may
// read rows that WHERE then throws away, but it must never skip a row WHERE
// would keep.
//
// WHERE keeps a row only when it is TRUE, and "a AND b" is TRUE only when both
// sides are, so every condition joined by a top-level AND has to hold on its
// own. The planner splits WHERE at those ANDs and looks at each piece:
//
//	pk = 5, pk IN (1, 5)          -> these exact keys
//	pk > 5, pk >= 5, 5 < pk, ...  -> keys above or below a bound
//	pk BETWEEN 1 AND 9            -> both bounds at once
//	pk = NULL, pk > NULL, ...     -> nothing (a comparison with NULL is never TRUE)
//	anything else                 -> no help; it is only checked per row
//
// and then intersects them: the tightest lower bound, the tightest upper
// bound, and only the listed keys that fall between them. So
// "WHERE id >= 100 AND id < 200 AND name LIKE 'A%'" walks the hundred keys
// from 100 to 199 and checks the LIKE on those, instead of reading the table.
//
// Bounds work because key order is value order. A primary key is always INT
// or TEXT: INT keys are fixed-width hex with the sign bit flipped, so
// -1 < 0 < 10 as keys too, and TEXT keys are the text itself, compared byte
// by byte like TEXT values are. A piece the planner can't turn into keys
// exactly, like "id > 2.5" on an INT key or a value of the wrong type, is
// left to the per-row check, so it still gives the right answer or error.
//
// Indexes (see index.go) work the same way: an index is just another sorted
// run of keys, one per row, ordered by the indexed column's value. So the
// planner looks at the conditions once for the primary key and once for each
// index, and picks the best result: reading nothing beats exact values,
// exact values beat a range, and a range beats reading the whole table. On a
// tie the primary key wins, since it reaches a row in one GET where an index
// needs a walk over its entries first, and between indexes the older one
// wins. That's a simple rule with no statistics behind it, so it can pick
// "dept = 'eng'" over a narrower "year > 2024", but it never picks a plan
// that's worse than reading everything.
//
// EXPLAIN shows the plan without running the statement.

type accessKind int

const (
	fullScan  accessKind = iota // read every row of the table
	keyLookup                   // GET each key in keys
	rangeScan                   // one B-tree walk over [from, to)
	noRows                      // WHERE can never be TRUE; read nothing
)

// access is how a statement reaches its rows.
type access struct {
	kind     accessKind
	keys     []string // keyLookup: the first key for each value, sorted, no repeats
	ends     []string // keyLookup: the first key past each value's keys
	vals     []any    // keyLookup: the values, same order as keys
	low      *bound   // rangeScan: nil means no lower bound
	high     *bound   // rangeScan: nil means no upper bound
	from, to string   // rangeScan: the key range, from included, to not
	col      string   // the column the plan is about
	index    string   // the index the keys belong to, or "" for the primary key
}

// keySpace says how one column's values turn into store keys, so the same
// planner works for the primary key, whose values are the row keys, and for
// an index, whose values are at the front of its entries.
type keySpace struct {
	col      column
	index    string             // "" for the primary key
	start    func(v any) string // the first key that can belong to v
	after    func(v any) string // the first key past every key of v
	from, to string             // every key in the space is in [from, to)
}

func pkSpace(table string, col column) keySpace {
	// Every row key of the table starts with "sql:row:<table>:", and ';'
	// is the byte after ':', so from and to hold exactly the table's rows.
	return keySpace{
		col:   col,
		start: func(v any) string { return rowKey(table, v) },
		after: func(v any) string { return rowKey(table, v) + "\x00" }, // the smallest key that sorts after v's
		from:  rowPrefix + table + ":",
		to:    rowPrefix + table + ";",
	}
}

func indexSpace(table string, ix indexDef, col column) keySpace {
	from, to := indexKeys(table, ix.Name)
	return keySpace{
		col:   col,
		index: ix.Name,
		start: func(v any) string { return indexValueStart(table, ix.Name, v) },
		after: func(v any) string { return indexValueAfter(table, ix.Name, v) },
		from:  from,
		to:    to,
	}
}

// bound is one end of a key range. key is where the range starts (for a
// lower bound) or the first key past it (for an upper bound), with the
// inclusive or exclusive end already worked in, so a tighter bound is just a
// bigger (lower) or smaller (upper) key.
type bound struct {
	val  any
	incl bool
	key  string
}

// planner collects what each AND-ed condition says about one column.
type planner struct {
	ks    keySpace
	col   column
	keyed bool  // some condition listed exact keys
	vals  []any // the values allowed so far, if keyed
	low   *bound
	high  *bound
	none  bool // some condition can never be TRUE
}

// planAccess works out which rows of table a statement with this WHERE has
// to read.
func planAccess(table string, s *schema, where Expr) access {
	best := access{kind: fullScan}
	if where == nil {
		return best
	}
	conds := conjuncts(where, nil)
	if pk := s.pk(); pk >= 0 {
		best = planSpace(pkSpace(table, s.Columns[pk]), conds)
	}
	for _, ix := range s.Indexes {
		col := s.Columns[s.index(ix.Column)]
		if a := planSpace(indexSpace(table, ix, col), conds); rank[a.kind] > rank[best.kind] {
			best = a
		}
	}
	return best
}

// rank orders plans from worst to best.
var rank = map[accessKind]int{fullScan: 0, rangeScan: 1, keyLookup: 2, noRows: 3}

// planSpace works out what conds allow in one key space.
func planSpace(ks keySpace, conds []Expr) access {
	p := &planner{ks: ks, col: ks.col}
	for _, c := range conds {
		p.add(c)
	}
	return p.finish()
}

// conjuncts splits "a AND b AND c" into a, b and c. ORs and NOTs are kept
// whole: neither side of an OR has to hold on its own.
func conjuncts(ex Expr, out []Expr) []Expr {
	if b, ok := ex.(Binary); ok && b.Op == "AND" {
		return conjuncts(b.Right, conjuncts(b.Left, out))
	}
	return append(out, ex)
}

// flipped is the operator that means the same with its sides swapped:
// 5 < id is id > 5.
var flipped = map[string]string{"=": "=", "<": ">", "<=": ">=", ">": "<", ">=": "<="}

func (p *planner) isCol(ex Expr) bool {
	ref, ok := ex.(ColumnRef)
	return ok && ref.Name == p.col.Name
}

func (p *planner) add(ex Expr) {
	switch x := ex.(type) {
	case Binary:
		if _, ok := flipped[x.Op]; !ok {
			return
		}
		c, l, op := x.Left, x.Right, x.Op
		if _, isLit := c.(Literal); isLit {
			c, l, op = l, c, flipped[x.Op]
		}
		lit, isLit := l.(Literal)
		if !p.isCol(c) || !isLit {
			return
		}
		if lit.Value == nil {
			p.none = true // a comparison with NULL is never TRUE
			return
		}
		v, err := coerce(lit.Value, p.col)
		if err != nil {
			return
		}
		switch op {
		case "=":
			p.allow([]any{v})
		case ">":
			p.lower(v, false)
		case ">=":
			p.lower(v, true)
		case "<":
			p.upper(v, false)
		case "<=":
			p.upper(v, true)
		}
	case In:
		if x.Negate || !p.isCol(x.Expr) {
			return
		}
		var vals []any
		for _, item := range x.List {
			lit, ok := item.(Literal)
			if !ok {
				return
			}
			if lit.Value == nil {
				continue // col = NULL never matches
			}
			v, err := coerce(lit.Value, p.col)
			if err != nil {
				return
			}
			vals = append(vals, v)
		}
		p.allow(vals)
	case Between:
		if x.Negate || !p.isCol(x.Expr) {
			return
		}
		low, okLow := x.Low.(Literal)
		high, okHigh := x.High.(Literal)
		if !okLow || !okHigh {
			return
		}
		if low.Value == nil || high.Value == nil {
			// Not quite "never TRUE" in general (9 BETWEEN NULL AND 5 is
			// FALSE, not NULL), but it is never TRUE, which is all WHERE
			// cares about.
			p.none = true
			return
		}
		lv, err1 := coerce(low.Value, p.col)
		hv, err2 := coerce(high.Value, p.col)
		if err1 != nil || err2 != nil {
			return
		}
		p.lower(lv, true)
		p.upper(hv, true)
	}
}

// allow narrows the set of allowed keys to vals (or to the values both lists
// share, if an earlier condition already listed some).
func (p *planner) allow(vals []any) {
	if !p.keyed {
		p.keyed, p.vals = true, vals
		return
	}
	var both []any
	for _, v := range vals {
		for _, w := range p.vals {
			if p.ks.start(v) == p.ks.start(w) {
				both = append(both, v)
				break
			}
		}
	}
	p.vals = both
}

// lower: > v starts past all of v's keys, >= v at the first of them.
func (p *planner) lower(v any, incl bool) {
	b := &bound{val: v, incl: incl, key: p.ks.after(v)}
	if incl {
		b.key = p.ks.start(v)
	}
	if p.low == nil || b.key > p.low.key {
		p.low = b
	}
}

// upper: < v stops before v's first key, <= v just past its last.
func (p *planner) upper(v any, incl bool) {
	b := &bound{val: v, incl: incl, key: p.ks.start(v)}
	if incl {
		b.key = p.ks.after(v)
	}
	if p.high == nil || b.key < p.high.key {
		p.high = b
	}
}

func (p *planner) finish() access {
	a := access{low: p.low, high: p.high, col: p.col.Name, index: p.ks.index}
	a.from, a.to = p.ks.from, p.ks.to
	if p.low != nil {
		a.from = p.low.key
	}
	if p.high != nil {
		a.to = p.high.key
	}
	switch {
	case p.none:
		a.kind = noRows
	case p.keyed:
		a.kind = keyLookup
		seen := map[string]bool{}
		for _, v := range p.vals {
			k := p.ks.start(v)
			if k < a.from || k >= a.to || seen[k] {
				continue
			}
			seen[k] = true
			a.keys = append(a.keys, k)
			a.ends = append(a.ends, p.ks.after(v))
			a.vals = append(a.vals, v)
		}
		// Sorted, so rows come back in the same order as a full table read.
		sort.Sort(byKey{a.keys, a.ends, a.vals})
		if len(a.keys) == 0 {
			a.kind = noRows
		}
	case p.low != nil || p.high != nil:
		a.kind = rangeScan
		if a.from >= a.to {
			a.kind = noRows
		}
	default:
		a.kind = fullScan
	}
	return a
}

// byKey sorts keys and the ends and values that go with them together.
type byKey struct {
	keys, ends []string
	vals       []any
}

func (b byKey) Len() int           { return len(b.keys) }
func (b byKey) Less(i, j int) bool { return b.keys[i] < b.keys[j] }
func (b byKey) Swap(i, j int) {
	b.keys[i], b.keys[j] = b.keys[j], b.keys[i]
	b.ends[i], b.ends[j] = b.ends[j], b.ends[i]
	b.vals[i], b.vals[j] = b.vals[j], b.vals[i]
}

// describe is the plan's first EXPLAIN line.
func (a access) describe(table string) string {
	by, unit := "primary key", "key"
	if a.index != "" {
		by, unit = "index "+a.index, "value"
	}
	switch a.kind {
	case keyLookup:
		parts := make([]string, len(a.vals))
		for i, v := range a.vals {
			parts[i] = Format(v)
		}
		if len(parts) == 1 {
			return fmt.Sprintf("lookup %s by %s: %s = %s (1 %s)", table, by, a.col, parts[0], unit)
		}
		return fmt.Sprintf("lookup %s by %s: %s IN (%s) (%d %ss)", table, by, a.col, strings.Join(parts, ", "), len(parts), unit)
	case rangeScan:
		var parts []string
		if a.low != nil {
			op := ">"
			if a.low.incl {
				op = ">="
			}
			parts = append(parts, fmt.Sprintf("%s %s %s", a.col, op, Format(a.low.val)))
		}
		if a.high != nil {
			op := "<"
			if a.high.incl {
				op = "<="
			}
			parts = append(parts, fmt.Sprintf("%s %s %s", a.col, op, Format(a.high.val)))
		}
		return fmt.Sprintf("range scan of %s by %s: %s", table, by, strings.Join(parts, " AND "))
	case noRows:
		if a.index != "" {
			return fmt.Sprintf("read nothing from %s: no row can match the condition on %s", table, a.col)
		}
		return fmt.Sprintf("read nothing from %s: no row can match the primary key condition", table)
	}
	return "full scan of " + table
}

// explain describes, step by step, how st would run, without running it.
// It checks the table and the columns first, so EXPLAIN of a query with a
// typo is an error, just like the query itself.
func (e *Executor) explain(st Statement) (Result, error) {
	var table string
	var where Expr
	var after []string
	switch x := st.(type) {
	case Select:
		table, where = x.Table, x.Where
		if len(x.GroupBy) > 0 {
			after = append(after, "group by "+strings.Join(x.GroupBy, ", "))
		} else if x.HasAggregates() {
			after = append(after, "aggregate all rows into one")
		}
		if x.Having != nil {
			after = append(after, "filter groups: "+exprString(x.Having))
		}
		switch {
		case x.OrderAgg != nil:
			after = append(after, "sort by "+x.OrderAgg.Name()+direction(x.Desc))
		case x.OrderBy != "":
			after = append(after, "sort by "+x.OrderBy+direction(x.Desc))
		}
		if x.Distinct {
			after = append(after, "drop duplicate rows")
		}
		if x.Limit >= 0 {
			after = append(after, fmt.Sprintf("keep the first %d", x.Limit))
		}
	case Update:
		table, where = x.Table, x.Where
		after = append(after, "update each matching row")
	case Delete:
		table, where = x.Table, x.Where
		after = append(after, "delete each matching row")
	default:
		return Result{}, fmt.Errorf("EXPLAIN works with SELECT, UPDATE and DELETE")
	}
	s, err := e.loadSchema(table)
	if err != nil {
		return Result{}, err
	}
	if err := checkColumns(s, where); err != nil {
		return Result{}, err
	}
	for _, name := range namedColumns(st) {
		if s.index(name) < 0 {
			return Result{}, fmt.Errorf("table %s has no column %s", table, name)
		}
	}
	a := planAccess(table, s, where)
	steps := []string{a.describe(table)}
	if where != nil && a.kind != noRows {
		steps = append(steps, "filter rows: "+exprString(where))
	}
	if a.kind != noRows {
		steps = append(steps, after...)
	}
	// Plain numbered lines rather than a result table, which would put
	// every step in quotes like a TEXT value.
	for i := range steps {
		steps[i] = fmt.Sprintf("%d. %s", i+1, steps[i])
	}
	return Result{Message: strings.Join(steps, "\n")}, nil
}

// namedColumns lists the columns a statement names outside WHERE.
func namedColumns(st Statement) []string {
	var names []string
	switch x := st.(type) {
	case Select:
		for _, it := range x.Items {
			if it.Agg != nil {
				names = append(names, it.Agg.Column)
			} else {
				names = append(names, it.Column)
			}
		}
		names = append(names, x.GroupBy...)
		names = append(names, x.OrderBy)
		if x.OrderAgg != nil {
			names = append(names, x.OrderAgg.Column)
		}
	case Update:
		for _, a := range x.Set {
			names = append(names, a.Column)
		}
	}
	out := names[:0]
	for _, n := range names {
		if n != "" { // "" is COUNT(*) or no ORDER BY
			out = append(out, n)
		}
	}
	return out
}

func direction(desc bool) string {
	if desc {
		return " DESC"
	}
	return ""
}

// exprString writes an expression back out as SQL, with brackets wherever
// AND and OR (or NOT) meet, so the grouping is never ambiguous.
func exprString(ex Expr) string {
	switch x := ex.(type) {
	case Literal:
		return Format(x.Value)
	case ColumnRef:
		return x.Name
	case AggregateRef:
		if x.Agg.Column == "" {
			return "COUNT(*)"
		}
		if x.Agg.Distinct {
			return x.Agg.Func + "(DISTINCT " + x.Agg.Column + ")"
		}
		return x.Agg.Func + "(" + x.Agg.Column + ")"
	case Binary:
		logical := func(op string) bool { return op == "AND" || op == "OR" }
		side := func(e Expr) string {
			// Brackets when AND meets OR, and around anything with an
			// operator inside a comparison: (a = 1) = TRUE.
			if b, ok := e.(Binary); ok && (!logical(x.Op) || logical(b.Op) && b.Op != x.Op) {
				return "(" + exprString(e) + ")"
			}
			return exprString(e)
		}
		return side(x.Left) + " " + x.Op + " " + side(x.Right)
	case Not:
		if b, ok := x.Expr.(Binary); ok && (b.Op == "AND" || b.Op == "OR") {
			return "NOT (" + exprString(x.Expr) + ")"
		}
		return "NOT " + exprString(x.Expr)
	case IsNull:
		if x.Negate {
			return exprString(x.Expr) + " IS NOT NULL"
		}
		return exprString(x.Expr) + " IS NULL"
	case In:
		parts := make([]string, len(x.List))
		for i, item := range x.List {
			parts[i] = exprString(item)
		}
		return exprString(x.Expr) + negated(x.Negate) + " IN (" + strings.Join(parts, ", ") + ")"
	case Like:
		return exprString(x.Expr) + negated(x.Negate) + " LIKE " + exprString(x.Pattern)
	case Between:
		return exprString(x.Expr) + negated(x.Negate) + " BETWEEN " + exprString(x.Low) + " AND " + exprString(x.High)
	}
	return fmt.Sprintf("%v", ex)
}

func negated(n bool) string {
	if n {
		return " NOT"
	}
	return ""
}
