package aol

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestBatchValueRoundTrip(t *testing.T) {
	for _, recs := range [][]Record{
		{set("a", "1")},
		{set("a", "1"), set("b", "2"), set("a", "3")},
		{set("empty", ""), {Op: OpDelete, Key: "gone"}, {Op: OpExpire, Key: "a", Value: "1700000000000"}},
		{{Op: OpSetExpire, Key: "otp", Value: SetExpireValue(1, "481516")}, set("unicode", "سلام 你好"), {Op: OpClear}},
		{set("bytes", "\x00\xff\x06\x00\x00\x00\x01")}, // looks like a nested header; it's just a value
	} {
		v, err := BatchValue(recs)
		if err != nil {
			t.Fatalf("BatchValue(%v): %v", recs, err)
		}
		got, err := SplitBatch(v)
		if err != nil || !reflect.DeepEqual(got, recs) {
			t.Errorf("round trip of %v = %v, %v", recs, got, err)
		}
	}
}

func TestBatchValueRefuses(t *testing.T) {
	if _, err := BatchValue(nil); err == nil {
		t.Error("BatchValue accepted an empty batch")
	}
	inner, _ := BatchValue([]Record{set("a", "1")})
	if _, err := BatchValue([]Record{{Op: OpBatch, Value: inner}}); err == nil {
		t.Error("BatchValue accepted a nested batch")
	}
	if _, err := BatchValue([]Record{{Op: 0, Key: "a"}}); err == nil {
		t.Error("BatchValue accepted op 0")
	}
	big := strings.Repeat("x", MaxFieldSize/2)
	if _, err := BatchValue([]Record{set("a", big), set("b", big)}); !errors.Is(err, ErrTooLarge) {
		t.Errorf("two halves plus headers: err = %v, want ErrTooLarge", err)
	}
	// Exactly at the limit is fine.
	exact := strings.Repeat("x", MaxFieldSize-2*batchEntryHeaderSize-2)
	if v, err := BatchValue([]Record{set("a", exact), set("b", "")}); err != nil || len(v) != MaxFieldSize {
		t.Errorf("batch of exactly MaxFieldSize: len %d, err %v", len(v), err)
	}
}

func TestSplitBatchRejectsJunk(t *testing.T) {
	good, _ := BatchValue([]Record{set("a", "1"), set("bb", "22")})
	nested, _ := BatchValue([]Record{set("a", "1")})
	nested = string(OpBatch) + nested[1:]
	for name, v := range map[string]string{
		"empty":          "",
		"short header":   good[:5],
		"cut in body":    good[:len(good)-1],
		"trailing bytes": good + "\x01",
		"op 0":           "\x00" + good[1:],
		"op 7":           "\x07" + good[1:],
		"nested":         nested,
		"huge lengths":   "\x01\xff\xff\xff\xff\xff\xff\xff\xff",
	} {
		if _, err := SplitBatch(v); !errors.Is(err, ErrBadBatch) {
			t.Errorf("%s: err = %v, want ErrBadBatch", name, err)
		}
	}
}

// The whole point: a batch cut short by a crash is dropped as one torn
// record, so none of its changes come back.
func TestTornBatchIsDroppedWhole(t *testing.T) {
	path := filepath.Join(t.TempDir(), "batch.log")
	v, _ := BatchValue([]Record{set("from", "90"), set("to", "10")})
	offsets := writeRecords(t, path, set("before", "ok"), Record{Op: OpBatch, Value: v})

	// Cut anywhere inside the batch record: after its header, in the first
	// change, between the two changes, one byte short of the end.
	full, _ := os.ReadFile(path)
	for _, cut := range []int64{offsets[1] + 3, offsets[1] + recordHeaderSize + 4, int64(len(full)) - 10, int64(len(full)) - 1} {
		if err := os.WriteFile(path, full[:cut], 0o644); err != nil {
			t.Fatal(err)
		}
		l, got, res := openReplay(t, path, ReplayOptions{})
		l.Close()
		if !reflect.DeepEqual(got, []Record{set("before", "ok")}) || res.TornBytes != cut-offsets[1] {
			t.Errorf("cut at %d: replayed %v, torn %d", cut, got, res.TornBytes)
		}
	}

	// And written whole, it comes back as one record.
	os.Remove(path)
	writeRecords(t, path, Record{Op: OpBatch, Value: v})
	l, got, _ := openReplay(t, path, ReplayOptions{})
	l.Close()
	if len(got) != 1 || got[0].Op != OpBatch || got[0].Value != v {
		t.Errorf("whole batch replayed as %v", got)
	}
}

// Batches came long after format version 1.
func TestLegacyLogHasNoBatch(t *testing.T) {
	path := filepath.Join(t.TempDir(), "old.log")
	v, _ := BatchValue([]Record{set("a", "1")})
	writeV1(t, path, set("name", "raiyaan"), Record{Op: OpBatch, Value: v})
	l, err := Open(path, true)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	if _, err := l.Replay(func(Record) {}, ReplayOptions{}); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("Replay = %v, want ErrCorrupt", err)
	}
}

// FuzzSplitBatch checks that SplitBatch never panics on junk, and that
// whatever it does accept packs back into exactly the same bytes, so there
// is only ever one way to read a batch.
func FuzzSplitBatch(f *testing.F) {
	good, _ := BatchValue([]Record{set("a", "1"), {Op: OpDelete, Key: "b"}})
	f.Add(good)
	f.Add("")
	f.Add("\x01\x00\x00\x00\x01\x00\x00\x00\x00k")
	f.Add("\x06\x00\x00\x00\x00\x00\x00\x00\x00")
	f.Fuzz(func(t *testing.T, v string) {
		recs, err := SplitBatch(v)
		if err != nil {
			return
		}
		back, err := BatchValue(recs)
		if err != nil {
			t.Fatalf("SplitBatch accepted %q but BatchValue refuses %v: %v", v, recs, err)
		}
		if back != v {
			t.Fatalf("SplitBatch(%q) = %v, which packs back as %q", v, recs, back)
		}
	})
}
