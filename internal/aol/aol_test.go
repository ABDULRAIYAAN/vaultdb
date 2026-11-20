package aol

import (
	"encoding/binary"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func openReplay(t *testing.T, path string, opts ReplayOptions) (*Log, []Record, ReplayResult) {
	t.Helper()
	l, err := Open(path, true)
	if err != nil {
		t.Fatal(err)
	}
	var got []Record
	res, err := l.Replay(func(r Record) { got = append(got, r) }, opts)
	if err != nil {
		l.Close()
		t.Fatal(err)
	}
	return l, got, res
}

func writeRecords(t *testing.T, path string, recs ...Record) (offsets []int64) {
	t.Helper()
	l, _, _ := openReplay(t, path, ReplayOptions{})
	defer l.Close()
	for _, r := range recs {
		offsets = append(offsets, l.Size())
		if err := l.Append(r); err != nil {
			t.Fatal(err)
		}
	}
	return offsets
}

func flipByte(t *testing.T, path string, at int64) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	data[at] ^= 0xFF
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
}

func set(k, v string) Record { return Record{Op: OpSet, Key: k, Value: v} }

func TestRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", "test.log") // also checks folder creation
	want := []Record{
		set("name", "raiyaan"),
		set("bio", "CS student, Taylor's"),
		set("empty", ""),
		set("unicode", "سلام 你好"),
		{Op: OpExpire, Key: "bio", Value: "1700000000000"},
		{Op: OpExpire, Key: "bio"}, // PERSIST
		{Op: OpSetExpire, Key: "otp", Value: SetExpireValue(1700000060000, "481516")},
		{Op: OpDelete, Key: "name"},
		{Op: OpClear},
	}
	writeRecords(t, path, want...)

	l, got, res := openReplay(t, path, ReplayOptions{})
	defer l.Close()
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("replayed\n%v\nwant\n%v", got, want)
	}
	if res.Records != len(want) || res.TornBytes != 0 || res.Legacy {
		t.Fatalf("result = %+v", res)
	}
	info, _ := os.Stat(path)
	if l.Size() != info.Size() {
		t.Fatalf("Size() = %d, file is %d", l.Size(), info.Size())
	}
}

func TestNewFileGetsHeader(t *testing.T) {
	path := filepath.Join(t.TempDir(), "new.log")
	l, _, _ := openReplay(t, path, ReplayOptions{})
	l.Close()
	data, _ := os.ReadFile(path)
	if string(data) != magic+"\x02" {
		t.Fatalf("new file contents = %q", data)
	}
}

// Power cut part way through the last record's data.
func TestTornBody(t *testing.T) {
	path := filepath.Join(t.TempDir(), "torn.log")
	off := writeRecords(t, path, set("a", "1"), set("b", "2"), set("c", "a value that gets cut off"))
	os.Truncate(path, off[2]+recordHeaderSize+3)

	l, got, res := openReplay(t, path, ReplayOptions{})
	if len(got) != 2 || res.TornBytes != recordHeaderSize+3 {
		t.Fatalf("got %v, result %+v", got, res)
	}
	l.Append(set("d", "4")) // new writes land cleanly after the good data
	l.Close()

	l2, got, _ := openReplay(t, path, ReplayOptions{})
	defer l2.Close()
	if len(got) != 3 || got[2].Key != "d" {
		t.Fatalf("after recovery replayed %v", got)
	}
}

// Power cut inside the 17-byte record header.
func TestTornHeader(t *testing.T) {
	path := filepath.Join(t.TempDir(), "torn.log")
	off := writeRecords(t, path, set("a", "1"), set("b", "2"))
	os.Truncate(path, off[1]+5)

	l, got, res := openReplay(t, path, ReplayOptions{})
	defer l.Close()
	if len(got) != 1 || res.TornBytes != 5 {
		t.Fatalf("got %v, result %+v", got, res)
	}
}

// Damaged data in the last record is treated like a torn write.
func TestBadChecksumOnLastRecordIsTorn(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tail.log")
	off := writeRecords(t, path, set("a", "1"), set("b", "2"))
	flipByte(t, path, off[1]+recordHeaderSize) // first byte of the last key

	l, got, res := openReplay(t, path, ReplayOptions{})
	defer l.Close()
	if len(got) != 1 || res.TornBytes == 0 {
		t.Fatalf("got %v, result %+v", got, res)
	}
}

// The headline feature: a flipped bit in the middle is caught, not loaded.
func TestBitFlipInMiddleIsDetected(t *testing.T) {
	for name, where := range map[string]int64{
		"in value":    recordHeaderSize + 1, // body byte of record 0
		"in length":   6,                    // keyLen field of record 0
		"in checksum": 1,                    // hdrCRC of record 0
	} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "bad.log")
			off := writeRecords(t, path, set("a", "first"), set("b", "second"), set("c", "third"))
			flipByte(t, path, off[0]+where)

			l, err := Open(path, true)
			if err != nil {
				t.Fatal(err)
			}
			defer l.Close()
			_, err = l.Replay(func(Record) {}, ReplayOptions{})
			if !errors.Is(err, ErrCorrupt) {
				t.Fatalf("err = %v, want ErrCorrupt", err)
			}
			// The damaged file must be left exactly as it was for inspection.
			info, _ := os.Stat(path)
			if info.Size() == off[0] {
				t.Fatal("file was truncated without repair mode")
			}
		})
	}
}

func TestRepairKeepsDataBeforeDamage(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bad.log")
	off := writeRecords(t, path, set("a", "1"), set("b", "2"), set("c", "3"), set("d", "4"))
	flipByte(t, path, off[2]+recordHeaderSize) // damage record c

	l, got, res := openReplay(t, path, ReplayOptions{Repair: true})
	defer l.Close()
	if len(got) != 2 || got[1].Key != "b" {
		t.Fatalf("repair kept %v, want a and b", got)
	}
	if res.RepairedBytes == 0 || l.Size() != off[2] {
		t.Fatalf("result %+v, size %d, want %d", res, l.Size(), off[2])
	}
}

func TestUnknownVersionRefused(t *testing.T) {
	path := filepath.Join(t.TempDir(), "future.log")
	os.WriteFile(path, []byte(magic+"\x09"), 0o644)
	l, _ := Open(path, true)
	defer l.Close()
	if _, err := l.Replay(func(Record) {}, ReplayOptions{}); err == nil {
		t.Fatal("expected error for unknown format version")
	}
}

// writeV1 writes records in the old VaultDB 0.2 format.
func writeV1(t *testing.T, path string, recs ...Record) {
	t.Helper()
	var data []byte
	for _, r := range recs {
		h := make([]byte, v1HeaderSize)
		h[0] = byte(r.Op)
		binary.BigEndian.PutUint32(h[1:5], uint32(len(r.Key)))
		binary.BigEndian.PutUint32(h[5:9], uint32(len(r.Value)))
		data = append(data, h...)
		data = append(data, r.Key...)
		data = append(data, r.Value...)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestLegacyLogIsReadThenRewritten(t *testing.T) {
	path := filepath.Join(t.TempDir(), "old.log")
	writeV1(t, path, set("name", "raiyaan"), set("x", "1"), Record{Op: OpDelete, Key: "x"})

	l, got, res := openReplay(t, path, ReplayOptions{})
	if !res.Legacy || len(got) != 3 {
		t.Fatalf("result %+v, records %v", res, got)
	}
	if err := l.Append(set("y", "2")); !errors.Is(err, ErrNeedsRewrite) {
		t.Fatalf("Append on legacy log: %v, want ErrNeedsRewrite", err)
	}
	err := l.Rewrite(func(emit func(Record) error) error { return emit(set("name", "raiyaan")) })
	if err != nil {
		t.Fatal(err)
	}
	if err := l.Append(set("y", "2")); err != nil {
		t.Fatalf("Append after rewrite: %v", err)
	}
	l.Close()

	l2, got, res := openReplay(t, path, ReplayOptions{})
	defer l2.Close()
	if res.Legacy || len(got) != 2 {
		t.Fatalf("after migration: result %+v, records %v", res, got)
	}
}

// EXPIRE came after format version 1, so an old file holding op 4 is
// damaged, not a deadline.
func TestLegacyLogHasNoExpire(t *testing.T) {
	path := filepath.Join(t.TempDir(), "old.log")
	writeV1(t, path, set("name", "raiyaan"), Record{Op: OpExpire, Key: "name", Value: "1"})
	l, err := Open(path, true)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	if _, err := l.Replay(func(Record) {}, ReplayOptions{}); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("Replay = %v, want ErrCorrupt", err)
	}
}

func TestRewriteReplacesContents(t *testing.T) {
	path := filepath.Join(t.TempDir(), "r.log")
	l, _, _ := openReplay(t, path, ReplayOptions{})
	for i := 0; i < 100; i++ {
		l.Append(set("counter", "value that keeps changing"))
	}
	before := l.Size()
	err := l.Rewrite(func(emit func(Record) error) error {
		return emit(set("counter", "final"))
	})
	if err != nil {
		t.Fatal(err)
	}
	if l.Size() >= before || l.Size() != int64(FileHeaderSize)+RecordSize("counter", "final") {
		t.Fatalf("size %d after rewrite (was %d)", l.Size(), before)
	}
	l.Append(set("next", "1"))
	l.Close()

	if _, err := os.Stat(tempPath(path)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("temp file left behind")
	}
	l2, got, _ := openReplay(t, path, ReplayOptions{})
	defer l2.Close()
	want := []Record{set("counter", "final"), set("next", "1")}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("replayed %v, want %v", got, want)
	}
}

// A crash during compaction leaves a temp file; the real log is untouched.
func TestInterruptedCompactionLeavesOldLog(t *testing.T) {
	path := filepath.Join(t.TempDir(), "c.log")
	writeRecords(t, path, set("a", "1"))
	os.WriteFile(tempPath(path), []byte("half-written compaction"), 0o644)

	l, got, _ := openReplay(t, path, ReplayOptions{})
	defer l.Close()
	if len(got) != 1 {
		t.Fatalf("got %v", got)
	}
	if _, err := os.Stat(tempPath(path)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("stale temp file was not removed")
	}
}

func TestAppendRules(t *testing.T) {
	l, err := Open(filepath.Join(t.TempDir(), "x.log"), true)
	if err != nil {
		t.Fatal(err)
	}
	if err := l.Append(set("k", "v")); err == nil {
		t.Fatal("Append before Replay should fail")
	}
	l.Replay(func(Record) {}, ReplayOptions{})
	l.Close()
	if err := l.Append(set("k", "v")); err == nil {
		t.Fatal("Append after Close should fail")
	}
	if err := l.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
}

func TestSetExpireValue(t *testing.T) {
	for _, c := range []struct {
		at  int64
		val string
	}{{1700000060000, "481516"}, {0, ""}, {-1, "neg"}, {1<<62 + 5, "\x00\xff bytes"}} {
		at, val, ok := SplitSetExpire(SetExpireValue(c.at, c.val))
		if !ok || at != c.at || val != c.val {
			t.Errorf("round trip of (%d, %q) = (%d, %q, %v)", c.at, c.val, at, val, ok)
		}
	}
	if len(SetExpireValue(1, "abc")) != DeadlineSize+3 {
		t.Error("SetExpireValue is not DeadlineSize bytes plus the value")
	}
	if _, _, ok := SplitSetExpire("short"); ok {
		t.Error("SplitSetExpire accepted a value shorter than a deadline")
	}
}

// SETEX came after format version 1 as well.
func TestLegacyLogHasNoSetExpire(t *testing.T) {
	path := filepath.Join(t.TempDir(), "old.log")
	writeV1(t, path, set("name", "raiyaan"), Record{Op: OpSetExpire, Key: "otp", Value: SetExpireValue(1, "x")})
	l, err := Open(path, true)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	if _, err := l.Replay(func(Record) {}, ReplayOptions{}); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("Replay = %v, want ErrCorrupt", err)
	}
}
