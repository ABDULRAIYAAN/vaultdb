package store

import (
	"path/filepath"
	"testing"
	"time"
)

func TestExpireAtAndDeadline(t *testing.T) {
	c := newClock()
	s := memStore(c)
	s.Set("session", "abc")
	s.Set("plain", "x")

	at := c.now().Add(10 * time.Second)
	if ok, err := s.ExpireAt("session", at); !ok || err != nil {
		t.Fatalf("ExpireAt = %v, %v; want true, nil", ok, err)
	}
	if got, has, err := s.Deadline("session"); err != nil || !has || !got.Equal(at) {
		t.Fatalf("Deadline = %v, %v, %v; want %v", got, has, err, at)
	}
	if left, _, _ := s.TTL("session"); left != 10*time.Second {
		t.Fatalf("TTL after ExpireAt = %v, want 10s", left)
	}
	if _, has, err := s.Deadline("plain"); has || err != nil {
		t.Fatalf("Deadline of a key with no expiry = %v, %v", has, err)
	}
	if _, _, err := s.Deadline("missing"); err != ErrNotFound {
		t.Fatalf("Deadline of a missing key: err = %v", err)
	}
	if ok, _ := s.ExpireAt("missing", at); ok {
		t.Fatal("ExpireAt on a missing key reported true")
	}

	// Between two milliseconds rounds up, so the key never goes early.
	if _, err := s.ExpireAt("session", at.Add(300*time.Microsecond)); err != nil {
		t.Fatal(err)
	}
	if got, _, _ := s.Deadline("session"); !got.Equal(at.Add(time.Millisecond)) {
		t.Fatalf("Deadline after a sub-ms ExpireAt = %v, want %v", got, at.Add(time.Millisecond))
	}

	// Gone at the deadline, and Deadline agrees.
	c.advance(10*time.Second + time.Millisecond)
	if _, _, err := s.Deadline("session"); err != ErrNotFound {
		t.Fatalf("Deadline after expiry: err = %v", err)
	}

	// Now or earlier deletes straight away, even with the clock between two
	// milliseconds (rounding "now" up would keep the key for a moment).
	c.advance(300 * time.Microsecond)
	if ok, err := s.ExpireAt("plain", c.now()); !ok || err != nil {
		t.Fatalf("ExpireAt(now) = %v, %v", ok, err)
	}
	if s.Exists("plain") {
		t.Fatal("ExpireAt(now) left the key")
	}
	s.Set("old", "x")
	if ok, _ := s.ExpireAt("old", time.Unix(0, 0)); !ok || s.Exists("old") {
		t.Fatal("ExpireAt(1970) didn't delete the key")
	}

	// More than MaxTTL ahead is refused and changes nothing.
	s.Set("k", "v")
	if _, err := s.ExpireAt("k", c.now().Add(MaxTTL+time.Second)); err != ErrTTLTooLong {
		t.Fatalf("ExpireAt over MaxTTL: err = %v", err)
	}
	if _, has, _ := s.Deadline("k"); has {
		t.Fatal("a refused ExpireAt set a deadline")
	}
}

func TestSetWithAtAndOld(t *testing.T) {
	c := newClock()
	s := memStore(c)

	set := func(key, value string, o SetOptions) (string, bool, bool) {
		t.Helper()
		old, existed, wrote, err := s.SetWithOld(key, value, o)
		if err != nil {
			t.Fatalf("SetWithOld(%q, %q, %+v): %v", key, value, o, err)
		}
		return old, existed, wrote
	}

	if old, existed, wrote := set("otp", "1", SetOptions{}); old != "" || existed || !wrote {
		t.Fatalf("first SET = %q, %v, %v", old, existed, wrote)
	}
	at := c.now().Add(time.Minute)
	if old, existed, wrote := set("otp", "2", SetOptions{At: at}); old != "1" || !existed || !wrote {
		t.Fatalf("SET EXAT = %q, %v, %v", old, existed, wrote)
	}
	if got, has, _ := s.Deadline("otp"); !has || !got.Equal(at) {
		t.Fatalf("Deadline after SET EXAT = %v, %v", got, has)
	}
	// NX hands back the old value even though it doesn't write; XX on a
	// missing key has nothing to hand back.
	if old, existed, wrote := set("otp", "3", SetOptions{IfMissing: true}); old != "2" || !existed || wrote {
		t.Fatalf("SET NX GET on a live key = %q, %v, %v", old, existed, wrote)
	}
	if old, existed, wrote := set("nobody", "x", SetOptions{IfExists: true}); old != "" || existed || wrote {
		t.Fatalf("SET XX GET on a missing key = %q, %v, %v", old, existed, wrote)
	}
	// An expired key has no old value.
	c.advance(time.Minute)
	if old, existed, _ := set("otp", "4", SetOptions{}); old != "" || existed {
		t.Fatalf("SET GET after expiry = %q, %v", old, existed)
	}

	// A deadline that has already come leaves the key deleted, even with the
	// clock between two milliseconds.
	c.advance(300 * time.Microsecond)
	if old, existed, wrote := set("otp", "5", SetOptions{At: c.now()}); old != "4" || !existed || !wrote {
		t.Fatalf("SET with a past EXAT = %q, %v, %v", old, existed, wrote)
	}
	if s.Exists("otp") {
		t.Fatal("SET with a past EXAT left the key")
	}
	if _, _, wrote := set("fresh", "v", SetOptions{At: time.Unix(1, 0)}); !wrote || s.Exists("fresh") {
		t.Fatal("SET with a past EXAT on a missing key should succeed and store nothing")
	}

	for _, o := range []SetOptions{
		{At: at, TTL: time.Second},
		{At: at, KeepTTL: true},
	} {
		if _, err := s.SetWith("bad", "x", o); err != ErrBadSetOptions {
			t.Errorf("SetWith(%+v): err = %v, want ErrBadSetOptions", o, err)
		}
	}
	if _, err := s.SetWith("bad", "x", SetOptions{At: c.now().Add(MaxTTL + time.Second)}); err != ErrTTLTooLong {
		t.Errorf("SetWith At over MaxTTL: err = %v", err)
	}
	if s.Exists("bad") {
		t.Fatal("a refused SetWith wrote the key")
	}
}

// Absolute deadlines go through the same records as relative ones, so they
// survive a restart, and a key whose moment comes while the database is
// closed is gone when it opens.
func TestAbsoluteDeadlinesSurviveRestart(t *testing.T) {
	c := newClock()
	path := filepath.Join(t.TempDir(), "at.log")
	opts := Options{Sync: true, Now: c.now}
	s, _ := mustOpen(t, path, opts)
	start := c.now()
	s.SetWith("short", "a", SetOptions{At: start.Add(5 * time.Second)})
	s.SetWith("long", "b", SetOptions{At: start.Add(time.Hour)})
	s.Set("later", "c")
	s.ExpireAt("later", start.Add(time.Minute))
	s.Set("past", "d")
	s.SetWith("past", "e", SetOptions{At: start}) // logged as a DEL
	s.Close()

	c.advance(10 * time.Second)
	s, info := mustOpen(t, path, opts)
	defer s.Close()
	if info.Records != 6 {
		t.Fatalf("replayed %d records, want 6 (SETEX, SETEX, SET, EXPIRE, SET, DEL)", info.Records)
	}
	if s.Exists("short") || s.Exists("past") {
		t.Fatal("a key whose deadline passed came back")
	}
	for k, want := range map[string]time.Time{"long": start.Add(time.Hour), "later": start.Add(time.Minute)} {
		if got, has, err := s.Deadline(k); err != nil || !has || !got.Equal(want) {
			t.Fatalf("Deadline(%s) after restart = %v, %v, %v; want %v", k, got, has, err, want)
		}
	}
	if st := s.Stats(); st.Keys != 2 {
		t.Fatalf("keys after restart = %d, want 2", st.Keys)
	}
}
