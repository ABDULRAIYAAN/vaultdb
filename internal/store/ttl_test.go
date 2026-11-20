package store

import (
	"fmt"
	"math/rand"
	"path/filepath"
	"reflect"
	"sort"
	"testing"
	"time"

	"github.com/ABDULRAIYAAN/vaultdb/internal/aol"
)

// fakeClock is a clock that only moves when told to.
type fakeClock struct{ t time.Time }

func newClock() *fakeClock { return &fakeClock{t: time.UnixMilli(1_700_000_000_000)} }

func (c *fakeClock) now() time.Time          { return c.t }
func (c *fakeClock) advance(d time.Duration) { c.t = c.t.Add(d) }

func memStore(c *fakeClock) *Store {
	s := New()
	s.now = c.now
	return s
}

func TestExpireHidesKeyAtDeadline(t *testing.T) {
	c := newClock()
	s := memStore(c)
	s.Set("session", "abc")
	s.Set("user:1", "raiyaan")
	if ok, err := s.Expire("session", 10*time.Second); !ok || err != nil {
		t.Fatalf("Expire = %v, %v; want true, nil", ok, err)
	}
	left, has, err := s.TTL("session")
	if err != nil || !has || left != 10*time.Second {
		t.Fatalf("TTL = %v, %v, %v; want 10s, true, nil", left, has, err)
	}
	if _, has, err := s.TTL("user:1"); has || err != nil {
		t.Fatalf("TTL of a key with no deadline = %v, %v", has, err)
	}

	c.advance(10*time.Second - time.Millisecond)
	if got, err := s.Get("session"); err != nil || got != "abc" {
		t.Fatalf("1ms before the deadline: Get = %q, %v", got, err)
	}

	c.advance(time.Millisecond) // exactly at the deadline: gone
	if _, err := s.Get("session"); err != ErrNotFound {
		t.Fatalf("Get after deadline: err = %v, want ErrNotFound", err)
	}
	if s.Exists("session") {
		t.Fatal("Exists after deadline")
	}
	if got := s.Keys(); !reflect.DeepEqual(got, []string{"user:1"}) {
		t.Fatalf("Keys = %v", got)
	}
	if pairs, _ := s.Scan("", "", 0); len(pairs) != 1 || pairs[0].Key != "user:1" {
		t.Fatalf("Scan = %v", pairs)
	}
	if pairs, _ := s.Prefix("sess", 0); len(pairs) != 0 {
		t.Fatalf("Prefix = %v", pairs)
	}
	if s.Len() != 1 {
		t.Fatalf("Len = %d, want 1", s.Len())
	}
	if st := s.Stats(); st.Keys != 1 || st.Expiring != 0 {
		t.Fatalf("Stats = %+v", st)
	}
	if _, _, err := s.TTL("session"); err != ErrNotFound {
		t.Fatalf("TTL after deadline: err = %v", err)
	}
	if ok, _ := s.Delete("session"); ok {
		t.Fatal("Delete of an expired key returned true")
	}
	if ok, _ := s.Expire("session", time.Minute); ok {
		t.Fatal("Expire of an expired key returned true")
	}
}

// Scan's limit counts only live keys, so expired ones can't use it up.
func TestScanLimitSkipsExpired(t *testing.T) {
	c := newClock()
	s := memStore(c)
	for i := 0; i < 6; i++ {
		s.Set(fmt.Sprintf("k%d", i), "v")
	}
	for _, k := range []string{"k0", "k1", "k3"} {
		s.Expire(k, time.Second)
	}
	c.advance(time.Second)
	pairs, more := s.Scan("", "", 2)
	if len(pairs) != 2 || pairs[0].Key != "k2" || pairs[1].Key != "k4" || !more {
		t.Fatalf("Scan limit 2 = %v, more=%v", pairs, more)
	}
}

func TestSetClearsDeadlineAndPersist(t *testing.T) {
	c := newClock()
	s := memStore(c)
	s.Set("a", "1")
	s.Set("b", "1")

	if ok, _ := s.Persist("a"); ok {
		t.Fatal("Persist of a key with no deadline returned true")
	}
	if ok, _ := s.Persist("missing"); ok {
		t.Fatal("Persist of a missing key returned true")
	}
	if ok, _ := s.Expire("missing", time.Second); ok {
		t.Fatal("Expire of a missing key returned true")
	}

	s.Expire("a", time.Second)
	s.Set("a", "2") // a new value, and no deadline
	s.Expire("b", time.Second)
	if ok, _ := s.Persist("b"); !ok {
		t.Fatal("Persist of a key with a deadline returned false")
	}
	c.advance(time.Hour)
	for _, k := range []string{"a", "b"} {
		if !s.Exists(k) {
			t.Fatalf("%s expired, but its deadline was cleared", k)
		}
	}

	// An expiry of zero or less deletes at once, and short ones round up.
	if ok, _ := s.Expire("a", 0); !ok || s.Exists("a") {
		t.Fatal("Expire 0 should delete the key")
	}
	s.Expire("b", time.Microsecond)
	if left, _, _ := s.TTL("b"); left != time.Millisecond {
		t.Fatalf("TTL after Expire(1µs) = %v, want 1ms", left)
	}
	if _, err := s.Expire("b", MaxTTL+time.Second); err != ErrTTLTooLong {
		t.Fatalf("Expire over MaxTTL: err = %v", err)
	}
}

// Writes free the memory of expired keys a few at a time, even keys nobody
// touches again.
func TestWritesReapExpiredKeys(t *testing.T) {
	c := newClock()
	s := memStore(c)
	for i := 0; i < 500; i++ {
		k := fmt.Sprintf("tmp%03d", i)
		s.Set(k, "v")
		s.Expire(k, time.Second)
	}
	c.advance(time.Second)
	for i := 0; i < 500/reapBatch+1; i++ {
		s.Set("busy", "x")
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.data.Len() != 1 || len(s.expires) != 0 {
		t.Fatalf("after %d writes: %d keys and %d deadlines still in memory, want 1 and 0",
			500/reapBatch+1, s.data.Len(), len(s.expires))
	}
	if s.liveBytes != int64(aol.FileHeaderSize)+aol.RecordSize("busy", "x") {
		t.Fatalf("liveBytes = %d after reaping", s.liveBytes)
	}
}

func TestDeadlinesSurviveRestart(t *testing.T) {
	c := newClock()
	path := filepath.Join(t.TempDir(), "ttl.log")
	opts := Options{Sync: true, Now: c.now}
	s, _ := mustOpen(t, path, opts)
	s.Set("short", "1")
	s.Set("long", "2")
	s.Set("forever", "3")
	s.Expire("short", 10*time.Second)
	s.Expire("long", time.Hour)
	s.Close()

	// The database is closed for 20 seconds: short ran out meanwhile, and
	// long has to remember it was set 20 seconds ago, not start over.
	c.advance(20 * time.Second)
	s2, info := mustOpen(t, path, opts)
	if info.Keys != 2 || s2.Exists("short") {
		t.Fatalf("after restart: %d keys, short exists = %v", info.Keys, s2.Exists("short"))
	}
	if left, has, _ := s2.TTL("long"); !has || left != time.Hour-20*time.Second {
		t.Fatalf("TTL(long) after restart = %v, %v", left, has)
	}
	if _, has, _ := s2.TTL("forever"); has {
		t.Fatal("forever got a deadline")
	}

	// Compaction keeps the deadline and leaves out expired keys.
	c.advance(time.Hour)
	if _, err := s2.Compact(); err != nil {
		t.Fatal(err)
	}
	if st := s2.Stats(); st.LogBytes != st.LiveBytes || st.Keys != 1 {
		t.Fatalf("after compaction: %+v", st)
	}
	s2.Close()
	s3, info := mustOpen(t, path, opts)
	defer s3.Close()
	if info.Records != 1 || !s3.Exists("forever") {
		t.Fatalf("after compaction and restart: %d records, keys %v", info.Records, s3.Keys())
	}
}

// TestExpiryMatchesModel runs random writes (plain and with SET options),
// expiries, clock jumps, compactions and restarts, and checks the store
// against a plain map after every step. After each compaction the log has to weigh exactly what
// liveBytes said it would, which checks the size bookkeeping too.
func TestExpiryMatchesModel(t *testing.T) {
	c := newClock()
	path := filepath.Join(t.TempDir(), "model.log")
	opts := Options{Sync: false, Now: c.now}
	s, _ := mustOpen(t, path, opts)
	defer func() { s.Close() }()

	type entry struct {
		val string
		at  int64 // deadline in ms, 0 for none
	}
	model := map[string]entry{}
	nowMs := func() int64 { return c.now().UnixMilli() }
	live := func(k string) (entry, bool) {
		e, ok := model[k]
		if ok && e.at != 0 && e.at <= nowMs() {
			delete(model, k)
			return entry{}, false
		}
		return e, ok
	}

	rng := rand.New(rand.NewSource(7))
	key := func() string { return fmt.Sprintf("k%02d", rng.Intn(40)) }
	for step := 0; step < 4000; step++ {
		switch r := rng.Intn(100); {
		case r < 20:
			k, v := key(), fmt.Sprint(rng.Intn(1000))
			s.Set(k, v)
			model[k] = entry{val: v}
		case r < 35:
			// SET with EX, KEEPTTL, NX or XX, in random valid mixes.
			k, v := key(), fmt.Sprint(rng.Intn(1000))
			var o SetOptions
			switch rng.Intn(3) {
			case 0:
				o.TTL = time.Duration(rng.Intn(5000)+1) * time.Millisecond
			case 1:
				o.KeepTTL = true
			}
			switch rng.Intn(3) {
			case 0:
				o.IfMissing = true
			case 1:
				o.IfExists = true
			}
			e, ok := live(k)
			want := !(o.IfMissing && ok) && !(o.IfExists && !ok)
			got, err := s.SetWith(k, v, o)
			if err != nil || got != want {
				t.Fatalf("step %d: SetWith(%s, %+v) = %v, %v; want %v", step, k, o, got, err, want)
			}
			if want {
				n := entry{val: v}
				if o.TTL > 0 {
					n.at = nowMs() + o.TTL.Milliseconds()
				} else if o.KeepTTL && ok {
					n.at = e.at
				}
				model[k] = n
			}
		case r < 45:
			k := key()
			_, want := live(k)
			got, _ := s.Delete(k)
			if got != want {
				t.Fatalf("step %d: Delete(%s) = %v, want %v", step, k, got, want)
			}
			delete(model, k)
		case r < 70:
			k := key()
			d := time.Duration(rng.Intn(5000)-500) * time.Millisecond
			e, want := live(k)
			got, err := s.Expire(k, d)
			if err != nil || got != want {
				t.Fatalf("step %d: Expire(%s, %v) = %v, %v; want %v", step, k, d, got, err, want)
			}
			if want && d <= 0 {
				delete(model, k)
			} else if want {
				e.at = nowMs() + d.Milliseconds()
				model[k] = e
			}
		case r < 78:
			k := key()
			e, ok := live(k)
			want := ok && e.at != 0
			if got, _ := s.Persist(k); got != want {
				t.Fatalf("step %d: Persist(%s) = %v, want %v", step, k, got, want)
			}
			if want {
				e.at = 0
				model[k] = e
			}
		case r < 93:
			c.advance(time.Duration(rng.Intn(1500)) * time.Millisecond)
		case r < 97:
			if _, err := s.Compact(); err != nil {
				t.Fatal(err)
			}
			if st := s.Stats(); st.LogBytes != st.LiveBytes {
				t.Fatalf("step %d: compacted log is %d bytes, liveBytes said %d", step, st.LogBytes, st.LiveBytes)
			}
		default:
			s.Close()
			s, _ = mustOpen(t, path, opts)
		}

		var want []string
		for k := range model {
			if _, ok := live(k); ok {
				want = append(want, k)
			}
		}
		sort.Strings(want)
		got := s.Keys()
		if len(want) == 0 {
			want = []string{}
		}
		if !reflect.DeepEqual(got, want) || s.Len() != len(want) {
			t.Fatalf("step %d: Keys = %v (Len %d), want %v", step, got, s.Len(), want)
		}
		for _, k := range want {
			e := model[k]
			v, _ := s.Get(k)
			left, has, _ := s.TTL(k)
			if v != e.val || has != (e.at != 0) || (has && left != time.Duration(e.at-nowMs())*time.Millisecond) {
				t.Fatalf("step %d: %s = %q, ttl %v %v; want %q, deadline %d", step, k, v, left, has, e.val, e.at)
			}
		}
	}
}
