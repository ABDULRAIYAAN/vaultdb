package store

import (
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ABDULRAIYAAN/vaultdb/internal/aol"
)

func mustOpen(t *testing.T, path string, opts Options) (*Store, OpenInfo) {
	t.Helper()
	s, info, err := Open(path, opts)
	if err != nil {
		t.Fatal(err)
	}
	return s, info
}

func manualOpts() Options { return Options{Sync: true} } // no auto-compaction

func fileSize(t *testing.T, path string) int64 {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return info.Size()
}

// The core promise of milestone 2: data survives closing and reopening.
func TestDataSurvivesRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "data", "vaultdb.log")

	s, info := mustOpen(t, path, manualOpts())
	if info.Keys != 0 {
		t.Fatalf("new database has %d keys", info.Keys)
	}
	s.Set("name", "raiyaan")
	s.Set("uni", "Taylor's")
	s.Set("temp", "x")
	s.Set("name", "Abdul Raiyaan") // overwrite
	s.Delete("temp")
	s.Delete("never-existed") // must not be logged
	s.Close()

	s2, info := mustOpen(t, path, manualOpts())
	defer s2.Close()
	if info.Records != 5 {
		t.Errorf("replayed %d records, want 5 (4 sets + 1 delete)", info.Records)
	}
	if got, _ := s2.Get("name"); got != "Abdul Raiyaan" {
		t.Errorf("name = %q after restart", got)
	}
	if s2.Exists("temp") || s2.Len() != 2 {
		t.Errorf("keys after restart: %v", s2.Keys())
	}
}

func TestClearSurvivesRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "vaultdb.log")
	s, _ := mustOpen(t, path, manualOpts())
	s.Set("a", "1")
	s.Set("b", "2")
	s.Clear()
	s.Set("c", "3")
	s.Close()

	s2, _ := mustOpen(t, path, manualOpts())
	defer s2.Close()
	if fmt.Sprint(s2.Keys()) != "[c]" {
		t.Fatalf("Keys = %v, want [c]", s2.Keys())
	}
}

// Crash in the middle of a write: earlier data must still load.
func TestRecoversFromTornWrite(t *testing.T) {
	path := filepath.Join(t.TempDir(), "vaultdb.log")
	s, _ := mustOpen(t, path, manualOpts())
	s.Set("safe", "yes")
	good := s.Stats().LogBytes
	s.Set("lost", "this write is interrupted")
	s.Close()
	os.Truncate(path, good+5)

	s2, info := mustOpen(t, path, manualOpts())
	defer s2.Close()
	if info.TornBytes != 5 {
		t.Errorf("TornBytes = %d, want 5", info.TornBytes)
	}
	if v, _ := s2.Get("safe"); v != "yes" || s2.Exists("lost") {
		t.Errorf("after recovery: safe=%q, keys=%v", v, s2.Keys())
	}
}

func TestCorruptLogRefusesThenRepairs(t *testing.T) {
	path := filepath.Join(t.TempDir(), "vaultdb.log")
	s, _ := mustOpen(t, path, manualOpts())
	s.Set("a", "1")
	damageAt := s.Stats().LogBytes + 17 // first data byte of record b
	s.Set("b", "2")
	s.Set("c", "3")
	s.Close()

	data, _ := os.ReadFile(path)
	data[damageAt] ^= 0xFF
	os.WriteFile(path, data, 0o644)

	if _, _, err := Open(path, manualOpts()); !errors.Is(err, aol.ErrCorrupt) {
		t.Fatalf("Open on corrupt log: %v, want ErrCorrupt", err)
	}
	opts := manualOpts()
	opts.Repair = true
	s2, info := mustOpen(t, path, opts)
	defer s2.Close()
	if info.RepairedBytes == 0 || fmt.Sprint(s2.Keys()) != "[a]" {
		t.Fatalf("after repair: info %+v, keys %v", info, s2.Keys())
	}
}

// Once the log is closed, writes must fail and leave memory unchanged.
func TestWriteAheadRule(t *testing.T) {
	path := filepath.Join(t.TempDir(), "vaultdb.log")
	s, _ := mustOpen(t, path, manualOpts())
	s.Set("k", "before")
	s.log.Close() // simulate the disk going away

	if err := s.Set("k", "after"); err == nil {
		t.Fatal("Set succeeded with closed log")
	}
	if v, _ := s.Get("k"); v != "before" {
		t.Fatalf("memory changed to %q even though the disk write failed", v)
	}
}

func TestCompactShrinksLogAndKeepsData(t *testing.T) {
	path := filepath.Join(t.TempDir(), "vaultdb.log")
	s, _ := mustOpen(t, path, manualOpts())
	for i := 0; i < 500; i++ {
		s.Set("counter", fmt.Sprint(i))
	}
	s.Set("name", "raiyaan")
	s.Set("temp", "gone soon")
	s.Delete("temp")

	st := s.Stats()
	res, err := s.Compact()
	if err != nil {
		t.Fatal(err)
	}
	if res.BytesBefore != st.LogBytes || res.BytesAfter != st.LiveBytes {
		t.Fatalf("compact %+v, stats before %+v", res, st)
	}
	if res.BytesAfter != fileSize(t, path) || res.BytesAfter*10 > res.BytesBefore {
		t.Fatalf("log only shrank from %d to %d", res.BytesBefore, res.BytesAfter)
	}

	s.Set("after", "compaction") // writes still work
	s.Close()

	s2, info := mustOpen(t, path, manualOpts())
	defer s2.Close()
	if info.Records != 3 {
		t.Errorf("replayed %d records, want 3", info.Records)
	}
	if v, _ := s2.Get("counter"); v != "499" {
		t.Errorf("counter = %q, want 499", v)
	}
	if fmt.Sprint(s2.Keys()) != "[after counter name]" {
		t.Errorf("Keys = %v", s2.Keys())
	}
}

// liveBytes must equal exactly what a compacted log weighs.
func TestLiveBytesIsExact(t *testing.T) {
	path := filepath.Join(t.TempDir(), "vaultdb.log")
	s, _ := mustOpen(t, path, manualOpts())
	defer s.Close()
	s.Set("a", "short")
	s.Set("a", "a much longer value than before")
	s.Set("b", "x")
	s.Delete("b")
	s.Set("c", "")
	want := s.Stats().LiveBytes
	s.Compact()
	if got := fileSize(t, path); got != want {
		t.Fatalf("LiveBytes predicted %d, compacted file is %d", want, got)
	}
}

func TestAutoCompaction(t *testing.T) {
	path := filepath.Join(t.TempDir(), "vaultdb.log")
	opts := Options{Sync: false, AutoCompact: true, CompactMinBytes: 4096}
	s, _ := mustOpen(t, path, opts)
	defer s.Close()

	value := strings.Repeat("v", 100)
	for i := 0; i < 1000; i++ {
		s.Set("same-key", value) // ~120 bytes of garbage per write
	}
	st := s.Stats()
	if st.Compactions == 0 {
		t.Fatal("auto-compaction never ran")
	}
	if st.LastCompactErr != nil {
		t.Fatal(st.LastCompactErr)
	}
	if st.LogBytes > opts.CompactMinBytes+st.LiveBytes {
		t.Fatalf("log is %d bytes, expected it to stay near %d", st.LogBytes, opts.CompactMinBytes)
	}
	if v, _ := s.Get("same-key"); v != value {
		t.Fatal("value lost during auto-compaction")
	}
}

// Live data that is genuinely large must not trigger compaction loops.
func TestNoAutoCompactionWithoutGarbage(t *testing.T) {
	path := filepath.Join(t.TempDir(), "vaultdb.log")
	s, _ := mustOpen(t, path, Options{AutoCompact: true, CompactMinBytes: 1024})
	defer s.Close()
	for i := 0; i < 200; i++ {
		s.Set(fmt.Sprintf("key%03d", i), "unique value")
	}
	if n := s.Stats().Compactions; n != 0 {
		t.Fatalf("compacted %d times with no garbage", n)
	}
}

// A log from VaultDB 0.2 (no header, no checksums) is upgraded on open.
func TestUpgradesOldFormatLog(t *testing.T) {
	path := filepath.Join(t.TempDir(), "vaultdb.log")
	var old []byte
	for _, r := range []struct {
		op   byte
		k, v string
	}{{1, "name", "raiyaan"}, {1, "bio", "CS student"}, {1, "tmp", "x"}, {2, "tmp", ""}} {
		h := make([]byte, 9)
		h[0] = r.op
		binary.BigEndian.PutUint32(h[1:5], uint32(len(r.k)))
		binary.BigEndian.PutUint32(h[5:9], uint32(len(r.v)))
		old = append(append(append(old, h...), r.k...), r.v...)
	}
	os.WriteFile(path, old, 0o644)

	s, info := mustOpen(t, path, DefaultOptions())
	if !info.Upgraded || info.Keys != 2 {
		t.Fatalf("info %+v", info)
	}
	s.Set("new", "write")
	s.Close()

	s2, info := mustOpen(t, path, DefaultOptions())
	defer s2.Close()
	if info.Upgraded || fmt.Sprint(s2.Keys()) != "[bio name new]" {
		t.Fatalf("second open: info %+v keys %v", info, s2.Keys())
	}
}

func TestInMemoryStore(t *testing.T) {
	s := New()
	if st := s.Stats(); st.Persistent || st.LogBytes != 0 {
		t.Fatalf("in-memory stats %+v", st)
	}
	if _, err := s.Compact(); err == nil {
		t.Fatal("Compact on in-memory store should fail")
	}
	if s.Close() != nil {
		t.Fatal("Close on in-memory store should be a no-op")
	}
}
