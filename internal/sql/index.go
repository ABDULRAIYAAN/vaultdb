package sql

import (
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strings"

	"github.com/ABDULRAIYAAN/vaultdb/internal/store"
)

// Secondary indexes
//
// The primary key is the only column whose values are part of a row's store
// key, so it is the only column the store can look up directly. An index
// gives another column the same power by keeping, next to the rows, one
// extra key per row: the column's value followed by the row's key.
//
//	CREATE INDEX by_dept ON staff (dept)
//
//	sql:row:staff:<id 1>                 -> (1, 'Ali', 'eng', ...)
//	sql:idx:staff:by_dept:eng<end><id 1> -> sql:row:staff:<id 1>
//	sql:idx:staff:by_dept:eng<end><id 2> -> sql:row:staff:<id 2>
//	sql:idx:staff:by_dept:sales<end><id 4> -> sql:row:staff:<id 4>
//
// Because the store is a B-tree, all entries for 'eng' sit next to each other,
// so "WHERE dept = 'eng'" is one short range walk followed by a GET per row,
// and since the values are encoded so that key order is value order,
// "WHERE dept >= 'e' AND dept < 'f'" is one walk too.
//
// How a value is encoded (see indexValue):
//
//	INT    16 hex digits with the sign bit flipped, like an INT primary key
//	FLOAT  16 hex digits of the IEEE 754 bits, with the sign bit flipped for
//	       positive numbers and every bit flipped for negative ones, which is
//	       the standard trick that makes the bits sort like the numbers
//	BOOL   0 or 1
//	TEXT   the bytes, with every 0x00 written as 0x00 0xFF
//
// and then <end>, the two bytes 0x00 0x01. TEXT needs the escaping so that
// <end> can't appear inside a value: without it, the entries for 'a' would be
// a prefix of the entries for 'a\x00b'. With it, every value's entries are
// exactly the keys from value+<end> up to value+0x00 0x02, and no other
// value's entries fall in between.
//
// NULLs are not indexed. WHERE keeps a row only when its condition is TRUE,
// and no comparison with NULL is ever TRUE, so no index lookup could want
// those rows anyway.
//
// Keeping the index right
//
// INSERT, UPDATE and DELETE change the index together with the row, in an
// order chosen so that a crash between two writes can only leave an extra
// index entry, never a missing one:
//
//	INSERT  index entry first, then the row
//	UPDATE  the new entry, then the row, then remove the old entry
//	DELETE  the row first, then its entry
//
// An extra entry is harmless: either its row is gone, and the GET finds
// nothing, or the row has another value now, and the usual check of every
// row against the whole WHERE throws it away. A missing entry would make a
// query silently skip a row, which is the one thing a plan must never do.

const indexPrefix = "sql:idx:"

// indexDef is one index in a table's schema.
type indexDef struct {
	Name   string `json:"name"`
	Column string `json:"column"`
}

// valueEnd follows every encoded value in an index key. valueEnd with its
// last byte raised to 0x02 is the first key past all of a value's entries.
const valueEnd = "\x00\x01"

// indexKeys returns where an index's entries start and end: every entry of
// the index is >= from and < to (';' is the byte after ':').
func indexKeys(table, name string) (from, to string) {
	base := indexPrefix + table + ":" + name
	return base + ":", base + ";"
}

// indexValue encodes v for an index key so that key order is value order.
// v must already have the column's type (see coerce), and must not be NULL.
func indexValue(v any) string {
	switch x := v.(type) {
	case int64:
		return fmt.Sprintf("%016x", uint64(x)^(1<<63))
	case float64:
		if x == 0 {
			x = 0 // -0.0 equals 0.0, so they must share entries
		}
		bits := math.Float64bits(x)
		if bits>>63 == 1 {
			bits = ^bits // negative: bigger magnitude is smaller, so flip it all
		} else {
			bits |= 1 << 63 // positive: just above every negative
		}
		return fmt.Sprintf("%016x", bits)
	case bool:
		if x {
			return "1"
		}
		return "0"
	case string:
		return strings.ReplaceAll(x, "\x00", "\x00\xff")
	}
	panic(fmt.Sprintf("indexValue: unexpected %T", v))
}

// indexValueStart is the first key that could hold v in the index, and
// indexValueAfter the first key past every entry for v.
func indexValueStart(table, name string, v any) string {
	from, _ := indexKeys(table, name)
	return from + indexValue(v) + valueEnd
}

func indexValueAfter(table, name string, v any) string {
	k := indexValueStart(table, name, v)
	return k[:len(k)-1] + "\x02"
}

// indexEntry is the index key for the row stored under rowKey with value v
// in the indexed column. The row's own key goes last, which keeps entries
// for equal values apart and in primary key order.
func indexEntry(table, name string, v any, rowKey string) string {
	return indexValueStart(table, name, v) + strings.TrimPrefix(rowKey, rowPrefix+table+":")
}

// indexEntries lists the index keys a row needs, one per index whose column
// is not NULL in this row.
func indexEntries(table string, s *schema, key string, row []any) []string {
	var out []string
	for _, ix := range s.Indexes {
		if v := row[s.index(ix.Column)]; v != nil {
			out = append(out, indexEntry(table, ix.Name, v, key))
		}
	}
	return out
}

// addIndexEntries writes the entries for a row that is about to be written.
func (e *Executor) addIndexEntries(table string, s *schema, key string, row []any) error {
	for _, k := range indexEntries(table, s, key, row) {
		if err := e.st.Set(k, key); err != nil {
			return err
		}
	}
	return nil
}

// removeIndexEntries deletes the entries for a row that is gone. keep lists
// entries the row still needs (after an UPDATE), which are left alone.
func (e *Executor) removeIndexEntries(table string, s *schema, key string, row []any, keep []string) error {
	for _, k := range indexEntries(table, s, key, row) {
		if contains(keep, k) {
			continue
		}
		if _, err := e.st.Delete(k); err != nil {
			return err
		}
	}
	return nil
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

func (e *Executor) saveSchema(table string, s *schema) error {
	raw, _ := json.Marshal(s)
	return e.st.Set(schemaPrefix+table, string(raw))
}

// findIndex returns the table that has the index called name, or "".
func (e *Executor) findIndex(name string) (string, *schema, error) {
	pairs, _ := e.st.Prefix(schemaPrefix, 0)
	for _, kv := range pairs {
		table := strings.TrimPrefix(kv.Key, schemaPrefix)
		s, err := e.loadSchema(table)
		if err != nil {
			return "", nil, err
		}
		for _, ix := range s.Indexes {
			if ix.Name == name {
				return table, s, nil
			}
		}
	}
	return "", nil, nil
}

func (e *Executor) createIndex(c CreateIndex) (Result, error) {
	s, err := e.loadSchema(c.Table)
	if err != nil {
		return Result{}, err
	}
	col := s.index(c.Column)
	if col < 0 {
		return Result{}, fmt.Errorf("table %s has no column %s", c.Table, c.Column)
	}
	if s.Columns[col].PK {
		return Result{}, fmt.Errorf("column %s is the primary key, which is already indexed", c.Column)
	}
	if other, _, err := e.findIndex(c.Name); err != nil {
		return Result{}, err
	} else if other != "" {
		return Result{}, fmt.Errorf("index %s already exists (on table %s)", c.Name, other)
	}
	for _, ix := range s.Indexes {
		if ix.Column == c.Column {
			return Result{}, fmt.Errorf("column %s already has index %s", c.Column, ix.Name)
		}
	}

	// Clear anything left by a CREATE INDEX that crashed before it finished,
	// then write an entry for every row, and only then record the index in
	// the schema, so the planner never uses an index that is half built.
	from, to := indexKeys(c.Table, c.Name)
	old, _ := e.st.Scan(from, to, 0)
	for _, kv := range old {
		if _, err := e.st.Delete(kv.Key); err != nil {
			return Result{}, err
		}
	}
	rows, _ := e.st.Prefix(rowPrefix+c.Table+":", 0)
	ix := indexDef{Name: c.Name, Column: c.Column}
	n := 0
	for _, kv := range rows {
		vals, err := decodeRow(kv.Value, s)
		if err != nil {
			return Result{}, fmt.Errorf("table %s: %v", c.Table, err)
		}
		if v := vals[col]; v != nil {
			if err := e.st.Set(indexEntry(c.Table, ix.Name, v, kv.Key), kv.Key); err != nil {
				return Result{}, err
			}
			n++
		}
	}
	s.Indexes = append(s.Indexes, ix)
	if err := e.saveSchema(c.Table, s); err != nil {
		return Result{}, err
	}
	entries := "entries"
	if n == 1 {
		entries = "entry"
	}
	return Result{Message: fmt.Sprintf("OK, index %s created on %s (%s) with %d %s", c.Name, c.Table, c.Column, n, entries)}, nil
}

func (e *Executor) dropIndex(d DropIndex) (Result, error) {
	table, s, err := e.findIndex(d.Name)
	if err != nil {
		return Result{}, err
	}
	if table == "" {
		return Result{}, fmt.Errorf("index %s does not exist", d.Name)
	}
	// Out of the schema first, so nothing uses the index while its entries
	// are being deleted.
	kept := s.Indexes[:0]
	for _, ix := range s.Indexes {
		if ix.Name != d.Name {
			kept = append(kept, ix)
		}
	}
	s.Indexes = kept
	if err := e.saveSchema(table, s); err != nil {
		return Result{}, err
	}
	from, to := indexKeys(table, d.Name)
	entries, _ := e.st.Scan(from, to, 0)
	for _, kv := range entries {
		if _, err := e.st.Delete(kv.Key); err != nil {
			return Result{}, err
		}
	}
	return Result{Message: fmt.Sprintf("OK, index %s dropped", d.Name)}, nil
}

func (e *Executor) showIndexes() (Result, error) {
	pairs, _ := e.st.Prefix(schemaPrefix, 0)
	res := Result{Columns: []string{"table", "index", "column"}}
	for _, kv := range pairs {
		table := strings.TrimPrefix(kv.Key, schemaPrefix)
		s, err := e.loadSchema(table)
		if err != nil {
			return Result{}, err
		}
		ixs := append([]indexDef(nil), s.Indexes...)
		sort.Slice(ixs, func(a, b int) bool { return ixs[a].Name < ixs[b].Name })
		for _, ix := range ixs {
			res.Rows = append(res.Rows, []any{table, ix.Name, ix.Column})
		}
	}
	return res, nil
}

// indexRows reads the rows an index plan points at: it walks the index
// entries for each value (or the whole range), collects the row keys they
// hold, and GETs each row. The keys are sorted first, so rows come back in
// primary key order, the same order as any other plan, and an entry whose
// row is gone is skipped.
func (e *Executor) indexRows(a access) []store.KV {
	var entries []store.KV
	switch a.kind {
	case keyLookup:
		for i := range a.keys {
			kvs, _ := e.st.Scan(a.keys[i], a.ends[i], 0)
			entries = append(entries, kvs...)
		}
	case rangeScan:
		entries, _ = e.st.Scan(a.from, a.to, 0)
	}
	keys := make([]string, len(entries))
	for i, kv := range entries {
		keys[i] = kv.Value
	}
	sort.Strings(keys)
	var out []store.KV
	for i, key := range keys {
		if i > 0 && key == keys[i-1] {
			continue // only an entry left behind by a crash can repeat a row
		}
		if raw, err := e.st.Get(key); err == nil {
			out = append(out, store.KV{Key: key, Value: raw})
		}
	}
	return out
}
