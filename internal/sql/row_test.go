package sql

import (
	"math"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/ABDULRAIYAAN/vaultdb/internal/store"
)

func rowSchema(types ...Type) *schema {
	s := &schema{}
	for i, t := range types {
		s.Columns = append(s.Columns, column{Name: string(rune('a' + i)), Type: t})
	}
	return s
}

// sameRow is reflect.DeepEqual, except that floats are compared by their
// bits, so NaN equals NaN and -0.0 does not equal 0.0.
func sameRow(a, b []any) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		fa, okA := a[i].(float64)
		fb, okB := b[i].(float64)
		if okA && okB {
			if math.Float64bits(fa) != math.Float64bits(fb) {
				return false
			}
		} else if !reflect.DeepEqual(a[i], b[i]) {
			return false
		}
	}
	return true
}

func TestRowRoundTrip(t *testing.T) {
	cases := []struct {
		types []Type
		row   []any
	}{
		{[]Type{TypeInt, TypeText, TypeFloat, TypeBool}, []any{int64(1), "Ali", 3.5, true}},
		{[]Type{TypeInt, TypeText, TypeFloat, TypeBool}, []any{nil, nil, nil, nil}},
		{[]Type{TypeBool, TypeBool}, []any{false, true}},
		{[]Type{TypeInt, TypeInt, TypeInt, TypeInt, TypeInt},
			[]any{int64(0), int64(-1), int64(63), int64(math.MinInt64), int64(math.MaxInt64)}},
		{[]Type{TypeFloat, TypeFloat, TypeFloat, TypeFloat, TypeFloat},
			[]any{math.Copysign(0, -1), math.Inf(1), math.Inf(-1), math.NaN(), math.SmallestNonzeroFloat64}},
		{[]Type{TypeText, TypeText, TypeText, TypeText},
			[]any{"", "héllo, 世界", strings.Repeat("x", 300), "\xff\xfe not utf-8 \x00"}},
		{nil, []any{}},
	}
	for _, c := range cases {
		raw, err := encodeRow(c.row)
		if err != nil {
			t.Fatalf("encode %v: %v", c.row, err)
		}
		if raw[0] != rowFormatBinary {
			t.Errorf("encode %v: first byte %#x, want %#x", c.row, raw[0], rowFormatBinary)
		}
		got, err := decodeRow(raw, rowSchema(c.types...))
		if err != nil {
			t.Fatalf("decode %v: %v", c.row, err)
		}
		if !sameRow(got, c.row) {
			t.Errorf("round trip changed the row:\n  got  %#v\n  want %#v", got, c.row)
		}
	}
}

// The layout is part of the file format, so pin it down byte by byte.
func TestRowLayout(t *testing.T) {
	raw, _ := encodeRow([]any{int64(-2), "Raiyaan", 3.9, true, nil})
	want := "\x01\x05" + // format, 5 values
		"\x03\x03" + // INT -2 as a zig-zag varint
		"\x05\x07Raiyaan" + // TEXT, 7 bytes
		"\x04\x33\x33\x33\x33\x33\x33\x0f\x40" + // FLOAT 3.9, little-endian bits
		"\x02" + // TRUE
		"\x00" // NULL
	if raw != want {
		t.Errorf("layout changed:\n  got  %q\n  want %q", raw, want)
	}
}

func TestEncodeRejectsUnknownTypes(t *testing.T) {
	if _, err := encodeRow([]any{int64(1), []int{2}}); err == nil {
		t.Error("encoding a []int should fail")
	}
}

func TestDecodeRejectsDamagedRows(t *testing.T) {
	s := rowSchema(TypeInt, TypeText, TypeFloat, TypeBool)
	good, _ := encodeRow([]any{int64(300), "Ali", 3.5, true})

	// Every cut-off version of a good row must be rejected, never misread.
	for n := 0; n < len(good); n++ {
		if vals, err := decodeRow(good[:n], s); err == nil {
			t.Errorf("row cut to %d of %d bytes decoded as %v", n, len(good), vals)
		}
	}

	bad := map[string]string{
		"trailing byte":       good + "\x00",
		"too few columns":     "\x01\x03" + good[2:],
		"too many columns":    "\x01\x05" + good[2:] + "\x00",
		"unknown tag":         "\x01\x04\x09",
		"text in INT column":  "\x01\x04\x05\x01x\x05\x00\x00\x02",
		"bool in TEXT column": "\x01\x04\x00\x02\x00\x02",
		"text runs past end":  "\x01\x04\x00\x05\x7fAli\x00\x02",
		"varint over 64 bits": "\x01\x04\x03\xff\xff\xff\xff\xff\xff\xff\xff\xff\x02\x00\x00\x00\x00",
		"empty":               "",
		"json, wrong count":   `[1,"Ali"]`,
		"json, broken":        `[1,"Ali",3.5,tr`,
	}
	for name, raw := range bad {
		if vals, err := decodeRow(raw, s); err == nil {
			t.Errorf("%s: decoded as %v, want an error", name, vals)
		}
	}
}

// Tables written by v1.0.0 and earlier hold JSON rows. They must still be
// readable, and an UPDATE must rewrite them in the new format.
func TestOldJSONRowsStillWork(t *testing.T) {
	st := store.New()
	e := NewExecutor(st)
	mustExec(t, e, `CREATE TABLE students (id INT PRIMARY KEY, name TEXT, gpa FLOAT, active BOOL)`)
	mustExec(t, e, `INSERT INTO students VALUES (1, 'Ali', 3.2, TRUE)`)
	// Rows 2 and 3 as v1.0.0 wrote them, next to row 1 in the new format.
	st.Set(rowKey("students", int64(2)), `[2,"Sara",4,false]`)
	st.Set(rowKey("students", int64(3)), `[3,"Old",null,null]`)

	want := [][]any{{int64(1), "Ali", 3.2, true}, {int64(2), "Sara", 4.0, false}, {int64(3), "Old", nil, nil}}
	if got := rows(t, e, `SELECT * FROM students`); !reflect.DeepEqual(got, want) {
		t.Fatalf("mixed formats: got %v, want %v", got, want)
	}

	mustExec(t, e, `UPDATE students SET gpa = 3.7 WHERE id = 2`)
	raw, _ := st.Get(rowKey("students", int64(2)))
	if raw[0] != rowFormatBinary {
		t.Errorf("updated row is still JSON: %q", raw)
	}
	if got := rows(t, e, `SELECT name, gpa FROM students WHERE id = 2`); !reflect.DeepEqual(got, [][]any{{"Sara", 3.7}}) {
		t.Errorf("after update: got %v", got)
	}
	// The row nobody updated is left exactly as it was.
	if raw, _ := st.Get(rowKey("students", int64(3))); raw != `[3,"Old",null,null]` {
		t.Errorf("untouched row was rewritten: %q", raw)
	}
}

// JSON can only hold valid UTF-8, so the old format quietly turned bad bytes
// into U+FFFD. The binary format keeps text exactly, also across a restart.
func TestTextIsStoredExactly(t *testing.T) {
	path := filepath.Join(t.TempDir(), "vaultdb.log")
	st, _, err := store.Open(path, store.DefaultOptions())
	if err != nil {
		t.Fatal(err)
	}
	e := NewExecutor(st)
	mustExec(t, e, `CREATE TABLE notes (id INT PRIMARY KEY, body TEXT)`)
	mustExec(t, e, "INSERT INTO notes VALUES (1, 'caf\xe9 \x00 end')")
	st.Close()

	st2, _, err := store.Open(path, store.DefaultOptions())
	if err != nil {
		t.Fatal(err)
	}
	defer st2.Close()
	got := rows(t, NewExecutor(st2), `SELECT body FROM notes`)
	if want := [][]any{{"caf\xe9 \x00 end"}}; !reflect.DeepEqual(got, want) {
		t.Errorf("got %q, want %q", got, want)
	}
}

// FuzzDecodeRow feeds random bytes to the decoder. It must never panic, and
// any binary row it accepts must survive another encode and decode unchanged.
func FuzzDecodeRow(f *testing.F) {
	seed, _ := encodeRow([]any{int64(-7), "Ali", 2.5, false})
	f.Add(seed)
	f.Add(`[1,"Ali",2.5,false]`)
	f.Add("\x01\x04\x00\x00\x00\x00")
	s := rowSchema(TypeInt, TypeText, TypeFloat, TypeBool)
	f.Fuzz(func(t *testing.T, raw string) {
		vals, err := decodeRow(raw, s)
		if err != nil || raw == "" || raw[0] != rowFormatBinary {
			return
		}
		again, err := encodeRow(vals)
		if err != nil {
			t.Fatalf("decoded %q to %#v, which does not encode: %v", raw, vals, err)
		}
		back, err := decodeRow(again, s)
		if err != nil || !sameRow(back, vals) {
			t.Fatalf("%q -> %#v -> %q -> %#v (%v)", raw, vals, again, back, err)
		}
	})
}
