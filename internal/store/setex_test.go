package store

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ABDULRAIYAAN/vaultdb/internal/aol"
)

func TestSetWithOptions(t *testing.T) {
	c := newClock()
	s := memStore(c)

	mustSet := func(key, value string, o SetOptions, want bool) {
		t.Helper()
		got, err := s.SetWith(key, value, o)
		if err != nil || got != want {
			t.Fatalf("SetWith(%q, %q, %+v) = %v, %v; want %v", key, value, o, got, err, want)
		}
	}
	ttl := func(key string) (time.Duration, bool) {
		t.Helper()
		left, has, err := s.TTL(key)
		if err != nil {
			t.Fatalf("TTL(%q): %v", key, err)
		}
		return left, has
	}

	// EX: the value and its deadline together.
	mustSet("otp", "481516", SetOptions{TTL: 30 * time.Second}, true)
	if left, has := ttl("otp"); !has || left != 30*time.Second {
		t.Fatalf("TTL after SET EX 30 = %v, %v", left, has)
	}
	// A plain SET still clears it, and KEEPTTL doesn't.
	c.advance(10 * time.Second)
	mustSet("otp", "999999", SetOptions{KeepTTL: true}, true)
	if left, has := ttl("otp"); !has || left != 20*time.Second {
		t.Fatalf("TTL after KEEPTTL = %v, %v; want 20s", left, has)
	}
	if v, _ := s.Get("otp"); v != "999999" {
		t.Fatalf("KEEPTTL didn't replace the value: %q", v)
	}
	mustSet("otp", "plain", SetOptions{}, true)
	if _, has := ttl("otp"); has {
		t.Fatal("a plain SetWith kept the deadline")
	}
	// KEEPTTL on a key with no deadline, or no key at all, is a plain SET.
	mustSet("otp", "again", SetOptions{KeepTTL: true}, true)
	mustSet("fresh", "v", SetOptions{KeepTTL: true}, true)
	for _, k := range []string{"otp", "fresh"} {
		if _, has := ttl(k); has {
			t.Fatalf("KEEPTTL gave %s a deadline it never had", k)
		}
	}

	// NX and XX.
	mustSet("lock", "a", SetOptions{IfMissing: true, TTL: time.Second}, true)
	mustSet("lock", "b", SetOptions{IfMissing: true, TTL: time.Second}, false)
	if v, _ := s.Get("lock"); v != "a" {
		t.Fatalf("NX overwrote the lock: %q", v)
	}
	mustSet("nobody", "x", SetOptions{IfExists: true}, false)
	if s.Exists("nobody") {
		t.Fatal("XX created a key")
	}
	mustSet("lock", "c", SetOptions{IfExists: true, KeepTTL: true}, true)
	if left, has := ttl("lock"); !has || left != time.Second {
		t.Fatalf("XX KEEPTTL lost the deadline: %v, %v", left, has)
	}
	// Once the lock has expired, it counts as missing for NX and XX.
	c.advance(time.Second)
	mustSet("lock", "d", SetOptions{IfExists: true}, false)
	mustSet("lock", "e", SetOptions{IfMissing: true}, true)
	if _, has := ttl("lock"); has {
		t.Fatal("a lock taken after expiry kept the old deadline")
	}

	// Short TTLs round up, like Expire.
	mustSet("blink", "x", SetOptions{TTL: time.Microsecond}, true)
	if left, _ := ttl("blink"); left != time.Millisecond {
		t.Fatalf("TTL after SetWith(1µs) = %v, want 1ms", left)
	}

	for _, o := range []SetOptions{
		{TTL: -time.Second},
		{TTL: time.Second, KeepTTL: true},
		{IfMissing: true, IfExists: true},
	} {
		if _, err := s.SetWith("bad", "x", o); err != ErrBadSetOptions {
			t.Errorf("SetWith(%+v): err = %v, want ErrBadSetOptions", o, err)
		}
	}
	if _, err := s.SetWith("bad", "x", SetOptions{TTL: MaxTTL + time.Second}); err != ErrTTLTooLong {
		t.Errorf("SetWith over MaxTTL: err = %v", err)
	}
	if _, err := s.SetWith("", "x", SetOptions{TTL: time.Second}); err != ErrEmptyKey {
		t.Errorf("SetWith empty key: err = %v", err)
	}
	if s.Exists("bad") {
		t.Fatal("a refused SetWith wrote the key")
	}
}

// SET ... EX is one record in the log, so cutting the log anywhere leaves the
// key either not set at all or set with its deadline, never set forever.
// As two records (SET, then EXPIRE) there would be a cut that keeps the
// value and loses the deadline.
func TestSetWithCrashNeverLosesDeadline(t *testing.T) {
	c := newClock()
	dir := t.TempDir()
	path := filepath.Join(dir, "crash.log")
	opts := Options{Sync: true, Now: c.now}
	s, _ := mustOpen(t, path, opts)
	s.Set("user", "raiyaan")
	before := s.Stats().LogBytes
	if ok, err := s.SetWith("lock", "token", SetOptions{TTL: 30 * time.Second, IfMissing: true}); !ok || err != nil {
		t.Fatalf("SetWith = %v, %v", ok, err)
	}
	after := s.Stats().LogBytes
	s.Close()
	if want := aol.RecordSize("lock", "token") + aol.DeadlineSize; after-before != want {
		t.Fatalf("SET EX grew the log by %d bytes, want one %d-byte record", after-before, want)
	}

	full, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	cut := filepath.Join(dir, "cut.log")
	for n := before; n <= after; n++ {
		if err := os.WriteFile(cut, full[:n], 0o644); err != nil {
			t.Fatal(err)
		}
		s, _ := mustOpen(t, cut, opts)
		left, has, err := s.TTL("lock")
		switch {
		case n == after && (err != nil || !has || left != 30*time.Second):
			t.Fatalf("full log: TTL(lock) = %v, %v, %v", left, has, err)
		case n < after && err != ErrNotFound:
			t.Fatalf("log cut to %d bytes: lock exists (deadline: %v), want it missing", n, has)
		}
		if v, _ := s.Get("user"); v != "raiyaan" {
			t.Fatalf("log cut to %d bytes lost an earlier key", n)
		}
		s.Close()
	}
}

// Compaction writes a key with a deadline as one SETEX record, which is
// smaller than a SET plus an EXPIRE, and liveBytes predicts it exactly.
func TestCompactionWritesSetExpire(t *testing.T) {
	c := newClock()
	path := filepath.Join(t.TempDir(), "compact.log")
	opts := Options{Sync: false, Now: c.now}
	s, _ := mustOpen(t, path, opts)
	s.Set("forever", "1")
	s.Set("session", "abc")
	s.Expire("session", time.Hour) // the old way: SET then EXPIRE
	s.SetWith("otp", "481516", SetOptions{TTL: time.Minute})
	s.SetWith("otp", "123456", SetOptions{KeepTTL: true})

	if _, err := s.Compact(); err != nil {
		t.Fatal(err)
	}
	st := s.Stats()
	want := int64(aol.FileHeaderSize) + aol.RecordSize("forever", "1") +
		aol.RecordSize("session", "abc") + aol.DeadlineSize +
		aol.RecordSize("otp", "123456") + aol.DeadlineSize
	if st.LogBytes != want || st.LiveBytes != want {
		t.Fatalf("after compaction: log %d bytes, liveBytes %d, want %d", st.LogBytes, st.LiveBytes, want)
	}
	s.Close()

	c.advance(30 * time.Second)
	s2, info := mustOpen(t, path, opts)
	defer s2.Close()
	if info.Records != 3 || info.Keys != 3 {
		t.Fatalf("after restart: %d records, %d keys; want 3 and 3", info.Records, info.Keys)
	}
	if left, has, _ := s2.TTL("session"); !has || left != time.Hour-30*time.Second {
		t.Fatalf("TTL(session) = %v, %v", left, has)
	}
	if left, has, _ := s2.TTL("otp"); !has || left != 30*time.Second {
		t.Fatalf("TTL(otp) = %v, %v", left, has)
	}
	if v, _ := s2.Get("otp"); v != "123456" {
		t.Fatalf("otp = %q", v)
	}
	if _, has, _ := s2.TTL("forever"); has {
		t.Fatal("forever got a deadline")
	}
}

// A deadline takes DeadlineSize bytes of the SETEX value, so a value that
// would leave no room for it can't be given one, or compaction couldn't
// write it back out.
func TestDeadlineNeedsRoomInValue(t *testing.T) {
	s := memStore(newClock())
	big := strings.Repeat("x", aol.MaxFieldSize-aol.DeadlineSize+1)
	s.Set("big", big)
	if _, err := s.Expire("big", time.Minute); err != aol.ErrTooLarge {
		t.Fatalf("Expire on a %d-byte value: err = %v, want ErrTooLarge", len(big), err)
	}
	if _, err := s.SetWith("big2", big, SetOptions{TTL: time.Minute}); err != aol.ErrTooLarge {
		t.Fatalf("SetWith EX on a %d-byte value: err = %v, want ErrTooLarge", len(big), err)
	}
	if s.Exists("big2") {
		t.Fatal("a refused SetWith wrote the key")
	}
	fits := big[1:]
	if ok, err := s.SetWith("big", fits, SetOptions{TTL: time.Minute}); !ok || err != nil {
		t.Fatalf("SetWith EX on a %d-byte value = %v, %v", len(fits), ok, err)
	}
}
