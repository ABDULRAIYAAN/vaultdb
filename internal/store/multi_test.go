package store

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ABDULRAIYAAN/vaultdb/internal/aol"
)

func kvs(args ...string) []KV {
	var pairs []KV
	for i := 0; i < len(args); i += 2 {
		pairs = append(pairs, KV{args[i], args[i+1]})
	}
	return pairs
}

func TestMSetAndMGet(t *testing.T) {
	c := newClock()
	s := memStore(c)
	if err := s.MSet(kvs("name", "raiyaan", "uni", "Taylor's", "empty", "")); err != nil {
		t.Fatal(err)
	}
	vals, found := s.MGet([]string{"name", "missing", "empty", "uni", "name"})
	if !reflect.DeepEqual(vals, []string{"raiyaan", "", "", "Taylor's", "raiyaan"}) ||
		!reflect.DeepEqual(found, []bool{true, false, true, true, true}) {
		t.Fatalf("MGet = %q, %v", vals, found)
	}

	// The last value for a repeated key wins, as in Redis.
	s.MSet(kvs("dup", "1", "dup", "2", "dup", "3"))
	if v, _ := s.Get("dup"); v != "3" || s.Len() != 4 {
		t.Fatalf("dup = %q, Len = %d", v, s.Len())
	}

	// Like SET, MSET removes an expiry.
	s.SetWith("otp", "1234", SetOptions{TTL: time.Minute})
	s.MSet(kvs("otp", "5678", "other", "x"))
	if _, has, _ := s.TTL("otp"); has {
		t.Fatal("MSET kept the old expiry")
	}

	// MGet doesn't see expired keys.
	s.SetWith("brief", "x", SetOptions{TTL: time.Second})
	c.advance(time.Second)
	if _, found := s.MGet([]string{"brief"}); found[0] {
		t.Fatal("MGet saw an expired key")
	}

	// Nothing to do, nothing done.
	if err := s.MSet(nil); err != nil {
		t.Fatal(err)
	}
	if vals, found := s.MGet(nil); len(vals) != 0 || len(found) != 0 {
		t.Fatal("MGet(nil) returned something")
	}

	// One empty key anywhere stops the whole thing.
	if err := s.MSet(kvs("fine", "1", "", "2")); err != ErrEmptyKey {
		t.Fatalf("MSet with an empty key: err = %v", err)
	}
	if s.Exists("fine") {
		t.Fatal("MSet wrote part of a batch with an empty key")
	}
}

func TestMSetNX(t *testing.T) {
	c := newClock()
	s := memStore(c)
	if ok, err := s.MSetNX(kvs("a", "1", "b", "2")); !ok || err != nil {
		t.Fatalf("MSetNX on new keys = %v, %v", ok, err)
	}
	// b exists, so c isn't written either.
	if ok, err := s.MSetNX(kvs("c", "3", "b", "new")); ok || err != nil {
		t.Fatalf("MSetNX with one existing key = %v, %v", ok, err)
	}
	if v, _ := s.Get("b"); v != "2" || s.Exists("c") {
		t.Fatalf("after refused MSetNX: b=%q, c exists=%v", v, s.Exists("c"))
	}
	// An expired key counts as missing.
	s.Expire("a", time.Second)
	c.advance(time.Second)
	if ok, _ := s.MSetNX(kvs("a", "fresh", "c", "3")); !ok {
		t.Fatal("MSetNX refused because of an expired key")
	}
	if _, has, _ := s.TTL("a"); has {
		t.Fatal("the new a kept the old expiry")
	}
	// A key twice in one MSETNX is fine (neither exists yet); last wins.
	if ok, _ := s.MSetNX(kvs("d", "1", "d", "2")); !ok {
		t.Fatal("MSetNX refused a repeated new key")
	}
	if v, _ := s.Get("d"); v != "2" {
		t.Fatalf("d = %q", v)
	}
	if _, err := s.MSetNX(kvs("", "x")); err != ErrEmptyKey {
		t.Fatalf("MSetNX with an empty key: err = %v", err)
	}
}

// An MSET of several keys is one batch record in the log; of one key, a
// plain SET. Both come back after a restart, and compaction (which writes
// no batches) keeps them, with exact byte counts throughout.
func TestMSetSurvivesRestartAndCompaction(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mset.log")
	s, _ := mustOpen(t, path, manualOpts())
	s.Set("old", "x")
	s.SetWith("ttl", "y", SetOptions{TTL: time.Hour})
	before := s.Stats().LogBytes
	s.MSet(kvs("a", "1", "b", "22", "a", "333", "ttl", "plain"))
	recs := []aol.Record{{Op: aol.OpSet, Key: "a", Value: "1"}, {Op: aol.OpSet, Key: "b", Value: "22"},
		{Op: aol.OpSet, Key: "a", Value: "333"}, {Op: aol.OpSet, Key: "ttl", Value: "plain"}}
	v, _ := aol.BatchValue(recs)
	if grew := s.Stats().LogBytes - before; grew != aol.RecordSize("", v) {
		t.Fatalf("MSET of 4 pairs grew the log by %d bytes, want one batch of %d", grew, aol.RecordSize("", v))
	}
	before = s.Stats().LogBytes
	s.MSet(kvs("solo", "1"))
	if grew := s.Stats().LogBytes - before; grew != aol.RecordSize("solo", "1") {
		t.Fatalf("MSET of 1 pair grew the log by %d, want a plain SET", grew)
	}
	s.MSetNX(kvs("nx1", "p", "nx2", "q"))
	live := s.Stats().LiveBytes
	s.Close()

	want := map[string]string{"old": "x", "a": "333", "b": "22", "ttl": "plain", "solo": "1", "nx1": "p", "nx2": "q"}
	check := func(s *Store, when string) {
		t.Helper()
		for k, w := range want {
			if got, err := s.Get(k); got != w || err != nil {
				t.Errorf("%s: %s = %q, %v; want %q", when, k, got, err, w)
			}
		}
		if s.Len() != len(want) {
			t.Errorf("%s: %d keys, want %d", when, s.Len(), len(want))
		}
		if _, has, _ := s.TTL("ttl"); has {
			t.Errorf("%s: ttl has an expiry again", when)
		}
	}

	s, info := mustOpen(t, path, manualOpts())
	check(s, "after restart")
	if got := s.Stats().LiveBytes; got != live {
		t.Errorf("LiveBytes after replay = %d, before close %d", got, live)
	}
	// old, ttl, the MSET batch, solo, the MSETNX batch.
	if info.Records != 5 {
		t.Errorf("replayed %d records, want 5", info.Records)
	}
	s.Compact()
	if got := fileSize(t, path); got != live {
		t.Errorf("compacted log is %d bytes, LiveBytes said %d", got, live)
	}
	s.Close()
	s, _ = mustOpen(t, path, manualOpts())
	defer s.Close()
	check(s, "after compaction and restart")
}

// A crash part way through writing an MSET loses all of it, never half: the
// money moved from one key to another is either still in the first or has
// fully arrived in the second.
func TestMSetCrashIsAllOrNothing(t *testing.T) {
	path := filepath.Join(t.TempDir(), "crash.log")
	s, _ := mustOpen(t, path, manualOpts())
	s.MSet(kvs("from", "100", "to", "0"))
	good := s.Stats().LogBytes
	s.MSet(kvs("from", "40", "to", "60"))
	end := s.Stats().LogBytes
	s.Close()

	full, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for cut := good + 1; cut < end; cut++ {
		if err := os.WriteFile(path, full[:cut], 0o644); err != nil {
			t.Fatal(err)
		}
		s, info := mustOpen(t, path, manualOpts())
		vals, _ := s.MGet([]string{"from", "to"})
		s.Close()
		if vals[0] != "100" || vals[1] != "0" || info.TornBytes != cut-good {
			t.Fatalf("cut at %d of %d: from=%q to=%q, torn %d", cut, end, vals[0], vals[1], info.TornBytes)
		}
	}
}

func TestMSetTooLarge(t *testing.T) {
	path := filepath.Join(t.TempDir(), "big.log")
	s, _ := mustOpen(t, path, manualOpts())
	defer s.Close()
	half := strings.Repeat("x", aol.MaxFieldSize/2)
	// Each value fits on its own, but the batch holding both doesn't.
	if err := s.MSet(kvs("a", half, "b", half)); err != aol.ErrTooLarge {
		t.Fatalf("MSet over the batch limit: err = %v", err)
	}
	if ok, err := s.MSetNX(kvs("a", half, "b", half)); ok || err != aol.ErrTooLarge {
		t.Fatalf("MSetNX over the batch limit = %v, %v", ok, err)
	}
	if s.Exists("a") || s.Exists("b") || s.Len() != 0 {
		t.Fatal("a refused MSET left keys behind")
	}
}

// Writers keep moving a total of 100 between two keys with MSET, while
// readers check with MGET that the two always add up. A reader that ever
// saw one key from before an MSET and the other from after would catch it.
func TestMSetIsAtomicForReaders(t *testing.T) {
	path := filepath.Join(t.TempDir(), "race.log")
	s, _ := mustOpen(t, path, Options{AutoCompact: true, CompactMinBytes: 4 << 10})
	s.MSet(kvs("x", "100", "y", "0"))

	var wg sync.WaitGroup
	stop := make(chan struct{})
	errs := make(chan string, 8)
	for r := 0; r < 4; r++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				vals, _ := s.MGet([]string{"x", "y"})
				a, _ := strconv.Atoi(vals[0])
				b, _ := strconv.Atoi(vals[1])
				if a+b != 100 {
					errs <- fmt.Sprintf("MGet saw x=%s y=%s", vals[0], vals[1])
					return
				}
			}
		}()
	}
	var writers sync.WaitGroup
	for w := 0; w < 4; w++ {
		writers.Add(1)
		go func(w int) {
			defer writers.Done()
			for i := 0; i < 300; i++ {
				n := (w*300 + i) % 101
				if err := s.MSet(kvs("x", strconv.Itoa(n), "y", strconv.Itoa(100-n))); err != nil {
					errs <- err.Error()
					return
				}
			}
		}(w)
	}
	writers.Wait()
	close(stop)
	wg.Wait()
	close(errs)
	for e := range errs {
		t.Fatal(e)
	}
	if s.Stats().Compactions == 0 {
		t.Error("the test never compacted; make CompactMinBytes smaller")
	}
	s.Close()

	s, _ = mustOpen(t, path, manualOpts())
	defer s.Close()
	vals, found := s.MGet([]string{"x", "y"})
	a, _ := strconv.Atoi(vals[0])
	b, _ := strconv.Atoi(vals[1])
	if !found[0] || !found[1] || a+b != 100 {
		t.Fatalf("after restart: x=%q y=%q", vals[0], vals[1])
	}
}

// Many callers race MSETNX for overlapping groups of keys. Every key must
// end up owned by exactly one caller, and each winner must own its whole
// group: no caller gets part of what it asked for.
func TestMSetNXOneWinnerPerKey(t *testing.T) {
	s := New()
	const callers = 16
	won := make([]bool, callers)
	var wg sync.WaitGroup
	for c := 0; c < callers; c++ {
		wg.Add(1)
		go func(c int) {
			defer wg.Done()
			// Caller c wants seats c and c+1, so neighbours overlap.
			me := strconv.Itoa(c)
			ok, err := s.MSetNX(kvs("seat"+strconv.Itoa(c), me, "seat"+strconv.Itoa(c+1), me))
			if err != nil {
				t.Error(err)
			}
			won[c] = ok
		}(c)
	}
	wg.Wait()
	for c := 0; c < callers; c++ {
		if !won[c] {
			continue
		}
		me := strconv.Itoa(c)
		vals, _ := s.MGet([]string{"seat" + strconv.Itoa(c), "seat" + strconv.Itoa(c+1)})
		if vals[0] != me || vals[1] != me {
			t.Errorf("caller %d won but owns %q", c, vals)
		}
		if c+1 < callers && won[c+1] {
			t.Errorf("callers %d and %d both won seat %d", c, c+1, c+1)
		}
	}
}
