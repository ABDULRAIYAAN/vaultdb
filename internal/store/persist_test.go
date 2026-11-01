package store

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func mustOpen(t *testing.T, path string) (*Store, OpenInfo) {
	t.Helper()
	s, info, err := Open(path, true)
	if err != nil {
		t.Fatal(err)
	}
	return s, info
}

// The core promise of milestone 2: data survives closing and reopening.
func TestDataSurvivesRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "data", "vaultdb.log")

	s, info := mustOpen(t, path)
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

	s2, info := mustOpen(t, path)
	defer s2.Close()

	if info.Records != 5 {
		t.Errorf("replayed %d records, want 5 (4 sets + 1 delete)", info.Records)
	}
	if got, _ := s2.Get("name"); got != "Abdul Raiyaan" {
		t.Errorf("name = %q after restart", got)
	}
	if got, _ := s2.Get("uni"); got != "Taylor's" {
		t.Errorf("uni = %q after restart", got)
	}
	if s2.Exists("temp") {
		t.Error("deleted key came back after restart")
	}
	if s2.Len() != 2 || info.Keys != 2 {
		t.Errorf("Len = %d, info.Keys = %d, want 2", s2.Len(), info.Keys)
	}
}

func TestClearSurvivesRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "vaultdb.log")
	s, _ := mustOpen(t, path)
	s.Set("a", "1")
	s.Set("b", "2")
	s.Clear()
	s.Set("c", "3")
	s.Close()

	s2, _ := mustOpen(t, path)
	defer s2.Close()
	if fmt.Sprint(s2.Keys()) != "[c]" {
		t.Fatalf("Keys = %v, want [c]", s2.Keys())
	}
}

// Crash in the middle of a write: earlier data must still load.
func TestRecoversFromTornWrite(t *testing.T) {
	path := filepath.Join(t.TempDir(), "vaultdb.log")
	s, _ := mustOpen(t, path)
	s.Set("safe", "yes")
	good := s.LogSize()
	s.Set("lost", "this write is interrupted")
	s.Close()
	os.Truncate(path, good+5)

	s2, info := mustOpen(t, path)
	defer s2.Close()
	if info.TornBytes != 5 {
		t.Errorf("TornBytes = %d, want 5", info.TornBytes)
	}
	if v, _ := s2.Get("safe"); v != "yes" {
		t.Errorf("safe = %q", v)
	}
	if s2.Exists("lost") {
		t.Error("half-written key should not exist")
	}
}

// Once the log is closed, writes must fail and leave memory unchanged.
func TestWriteAheadRule(t *testing.T) {
	path := filepath.Join(t.TempDir(), "vaultdb.log")
	s, _ := mustOpen(t, path)
	s.Set("k", "before")
	s.log.Close() // simulate the disk going away

	if err := s.Set("k", "after"); err == nil {
		t.Fatal("Set succeeded with closed log")
	}
	if v, _ := s.Get("k"); v != "before" {
		t.Fatalf("memory changed to %q even though the disk write failed", v)
	}
}

func TestInMemoryStoreIsNotPersistent(t *testing.T) {
	s := New()
	if s.Persistent() || s.LogSize() != 0 || s.Close() != nil {
		t.Fatal("in-memory store should report no persistence")
	}
}
