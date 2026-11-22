package store

import (
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func TestGetDel(t *testing.T) {
	c := newClock()
	path := filepath.Join(t.TempDir(), "getdel.log")
	opts := Options{Sync: false, Now: c.now}
	s, _ := mustOpen(t, path, opts)
	defer func() { s.Close() }()

	s.Set("code", "481516")
	if v, err := s.GetDel("code"); v != "481516" || err != nil {
		t.Fatalf("GetDel = %q, %v; want the value", v, err)
	}
	if s.Exists("code") {
		t.Fatal("GetDel left the key")
	}
	if v, err := s.GetDel("code"); v != "" || err != ErrNotFound {
		t.Fatalf("second GetDel = %q, %v; want ErrNotFound", v, err)
	}

	// A missing or expired key writes nothing to the log.
	s.Set("short", "x")
	s.Expire("short", time.Second)
	c.advance(time.Second)
	before := s.Stats().LogBytes
	if _, err := s.GetDel("short"); err != ErrNotFound {
		t.Fatalf("GetDel of an expired key: err = %v", err)
	}
	if _, err := s.GetDel("never"); err != ErrNotFound {
		t.Fatalf("GetDel of a missing key: err = %v", err)
	}
	if after := s.Stats().LogBytes; after != before {
		t.Fatalf("GetDel of missing keys grew the log from %d to %d bytes", before, after)
	}

	// The delete is in the log, so it holds across a restart.
	s.Set("job", "send-email")
	s.GetDel("job")
	s.Close()
	s, _ = mustOpen(t, path, opts)
	if s.Exists("job") {
		t.Fatal("a GetDel'd key came back after a restart")
	}
}

func TestGetDelOnlyOneWinner(t *testing.T) {
	s := New()
	const rounds, workers = 200, 8
	for r := 0; r < rounds; r++ {
		s.Set("ticket", "one")
		var wg sync.WaitGroup
		var mu sync.Mutex
		winners := 0
		for w := 0; w < workers; w++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				if _, err := s.GetDel("ticket"); err == nil {
					mu.Lock()
					winners++
					mu.Unlock()
				}
			}()
		}
		wg.Wait()
		if winners != 1 {
			t.Fatalf("round %d: %d callers got the value, want exactly 1", r, winners)
		}
	}
}

func TestGetEx(t *testing.T) {
	c := newClock()
	path := filepath.Join(t.TempDir(), "getex.log")
	opts := Options{Sync: false, Now: c.now}
	s, _ := mustOpen(t, path, opts)
	defer func() { s.Close() }()

	get := func(key string, o GetExOptions) string {
		t.Helper()
		v, err := s.GetEx(key, o)
		if err != nil {
			t.Fatalf("GetEx(%s, %+v): %v", key, o, err)
		}
		return v
	}
	ttl := func(key string) (time.Duration, bool) {
		t.Helper()
		left, has, err := s.TTL(key)
		if err != nil {
			t.Fatalf("TTL(%s): %v", key, err)
		}
		return left, has
	}

	s.Set("session", "abc")

	// No option: a plain read that writes nothing.
	before := s.Stats().LogBytes
	if v := get("session", GetExOptions{}); v != "abc" {
		t.Fatalf("GetEx with no option = %q", v)
	}
	if s.Stats().LogBytes != before {
		t.Fatal("GetEx with no option wrote to the log")
	}

	// EX: reads and sets a deadline, rounding up to the millisecond.
	if v := get("session", GetExOptions{TTL: 30 * time.Second}); v != "abc" {
		t.Fatalf("GetEx EX = %q", v)
	}
	if left, has := ttl("session"); !has || left != 30*time.Second {
		t.Fatalf("TTL after GetEx EX = %v, %v", left, has)
	}
	get("session", GetExOptions{TTL: 300 * time.Microsecond})
	if left, _ := ttl("session"); left != time.Millisecond {
		t.Fatalf("TTL after a sub-ms GetEx = %v, want 1ms", left)
	}

	// Each read pushes the deadline back, so a session used often never ends.
	get("session", GetExOptions{TTL: 10 * time.Second})
	for i := 0; i < 5; i++ {
		c.advance(8 * time.Second)
		get("session", GetExOptions{TTL: 10 * time.Second})
	}
	if left, _ := ttl("session"); left != 10*time.Second {
		t.Fatalf("TTL after sliding GetEx = %v, want 10s", left)
	}

	// At: a moment ahead sets it, rounded up.
	at := c.now().Add(time.Minute)
	get("session", GetExOptions{At: at.Add(300 * time.Microsecond)})
	if got, _, _ := s.Deadline("session"); !got.Equal(at.Add(time.Millisecond)) {
		t.Fatalf("Deadline after GetEx At = %v, want %v", got, at.Add(time.Millisecond))
	}

	// Persist: removes the deadline; on a key without one it writes nothing.
	get("session", GetExOptions{Persist: true})
	if _, has := ttl("session"); has {
		t.Fatal("GetEx Persist kept the deadline")
	}
	before = s.Stats().LogBytes
	get("session", GetExOptions{Persist: true})
	if s.Stats().LogBytes != before {
		t.Fatal("GetEx Persist on a key with no deadline wrote to the log")
	}

	// A moment that has already come: the value is returned and the key is
	// gone, even with the clock between two milliseconds.
	c.advance(300 * time.Microsecond)
	if v := get("session", GetExOptions{At: c.now()}); v != "abc" {
		t.Fatalf("GetEx At(now) = %q", v)
	}
	if s.Exists("session") {
		t.Fatal("GetEx At(now) left the key")
	}

	// Missing and expired keys: ErrNotFound and nothing written.
	s.Set("brief", "x")
	s.Expire("brief", time.Second)
	c.advance(time.Second)
	before = s.Stats().LogBytes
	for _, k := range []string{"brief", "never"} {
		for _, o := range []GetExOptions{{}, {TTL: time.Second}, {At: c.now().Add(time.Second)}, {Persist: true}} {
			if v, err := s.GetEx(k, o); v != "" || err != ErrNotFound {
				t.Fatalf("GetEx(%s, %+v) = %q, %v; want ErrNotFound", k, o, v, err)
			}
		}
	}
	if s.Stats().LogBytes != before {
		t.Fatal("GetEx of missing keys wrote to the log")
	}

	// Bad options are refused and change nothing.
	s.Set("k", "v")
	bad := []GetExOptions{
		{TTL: -time.Second},
		{TTL: time.Second, Persist: true},
		{TTL: time.Second, At: c.now().Add(time.Second)},
		{At: c.now().Add(time.Second), Persist: true},
	}
	for _, o := range bad {
		if _, err := s.GetEx("k", o); err != ErrBadGetExOptions {
			t.Fatalf("GetEx(%+v): err = %v, want ErrBadGetExOptions", o, err)
		}
	}
	if _, err := s.GetEx("k", GetExOptions{TTL: MaxTTL + time.Second}); err != ErrTTLTooLong {
		t.Fatalf("GetEx over MaxTTL: err = %v", err)
	}
	if _, err := s.GetEx("k", GetExOptions{At: c.now().Add(MaxTTL + time.Second)}); err != ErrTTLTooLong {
		t.Fatalf("GetEx At over MaxTTL: err = %v", err)
	}
	if _, has := ttl("k"); has {
		t.Fatal("a refused GetEx set a deadline")
	}

	// Deadlines set by GetEx are in the log and survive a restart.
	get("k", GetExOptions{TTL: time.Minute})
	s.Close()
	c.advance(20 * time.Second)
	s, _ = mustOpen(t, path, opts)
	if left, has := ttl("k"); !has || left != 40*time.Second {
		t.Fatalf("TTL after restart = %v, %v; want 40s", left, has)
	}
}
