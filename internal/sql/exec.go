package sql

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
	"sync"

	"github.com/ABDULRAIYAAN/vaultdb/internal/store"
)

// How tables are stored
//
// There is no separate table engine: every table lives inside the same
// key-value store, as ordinary keys. That is how real databases such as
// CockroachDB and TiDB work too.
//
//	sql:schema:students        -> {"columns":[{"name":"id","type":"INT","pk":true}, ...]}
//	sql:seq:students           -> last automatic row id (tables without a primary key)
//	sql:row:students:<key>     -> [1,"Ali",3.5,true]     one row, values in column order
//
// Because the store is a B-tree, all rows of a table sit next to each other
// (they share the prefix "sql:row:students:"), so reading a whole table is
// one PREFIX walk, and finding a row by primary key is one GET.

const (
	schemaPrefix = "sql:schema:"
	seqPrefix    = "sql:seq:"
	rowPrefix    = "sql:row:"
)

// Store is the part of the key-value store the executor needs.
type Store interface {
	Get(key string) (string, error)
	Set(key, value string) error
	Delete(key string) (bool, error)
	Prefix(prefix string, limit int) ([]store.KV, bool)
}

// Executor runs parsed statements against a Store.
//
// Statements run one at a time. A single statement can touch many keys, for
// example INSERT checks that a primary key is free and then writes the row.
// If two network clients ran INSERTs at once, both could see the key as free
// and both would write it. The mutex makes each statement all-or-nothing as
// far as other clients can tell.
type Executor struct {
	mu sync.Mutex
	st Store
}

// NewExecutor creates an executor on top of st.
func NewExecutor(st Store) *Executor { return &Executor{st: st} }

// Result is what a statement returns: rows for SELECT, a message otherwise.
type Result struct {
	Columns []string
	Rows    [][]any
	Message string
}

type column struct {
	Name string `json:"name"`
	Type Type   `json:"type"`
	PK   bool   `json:"pk,omitempty"`
}

type schema struct {
	Columns []column `json:"columns"`
}

func (s *schema) index(name string) int {
	for i, c := range s.Columns {
		if c.Name == name {
			return i
		}
	}
	return -1
}

func (s *schema) pk() int {
	for i, c := range s.Columns {
		if c.PK {
			return i
		}
	}
	return -1
}

// Exec parses and runs one SQL statement.
func (e *Executor) Exec(query string) (Result, error) {
	st, err := Parse(query)
	if err != nil {
		return Result{}, err
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	switch s := st.(type) {
	case CreateTable:
		return e.create(s)
	case DropTable:
		return e.drop(s)
	case ShowTables:
		return e.showTables()
	case Insert:
		return e.insert(s)
	case Select:
		return e.selectRows(s)
	case Update:
		return e.update(s)
	case Delete:
		return e.deleteRows(s)
	}
	return Result{}, fmt.Errorf("unsupported statement %T", st)
}

func (e *Executor) loadSchema(table string) (*schema, error) {
	raw, err := e.st.Get(schemaPrefix + table)
	if errors.Is(err, store.ErrNotFound) {
		return nil, fmt.Errorf("table %s does not exist", table)
	}
	if err != nil {
		return nil, err
	}
	var s schema
	if err := json.Unmarshal([]byte(raw), &s); err != nil {
		return nil, fmt.Errorf("schema of table %s is damaged: %v", table, err)
	}
	return &s, nil
}

func (e *Executor) create(c CreateTable) (Result, error) {
	if _, err := e.st.Get(schemaPrefix + c.Table); err == nil {
		return Result{}, fmt.Errorf("table %s already exists", c.Table)
	}
	s := schema{}
	seen := map[string]bool{}
	for _, def := range c.Columns {
		if seen[def.Name] {
			return Result{}, fmt.Errorf("column %s is listed twice", def.Name)
		}
		seen[def.Name] = true
		if def.PrimaryKey {
			if s.pk() >= 0 {
				return Result{}, errors.New("a table can have only one PRIMARY KEY column")
			}
			if def.Type != TypeInt && def.Type != TypeText {
				return Result{}, fmt.Errorf("PRIMARY KEY column %s must be INT or TEXT", def.Name)
			}
		}
		s.Columns = append(s.Columns, column{Name: def.Name, Type: def.Type, PK: def.PrimaryKey})
	}
	raw, _ := json.Marshal(s)
	if err := e.st.Set(schemaPrefix+c.Table, string(raw)); err != nil {
		return Result{}, err
	}
	return Result{Message: fmt.Sprintf("OK, table %s created", c.Table)}, nil
}

func (e *Executor) drop(d DropTable) (Result, error) {
	if _, err := e.loadSchema(d.Table); err != nil {
		return Result{}, err
	}
	rows, _ := e.st.Prefix(rowPrefix+d.Table+":", 0)
	for _, kv := range rows {
		if _, err := e.st.Delete(kv.Key); err != nil {
			return Result{}, err
		}
	}
	e.st.Delete(seqPrefix + d.Table)
	if _, err := e.st.Delete(schemaPrefix + d.Table); err != nil {
		return Result{}, err
	}
	return Result{Message: fmt.Sprintf("OK, table %s dropped (%d rows deleted)", d.Table, len(rows))}, nil
}

func (e *Executor) showTables() (Result, error) {
	pairs, _ := e.st.Prefix(schemaPrefix, 0)
	res := Result{Columns: []string{"table"}}
	for _, kv := range pairs {
		res.Rows = append(res.Rows, []any{strings.TrimPrefix(kv.Key, schemaPrefix)})
	}
	return res, nil
}

// rowKey builds the store key for a row. Integer keys are encoded so that
// sorting the text sorts the numbers correctly, including negatives:
// flip the sign bit and write 16 hex digits. So -1 < 0 < 2 < 10 in key order.
func rowKey(table string, pk any) string {
	switch v := pk.(type) {
	case int64:
		return fmt.Sprintf("%s%s:%016x", rowPrefix, table, uint64(v)^(1<<63))
	default:
		return fmt.Sprintf("%s%s:%v", rowPrefix, table, v)
	}
}

func (e *Executor) nextRowID(table string) (int64, error) {
	var n int64
	if raw, err := e.st.Get(seqPrefix + table); err == nil {
		fmt.Sscan(raw, &n)
	}
	n++
	return n, e.st.Set(seqPrefix+table, fmt.Sprint(n))
}

func (e *Executor) insert(ins Insert) (Result, error) {
	s, err := e.loadSchema(ins.Table)
	if err != nil {
		return Result{}, err
	}
	// Which table column does each value go into?
	targets := make([]int, 0, len(s.Columns))
	if len(ins.Columns) == 0 {
		for i := range s.Columns {
			targets = append(targets, i)
		}
	} else {
		for _, name := range ins.Columns {
			i := s.index(name)
			if i < 0 {
				return Result{}, fmt.Errorf("table %s has no column %s", ins.Table, name)
			}
			targets = append(targets, i)
		}
	}

	// Build and check every row first, so a bad row stops the whole INSERT
	// before anything is written.
	pk := s.pk()
	rows := make([][]any, 0, len(ins.Rows))
	newKeys := map[string]bool{}
	for n, exprs := range ins.Rows {
		if len(exprs) != len(targets) {
			return Result{}, fmt.Errorf("row %d has %d values but %d columns were expected", n+1, len(exprs), len(targets))
		}
		row := make([]any, len(s.Columns))
		for j, ex := range exprs {
			v, err := eval(ex, nil, nil)
			if err != nil {
				return Result{}, err
			}
			col := s.Columns[targets[j]]
			if row[targets[j]], err = coerce(v, col); err != nil {
				return Result{}, fmt.Errorf("row %d: %v", n+1, err)
			}
		}
		if pk >= 0 {
			if row[pk] == nil {
				return Result{}, fmt.Errorf("row %d: primary key %s cannot be NULL", n+1, s.Columns[pk].Name)
			}
			k := rowKey(ins.Table, row[pk])
			_, err := e.st.Get(k)
			if err == nil || newKeys[k] {
				return Result{}, fmt.Errorf("row %d: a row with %s = %v already exists", n+1, s.Columns[pk].Name, row[pk])
			}
			newKeys[k] = true
		}
		rows = append(rows, row)
	}

	for _, row := range rows {
		var key string
		if pk >= 0 {
			key = rowKey(ins.Table, row[pk])
		} else {
			id, err := e.nextRowID(ins.Table)
			if err != nil {
				return Result{}, err
			}
			key = rowKey(ins.Table, id)
		}
		if err := e.writeRow(key, row); err != nil {
			return Result{}, err
		}
	}
	return Result{Message: fmt.Sprintf("OK, %d %s inserted", len(rows), plural(len(rows), "row"))}, nil
}

func (e *Executor) writeRow(key string, row []any) error {
	raw, err := json.Marshal(row)
	if err != nil {
		return err
	}
	return e.st.Set(key, string(raw))
}

func decodeRow(raw string, s *schema) ([]any, error) {
	dec := json.NewDecoder(strings.NewReader(raw))
	dec.UseNumber() // keep 42 and 42.0 apart until we know the column type
	var vals []any
	if err := dec.Decode(&vals); err != nil || len(vals) != len(s.Columns) {
		return nil, fmt.Errorf("stored row is damaged")
	}
	for i, v := range vals {
		if num, ok := v.(json.Number); ok {
			if s.Columns[i].Type == TypeInt {
				vals[i], _ = num.Int64()
			} else {
				vals[i], _ = num.Float64()
			}
		}
	}
	return vals, nil
}

type storedRow struct {
	key  string
	vals []any
}

// matchingRows returns every row of the table that passes where. If where is
// "primary key = value" or "primary key IN (values)", it fetches just those
// rows directly with a GET each instead of reading the whole table.
func (e *Executor) matchingRows(table string, s *schema, where Expr) ([]storedRow, error) {
	var candidates []store.KV
	if keys, ok := pkLookup(table, s, where); ok {
		for _, key := range keys {
			if raw, err := e.st.Get(key); err == nil {
				candidates = append(candidates, store.KV{Key: key, Value: raw})
			}
		}
	} else {
		candidates, _ = e.st.Prefix(rowPrefix+table+":", 0)
	}

	var out []storedRow
	for _, kv := range candidates {
		vals, err := decodeRow(kv.Value, s)
		if err != nil {
			return nil, fmt.Errorf("table %s: %v", table, err)
		}
		if where != nil {
			v, err := eval(where, s, vals)
			if err != nil {
				return nil, err
			}
			if v != true {
				continue
			}
		}
		out = append(out, storedRow{kv.Key, vals})
	}
	return out, nil
}

// pkLookup recognises "WHERE pk = literal" (either way round) and
// "WHERE pk IN (literal, ...)", and returns the row keys to fetch, sorted and
// without repeats so rows come back in the same order as a full table read.
// Anything it can't turn into a key exactly (a value of the wrong type, say)
// makes it give up, and the caller falls back to reading every row, so the
// answer, or the error, is always the same as without the shortcut.
func pkLookup(table string, s *schema, where Expr) ([]string, bool) {
	pk := s.pk()
	if pk < 0 {
		return nil, false
	}
	isPK := func(ex Expr) bool {
		ref, ok := ex.(ColumnRef)
		return ok && ref.Name == s.Columns[pk].Name
	}
	var lits []Expr
	switch x := where.(type) {
	case Binary:
		if x.Op != "=" {
			return nil, false
		}
		c, l := x.Left, x.Right
		if _, isLit := c.(Literal); isLit {
			c, l = l, c
		}
		if !isPK(c) {
			return nil, false
		}
		lits = []Expr{l}
	case In:
		if x.Negate || !isPK(x.Expr) {
			return nil, false
		}
		lits = x.List
	default:
		return nil, false
	}

	var keys []string
	seen := map[string]bool{}
	for _, ex := range lits {
		lit, ok := ex.(Literal)
		if !ok {
			return nil, false
		}
		if lit.Value == nil {
			continue // pk = NULL never matches, and a pk is never NULL anyway
		}
		v, err := coerce(lit.Value, s.Columns[pk])
		if err != nil {
			return nil, false
		}
		if k := rowKey(table, v); !seen[k] {
			seen[k] = true
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	return keys, true
}

func (e *Executor) selectRows(sel Select) (Result, error) {
	s, err := e.loadSchema(sel.Table)
	if err != nil {
		return Result{}, err
	}
	if err := checkColumns(s, sel.Where); err != nil {
		return Result{}, err
	}
	rows, err := e.matchingRows(sel.Table, s, sel.Where)
	if err != nil {
		return Result{}, err
	}

	if sel.HasAggregates() || len(sel.GroupBy) > 0 {
		return groupRows(sel, s, rows)
	}
	if sel.OrderAgg != nil {
		return Result{}, fmt.Errorf("ORDER BY %s needs GROUP BY or aggregate functions in the select list", sel.OrderAgg.Name())
	}

	if sel.OrderBy != "" {
		i := s.index(sel.OrderBy)
		if i < 0 {
			return Result{}, fmt.Errorf("table %s has no column %s", sel.Table, sel.OrderBy)
		}
		sort.SliceStable(rows, func(a, b int) bool {
			c := compareForSort(rows[a].vals[i], rows[b].vals[i])
			if sel.Desc {
				return c > 0
			}
			return c < 0
		})
	}
	if sel.Limit >= 0 && len(rows) > sel.Limit && !sel.Distinct {
		rows = rows[:sel.Limit] // fewer rows to copy; with DISTINCT, LIMIT waits until duplicates are gone
	}

	pick := make([]int, 0, len(s.Columns))
	res := Result{}
	if len(sel.Items) == 0 {
		for i, c := range s.Columns {
			pick = append(pick, i)
			res.Columns = append(res.Columns, c.Name)
		}
	} else {
		for _, it := range sel.Items {
			i := s.index(it.Column)
			if i < 0 {
				return Result{}, fmt.Errorf("table %s has no column %s", sel.Table, it.Column)
			}
			pick = append(pick, i)
			res.Columns = append(res.Columns, it.Column)
		}
	}
	for _, r := range rows {
		out := make([]any, len(pick))
		for j, i := range pick {
			out[j] = r.vals[i]
		}
		res.Rows = append(res.Rows, out)
	}
	if sel.Distinct {
		res.Rows = distinctRows(res.Rows)
	}
	res.Rows = limitRows(res.Rows, sel.Limit)
	return res, nil
}

// distinctRows keeps the first of every set of identical rows and drops the
// rest, without changing their order (so an ORDER BY still holds). NULLs
// count as equal here, as in standard SQL's DISTINCT.
func distinctRows(rows [][]any) [][]any {
	seen := make(map[string]bool, len(rows))
	kept := rows[:0]
	for _, r := range rows {
		k := valuesKey(r)
		if !seen[k] {
			seen[k] = true
			kept = append(kept, r)
		}
	}
	return kept
}

// limitRows cuts rows down to the first limit of them; -1 means no limit.
func limitRows(rows [][]any, limit int) [][]any {
	if limit >= 0 && len(rows) > limit {
		return rows[:limit]
	}
	return rows
}

// valuesKey turns a list of values into a string that is equal for two lists
// exactly when their values are equal, for use as a map key. It is the JSON
// text of the values: a column has one type, so equal values give equal
// text. The one exception is -0.0, which equals 0.0 but is written "-0", so
// it is turned into 0.0 first.
func valuesKey(vals []any) string {
	norm := vals
	copied := false // only copy vals if there is a -0.0 to fix, and never change the caller's slice
	for i, v := range vals {
		if f, ok := v.(float64); ok && f == 0 && math.Signbit(f) {
			if !copied {
				norm = append([]any(nil), vals...)
				copied = true
			}
			norm[i] = 0.0
		}
	}
	raw, _ := json.Marshal(norm)
	return string(raw)
}

// group is the rows that share one combination of GROUP BY values.
type group struct {
	keys []any // the GROUP BY column values, in GROUP BY order
	rows []storedRow
}

// groupRows answers a query with aggregates and/or GROUP BY.
//
// Without GROUP BY every matching row falls into a single group, so the
// result is exactly one row, even when no rows matched (COUNT is then 0 and
// the other aggregates NULL, as in standard SQL).
//
// With GROUP BY, rows are bucketed by their GROUP BY values: a map from an
// encoding of those values to the group. NULLs count as equal here, so all
// rows with a NULL dept form one group, again as in standard SQL. HAVING then
// drops the groups whose condition is not true. Groups come back sorted by
// their GROUP BY values unless ORDER BY says otherwise, and LIMIT counts
// groups, not the rows inside them.
func groupRows(sel Select, s *schema, rows []storedRow) (Result, error) {
	// Check every name up front, so a typo is reported even with no rows.
	groupIdx := make([]int, len(sel.GroupBy))
	for j, g := range sel.GroupBy {
		if groupIdx[j] = s.index(g); groupIdx[j] < 0 {
			return Result{}, fmt.Errorf("table %s has no column %s", sel.Table, g)
		}
	}
	for _, it := range sel.Items {
		if it.Agg != nil {
			if err := checkAggregate(*it.Agg, sel.Table, s); err != nil {
				return Result{}, err
			}
		}
	}
	for _, a := range havingAggregates(sel.Having, nil) {
		if err := checkAggregate(a, sel.Table, s); err != nil {
			return Result{}, err
		}
	}
	orderKey := -1 // which GROUP BY column ORDER BY names, if any
	switch {
	case sel.OrderAgg != nil:
		if len(sel.GroupBy) == 0 {
			return Result{}, fmt.Errorf("ORDER BY cannot be used with aggregate functions without GROUP BY: they return a single row")
		}
		if err := checkAggregate(*sel.OrderAgg, sel.Table, s); err != nil {
			return Result{}, err
		}
	case sel.OrderBy != "":
		if len(sel.GroupBy) == 0 {
			return Result{}, fmt.Errorf("ORDER BY cannot be used with aggregate functions without GROUP BY: they return a single row")
		}
		for j, g := range sel.GroupBy {
			if g == sel.OrderBy {
				orderKey = j
			}
		}
		if orderKey < 0 {
			return Result{}, fmt.Errorf("ORDER BY %s: a grouped query can only be sorted by a GROUP BY column or an aggregate", sel.OrderBy)
		}
	}

	var groups []*group
	if len(sel.GroupBy) == 0 {
		groups = []*group{{rows: rows}}
	} else {
		byKey := map[string]*group{}
		for _, r := range rows {
			keys := make([]any, len(groupIdx))
			for j, i := range groupIdx {
				keys[j] = r.vals[i]
			}
			k := valuesKey(keys)
			g := byKey[k]
			if g == nil {
				g = &group{keys: keys}
				byKey[k] = g
				groups = append(groups, g)
			}
			g.rows = append(g.rows, r)
		}
		sort.Slice(groups, func(a, b int) bool { return compareKeys(groups[a].keys, groups[b].keys) < 0 })
	}

	if sel.Having != nil {
		kept := groups[:0]
		for _, g := range groups {
			cond, err := bindGroup(sel.Having, sel, s, g)
			if err != nil {
				return Result{}, err
			}
			v, err := eval(cond, nil, nil)
			if err != nil {
				return Result{}, fmt.Errorf("HAVING: %w", err)
			}
			if v == true {
				kept = append(kept, g)
			}
		}
		groups = kept
	}

	if sel.OrderAgg != nil || orderKey >= 0 {
		sortBy := make([]any, len(groups))
		for n, g := range groups {
			if orderKey >= 0 {
				sortBy[n] = g.keys[orderKey]
				continue
			}
			v, err := aggregate(*sel.OrderAgg, s, g.rows)
			if err != nil {
				return Result{}, err
			}
			sortBy[n] = v
		}
		// Sort an index so the groups and their sort values move together.
		perm := make([]int, len(groups))
		for n := range perm {
			perm[n] = n
		}
		sort.SliceStable(perm, func(a, b int) bool {
			c := compareForSort(sortBy[perm[a]], sortBy[perm[b]])
			if sel.Desc {
				return c > 0
			}
			return c < 0
		})
		sorted := make([]*group, len(groups))
		for n, p := range perm {
			sorted[n] = groups[p]
		}
		groups = sorted
	}
	if sel.Limit >= 0 && len(groups) > sel.Limit && !sel.Distinct {
		groups = groups[:sel.Limit] // skip aggregating groups LIMIT would drop anyway
	}

	res := Result{Columns: make([]string, len(sel.Items))}
	for j, it := range sel.Items {
		res.Columns[j] = it.Name()
	}
	for _, g := range groups {
		out := make([]any, len(sel.Items))
		for j, it := range sel.Items {
			if it.Agg == nil {
				// The parser made sure this column is in GROUP BY.
				for k, name := range sel.GroupBy {
					if name == it.Column {
						out[j] = g.keys[k]
					}
				}
				continue
			}
			v, err := aggregate(*it.Agg, s, g.rows)
			if err != nil {
				return Result{}, err
			}
			out[j] = v
		}
		res.Rows = append(res.Rows, out)
	}
	if sel.Distinct {
		res.Rows = distinctRows(res.Rows)
	}
	res.Rows = limitRows(res.Rows, sel.Limit)
	return res, nil
}

// havingAggregates lists every aggregate used in a HAVING condition.
func havingAggregates(ex Expr, found []Aggregate) []Aggregate {
	switch x := ex.(type) {
	case AggregateRef:
		return append(found, x.Agg)
	case Binary:
		return havingAggregates(x.Right, havingAggregates(x.Left, found))
	case Not:
		return havingAggregates(x.Expr, found)
	case IsNull:
		return havingAggregates(x.Expr, found)
	case In:
		found = havingAggregates(x.Expr, found)
		for _, item := range x.List {
			found = havingAggregates(item, found)
		}
		return found
	case Like:
		return havingAggregates(x.Pattern, havingAggregates(x.Expr, found))
	}
	return found
}

// bindGroup turns a HAVING condition into one with only literals in it, by
// replacing each aggregate with its value over g's rows and each GROUP BY
// column with g's value for it. The result can then go through the ordinary
// eval, so HAVING gets exactly the same NULL and comparison rules as WHERE.
func bindGroup(ex Expr, sel Select, s *schema, g *group) (Expr, error) {
	switch x := ex.(type) {
	case AggregateRef:
		v, err := aggregate(x.Agg, s, g.rows)
		return Literal{Value: v}, err
	case ColumnRef:
		// The parser made sure this column is in GROUP BY.
		for k, name := range sel.GroupBy {
			if name == x.Name {
				return Literal{Value: g.keys[k]}, nil
			}
		}
		return nil, fmt.Errorf("column %s in HAVING must be listed in GROUP BY", x.Name)
	case Binary:
		l, err := bindGroup(x.Left, sel, s, g)
		if err != nil {
			return nil, err
		}
		r, err := bindGroup(x.Right, sel, s, g)
		if err != nil {
			return nil, err
		}
		return Binary{Op: x.Op, Left: l, Right: r}, nil
	case Not:
		e, err := bindGroup(x.Expr, sel, s, g)
		return Not{Expr: e}, err
	case IsNull:
		e, err := bindGroup(x.Expr, sel, s, g)
		return IsNull{Expr: e, Negate: x.Negate}, err
	case In:
		e, err := bindGroup(x.Expr, sel, s, g)
		if err != nil {
			return nil, err
		}
		list := make([]Expr, len(x.List))
		for i, item := range x.List {
			if list[i], err = bindGroup(item, sel, s, g); err != nil {
				return nil, err
			}
		}
		return In{Expr: e, List: list, Negate: x.Negate}, nil
	case Like:
		e, err := bindGroup(x.Expr, sel, s, g)
		if err != nil {
			return nil, err
		}
		pat, err := bindGroup(x.Pattern, sel, s, g)
		return Like{Expr: e, Pattern: pat, Negate: x.Negate}, err
	}
	return ex, nil
}

// compareKeys orders two lists of GROUP BY values column by column.
func compareKeys(a, b []any) int {
	for i := range a {
		if c := compareForSort(a[i], b[i]); c != 0 {
			return c
		}
	}
	return 0
}

// checkAggregate makes sure an aggregate names a real column of a type it
// can work on.
func checkAggregate(a Aggregate, table string, s *schema) error {
	if a.Column == "" { // COUNT(*)
		return nil
	}
	i := s.index(a.Column)
	if i < 0 {
		return fmt.Errorf("table %s has no column %s", table, a.Column)
	}
	c := s.Columns[i]
	if (a.Func == "SUM" || a.Func == "AVG") && c.Type != TypeInt && c.Type != TypeFloat {
		return fmt.Errorf("%s needs an INT or FLOAT column, but %s is %s", a.Func, c.Name, c.Type)
	}
	return nil
}

// aggregate computes one aggregate over rows. checkAggregate must have
// accepted a first.
func aggregate(a Aggregate, s *schema, rows []storedRow) (any, error) {
	if a.Column == "" { // COUNT(*)
		return int64(len(rows)), nil
	}
	i := s.index(a.Column)
	c := s.Columns[i]

	var (
		count  int64
		sumInt int64   // exact running total for INT columns
		sumF   float64 // running total for FLOAT columns and for AVG
		best   any     // current MIN or MAX
		seen   map[any]bool
	)
	if a.Distinct {
		seen = map[any]bool{}
	}
	for _, r := range rows {
		v := r.vals[i]
		if v == nil {
			continue
		}
		if a.Distinct {
			// Values are int64, float64, string or bool, which all work as
			// map keys. Go's map also treats 0.0 and -0.0 as the same key.
			if seen[v] {
				continue
			}
			seen[v] = true
		}
		count++
		switch a.Func {
		case "SUM", "AVG":
			f, _ := toFloat(v)
			sumF += f
			if n, ok := v.(int64); ok && a.Func == "SUM" {
				if (n > 0 && sumInt > math.MaxInt64-n) || (n < 0 && sumInt < math.MinInt64-n) {
					return nil, fmt.Errorf("SUM(%s) is too large for an INT", c.Name)
				}
				sumInt += n
			}
		case "MIN":
			if best == nil || compareForSort(v, best) < 0 {
				best = v
			}
		case "MAX":
			if best == nil || compareForSort(v, best) > 0 {
				best = v
			}
		}
	}

	switch a.Func {
	case "COUNT":
		return count, nil
	case "MIN", "MAX":
		return best, nil
	}
	if count == 0 {
		return nil, nil // SUM or AVG of no values
	}
	if a.Func == "AVG" {
		return sumF / float64(count), nil
	}
	if c.Type == TypeInt {
		return sumInt, nil
	}
	return sumF, nil
}

func (e *Executor) update(u Update) (Result, error) {
	s, err := e.loadSchema(u.Table)
	if err != nil {
		return Result{}, err
	}
	if err := checkColumns(s, u.Where); err != nil {
		return Result{}, err
	}
	targets := make([]int, len(u.Set))
	for j, a := range u.Set {
		i := s.index(a.Column)
		if i < 0 {
			return Result{}, fmt.Errorf("table %s has no column %s", u.Table, a.Column)
		}
		if s.Columns[i].PK {
			return Result{}, fmt.Errorf("the primary key %s cannot be changed; delete the row and insert it again", a.Column)
		}
		if err := checkColumns(s, a.Value); err != nil {
			return Result{}, err
		}
		targets[j] = i
	}
	rows, err := e.matchingRows(u.Table, s, u.Where)
	if err != nil {
		return Result{}, err
	}
	// Work out every new row before writing any of them.
	updated := make([][]any, len(rows))
	for n, r := range rows {
		newVals := append([]any(nil), r.vals...)
		for j, a := range u.Set {
			v, err := eval(a.Value, s, r.vals) // right-hand side sees the old row
			if err != nil {
				return Result{}, err
			}
			if newVals[targets[j]], err = coerce(v, s.Columns[targets[j]]); err != nil {
				return Result{}, err
			}
		}
		updated[n] = newVals
	}
	for n, r := range rows {
		if err := e.writeRow(r.key, updated[n]); err != nil {
			return Result{}, err
		}
	}
	return Result{Message: fmt.Sprintf("OK, %d %s updated", len(rows), plural(len(rows), "row"))}, nil
}

func (e *Executor) deleteRows(d Delete) (Result, error) {
	s, err := e.loadSchema(d.Table)
	if err != nil {
		return Result{}, err
	}
	if err := checkColumns(s, d.Where); err != nil {
		return Result{}, err
	}
	rows, err := e.matchingRows(d.Table, s, d.Where)
	if err != nil {
		return Result{}, err
	}
	for _, r := range rows {
		if _, err := e.st.Delete(r.key); err != nil {
			return Result{}, err
		}
	}
	return Result{Message: fmt.Sprintf("OK, %d %s deleted", len(rows), plural(len(rows), "row"))}, nil
}

// checkColumns makes sure every column an expression mentions exists, so a
// typo is reported even when the table is empty.
func checkColumns(s *schema, ex Expr) error {
	switch x := ex.(type) {
	case ColumnRef:
		if s.index(x.Name) < 0 {
			return fmt.Errorf("unknown column %s", x.Name)
		}
	case Binary:
		if err := checkColumns(s, x.Left); err != nil {
			return err
		}
		return checkColumns(s, x.Right)
	case Not:
		return checkColumns(s, x.Expr)
	case IsNull:
		return checkColumns(s, x.Expr)
	case In:
		if err := checkColumns(s, x.Expr); err != nil {
			return err
		}
		for _, item := range x.List {
			if err := checkColumns(s, item); err != nil {
				return err
			}
		}
	case Like:
		if err := checkColumns(s, x.Expr); err != nil {
			return err
		}
		return checkColumns(s, x.Pattern)
	}
	return nil
}

func plural(n int, word string) string {
	if n == 1 {
		return word
	}
	return word + "s"
}
