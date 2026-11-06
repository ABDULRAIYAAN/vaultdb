package sql

import (
	"encoding/json"
	"errors"
	"fmt"
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
// "primary key = value", it fetches that one row directly with a GET instead
// of reading the whole table.
func (e *Executor) matchingRows(table string, s *schema, where Expr) ([]storedRow, error) {
	var candidates []store.KV
	if key, ok := pkLookup(table, s, where); ok {
		if raw, err := e.st.Get(key); err == nil {
			candidates = []store.KV{{Key: key, Value: raw}}
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

// pkLookup recognises WHERE pk = literal (either way round).
func pkLookup(table string, s *schema, where Expr) (string, bool) {
	b, ok := where.(Binary)
	pk := s.pk()
	if !ok || b.Op != "=" || pk < 0 {
		return "", false
	}
	c, l := b.Left, b.Right
	if _, isLit := c.(Literal); isLit {
		c, l = l, c
	}
	ref, ok1 := c.(ColumnRef)
	lit, ok2 := l.(Literal)
	if !ok1 || !ok2 || ref.Name != s.Columns[pk].Name {
		return "", false
	}
	v, err := coerce(lit.Value, s.Columns[pk])
	if err != nil || v == nil {
		return "", false
	}
	return rowKey(table, v), true
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
	if sel.Limit >= 0 && len(rows) > sel.Limit {
		rows = rows[:sel.Limit]
	}

	if sel.Count {
		return Result{Columns: []string{"count"}, Rows: [][]any{{int64(len(rows))}}}, nil
	}

	pick := make([]int, 0, len(s.Columns))
	res := Result{}
	if len(sel.Columns) == 0 {
		for i, c := range s.Columns {
			pick = append(pick, i)
			res.Columns = append(res.Columns, c.Name)
		}
	} else {
		for _, name := range sel.Columns {
			i := s.index(name)
			if i < 0 {
				return Result{}, fmt.Errorf("table %s has no column %s", sel.Table, name)
			}
			pick = append(pick, i)
			res.Columns = append(res.Columns, name)
		}
	}
	for _, r := range rows {
		out := make([]any, len(pick))
		for j, i := range pick {
			out[j] = r.vals[i]
		}
		res.Rows = append(res.Rows, out)
	}
	return res, nil
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
	}
	return nil
}

func plural(n int, word string) string {
	if n == 1 {
		return word
	}
	return word + "s"
}
