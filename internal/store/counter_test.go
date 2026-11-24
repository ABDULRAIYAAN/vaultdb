package store

import (
	"math"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func TestIncrBy(t *testing.T) {
	s := New()
	steps := []struct {
		delta int64
		want  int64
	}{{1, 1}, {1, 2}, {10, 12}, {-20, -8}, {0, -8}, {8, 0}}
	for _, st := range steps {
		if got, err := s.IncrBy("hits", st.delta); got != st.want || err != nil {
			t.Fatalf("IncrBy(hits, %d) = %d, %v; want %d", st.delta, got, err, st.want)
		}
	}
	if v, _ := s.Get("hits"); v != "0" {
		t.Fatalf("stored value = %q, want \"0\"", v)
	}

	// Existing values in the one accepted form work, others are refused.
	for v, want := range map[string]int64{"41": 42, "-1": 0, "0": 1, "9223372036854775806": math.MaxInt64} {
		s.Set("n", v)
		if got, err := s.IncrBy("n", 1); got != want || err != nil {
			t.Errorf("IncrBy on %q = %d, %v; want %d", v, got, err, want)
		}
	}
	for _, v := range []string{"", "abc", "4.2", "+5", "05", "-0", " 5", "5 ", "1e3", "0x10", "9223372036854775808", "-9223372036854775809", "123456789012345678901"} {
		s.Set("bad", v)
		if got, err := s.IncrBy("bad", 1); err != ErrNotInteger || got != 0 {
			t.Errorf("IncrBy on %q = %d, %v; want ErrNotInteger", v, got, err)
		}
		if got, _ := s.Get("bad"); got != v {
			t.Errorf("a refused IncrBy changed %q to %q", v, got)
		}
	}

	// Overflow, both ways, including right at the edge.
	s.Set("big", "9223372036854775807")
	if _, err := s.IncrBy("big", 1); err != ErrOverflow {
		t.Fatalf("MaxInt64 + 1: err = %v, want ErrOverflow", err)
	}
	if got, err := s.IncrBy("big", -1); got != math.MaxInt64-1 || err != nil {
		t.Fatalf("MaxInt64 - 1 = %d, %v", got, err)
	}
	s.Set("small", "-9223372036854775808")
	if _, err := s.IncrBy("small", -1); err != ErrOverflow {
		t.Fatalf("MinInt64 - 1: err = %v, want ErrOverflow", err)
	}
	if got, err := s.IncrBy("small", math.MaxInt64); got != -1 || err != nil {
		t.Fatalf("MinInt64 + MaxInt64 = %d, %v; want -1", got, err)
	}
	if got, err := s.IncrBy("fresh", math.MinInt64); got != math.MinInt64 || err != nil {
		t.Fatalf("0 + MinInt64 = %d, %v", got, err)
	}
	if v, _ := s.Get("fresh"); v != "-9223372036854775808" {
		t.Fatalf("MinInt64 stored as %q", v)
	}
	if _, err := s.IncrBy("", 1); err != ErrEmptyKey {
		t.Fatalf("IncrBy with an empty key: err = %v", err)
	}
}

func TestIncrByKeepsDeadline(t *testing.T) {
	c := newClock()
	path := filepath.Join(t.TempDir(), "counter.log")
	opts := Options{Sync: false, Now: c.now}
	s, _ := mustOpen(t, path, opts)
	defer func() { s.Close() }()

	// A rate limit: a counter that resets a minute after the first hit.
	s.IncrBy("rate:ip1", 1)
	s.Expire("rate:ip1", time.Minute)
	c.advance(20 * time.Second)
	if got, _ := s.IncrBy("rate:ip1", 1); got != 2 {
		t.Fatalf("second hit = %d, want 2", got)
	}
	if left, has, _ := s.TTL("rate:ip1"); !has || left != 40*time.Second {
		t.Fatalf("TTL after IncrBy = %v, %v; want 40s kept", left, has)
	}

	// The kept deadline is in the log: it comes back after a restart, and
	// after a compaction followed by a restart.
	s.Close()
	s, _ = mustOpen(t, path, opts)
	if v, _ := s.Get("rate:ip1"); v != "2" {
		t.Fatalf("after restart: %q, want \"2\"", v)
	}
	if left, has, _ := s.TTL("rate:ip1"); !has || left != 40*time.Second {
		t.Fatalf("after restart TTL = %v, %v; want 40s", left, has)
	}
	if _, err := s.Compact(); err != nil {
		t.Fatal(err)
	}
	s.Close()
	s, _ = mustOpen(t, path, opts)
	if left, has, _ := s.TTL("rate:ip1"); !has || left != 40*time.Second {
		t.Fatalf("after compaction and restart TTL = %v, %v; want 40s", left, has)
	}

	// Once the window has passed, the counter starts again from 0, with no
	// deadline.
	c.advance(40 * time.Second)
	if got, _ := s.IncrBy("rate:ip1", 1); got != 1 {
		t.Fatalf("hit after the window = %d, want 1", got)
	}
	if _, has, _ := s.TTL("rate:ip1"); has {
		t.Fatal("a counter that restarted from 0 kept the old deadline")
	}

	// Refused increments write nothing.
	s.Set("name", "raiyaan")
	s.Set("max", "9223372036854775807")
	before := s.Stats().LogBytes
	s.IncrBy("name", 1)
	s.IncrBy("max", 1)
	if after := s.Stats().LogBytes; after != before {
		t.Fatalf("refused IncrBy grew the log from %d to %d bytes", before, after)
	}
	if st := s.Stats(); st.LiveBytes > st.LogBytes {
		t.Fatalf("liveBytes %d is more than the log's %d", st.LiveBytes, st.LogBytes)
	}
}

func TestIncrByNoLostUpdates(t *testing.T) {
	path := filepath.Join(t.TempDir(), "counter.log")
	s, _ := mustOpen(t, path, Options{Sync: false})
	const workers, each = 8, 500
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < each; i++ {
				delta := int64(1)
				if w%2 == 1 {
					delta = 3
				}
				if _, err := s.IncrBy("views", delta); err != nil {
					t.Error(err)
					return
				}
			}
		}(w)
	}
	wg.Wait()
	want := "8000" // 4 workers adding 1 and 4 adding 3, 500 times each
	if v, _ := s.Get("views"); v != want {
		t.Fatalf("views = %s, want %s: some increments were lost", v, want)
	}
	s.Close()
	s, _ = mustOpen(t, path, Options{Sync: false})
	defer s.Close()
	if v, _ := s.Get("views"); v != want {
		t.Fatalf("after restart views = %s, want %s", v, want)
	}
}
