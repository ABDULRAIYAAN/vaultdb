package aol

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func openReplay(t *testing.T, path string) (*Log, []Record, ReplayResult) {
	t.Helper()
	l, err := Open(path, true)
	if err != nil {
		t.Fatal(err)
	}
	var got []Record
	res, err := l.Replay(func(r Record) { got = append(got, r) })
	if err != nil {
		t.Fatal(err)
	}
	return l, got, res
}

func TestRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", "test.log") // also checks dir creation
	want := []Record{
		{Op: OpSet, Key: "name", Value: "raiyaan"},
		{Op: OpSet, Key: "bio", Value: "CS student, Taylor's"},
		{Op: OpSet, Key: "empty", Value: ""},
		{Op: OpSet, Key: "unicode", Value: "سلام 你好"},
		{Op: OpDelete, Key: "name"},
		{Op: OpClear},
	}

	l, got, _ := openReplay(t, path)
	if len(got) != 0 {
		t.Fatalf("new log replayed %d records", len(got))
	}
	for _, r := range want {
		if err := l.Append(r); err != nil {
			t.Fatal(err)
		}
	}
	l.Close()

	l2, got, res := openReplay(t, path)
	defer l2.Close()
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("replayed\n%v\nwant\n%v", got, want)
	}
	if res.Records != len(want) || res.TornBytes != 0 {
		t.Fatalf("result = %+v", res)
	}
	info, _ := os.Stat(path)
	if l2.Size() != info.Size() {
		t.Fatalf("Size() = %d, file is %d", l2.Size(), info.Size())
	}
}

// Simulates a power cut part way through writing the last record.
func TestTornWriteIsTruncated(t *testing.T) {
	path := filepath.Join(t.TempDir(), "torn.log")
	l, _, _ := openReplay(t, path)
	l.Append(Record{Op: OpSet, Key: "a", Value: "1"})
	l.Append(Record{Op: OpSet, Key: "b", Value: "2"})
	goodSize := l.Size()
	l.Append(Record{Op: OpSet, Key: "c", Value: "a value that will be cut off"})
	l.Close()

	// Chop the file part way into the third record (inside the body).
	if err := os.Truncate(path, goodSize+headerSize+3); err != nil {
		t.Fatal(err)
	}

	l2, got, res := openReplay(t, path)
	if len(got) != 2 || got[1].Key != "b" {
		t.Fatalf("replayed %v, want records a and b", got)
	}
	if res.TornBytes != headerSize+3 {
		t.Fatalf("TornBytes = %d, want %d", res.TornBytes, headerSize+3)
	}
	// New writes must land cleanly after the good data.
	l2.Append(Record{Op: OpSet, Key: "d", Value: "4"})
	l2.Close()

	l3, got, _ := openReplay(t, path)
	defer l3.Close()
	if len(got) != 3 || got[2].Key != "d" {
		t.Fatalf("after recovery replayed %v", got)
	}
}

// A cut inside the 9-byte header is also a torn write.
func TestTornHeader(t *testing.T) {
	path := filepath.Join(t.TempDir(), "torn.log")
	l, _, _ := openReplay(t, path)
	l.Append(Record{Op: OpSet, Key: "a", Value: "1"})
	good := l.Size()
	l.Append(Record{Op: OpSet, Key: "b", Value: "2"})
	l.Close()
	os.Truncate(path, good+4)

	l2, got, res := openReplay(t, path)
	defer l2.Close()
	if len(got) != 1 || res.TornBytes != 4 {
		t.Fatalf("got %v, result %+v", got, res)
	}
}

func TestGarbageIsCorrupt(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bad.log")
	os.WriteFile(path, []byte{99, 0, 0, 0, 1, 0, 0, 0, 1, 'k', 'v'}, 0o644)
	l, err := Open(path, true)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	_, err = l.Replay(func(Record) {})
	if !errors.Is(err, ErrCorrupt) {
		t.Fatalf("err = %v, want ErrCorrupt", err)
	}
}

func TestAppendAfterClose(t *testing.T) {
	l, _, _ := openReplay(t, filepath.Join(t.TempDir(), "x.log"))
	l.Close()
	if err := l.Append(Record{Op: OpSet, Key: "k"}); err == nil {
		t.Fatal("expected error appending to closed log")
	}
	if err := l.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
}
