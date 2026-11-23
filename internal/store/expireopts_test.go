package store

import (
	"path/filepath"
	"testing"
	"time"
)

// TestExpireConditions runs every EXPIRE condition against a key with no
// deadline and a key whose deadline is 10 seconds away, with a new deadline
// that is sooner, the same, later, or already past.
func TestExpireConditions(t *testing.T) {
	nx := ExpireOptions{IfNoDeadline: true}
	xx := ExpireOptions{IfHasDeadline: true}
	gt := ExpireOptions{IfLater: true}
	lt := ExpireOptions{IfSooner: true}
	xxgt := ExpireOptions{IfHasDeadline: true, IfLater: true}
	xxlt := ExpireOptions{IfHasDeadline: true, IfSooner: true}

	const none = -1 // wantLeft for "the key has no deadline"
	const gone = -2 // wantLeft for "the key was deleted"
	cases := []struct {
		name     string
		timed    bool          // the key starts with a 10s deadline
		d        time.Duration // the new deadline, from now
		o        ExpireOptions
		want     bool
		wantLeft time.Duration
	}{
		{"no options, plain", false, 5 * time.Second, ExpireOptions{}, true, 5 * time.Second},
		{"no options, timed", true, 20 * time.Second, ExpireOptions{}, true, 20 * time.Second},

		{"NX, plain", false, 5 * time.Second, nx, true, 5 * time.Second},
		{"NX, timed", true, 5 * time.Second, nx, false, 10 * time.Second},
		{"NX, plain, past", false, 0, nx, true, gone},

		{"XX, plain", false, 5 * time.Second, xx, false, none},
		{"XX, timed", true, 5 * time.Second, xx, true, 5 * time.Second},
		{"XX, plain, past", false, 0, xx, false, none},

		// No deadline counts as "never", which nothing is later than.
		{"GT, plain", false, 5 * time.Second, gt, false, none},
		{"GT, timed, later", true, 20 * time.Second, gt, true, 20 * time.Second},
		{"GT, timed, same", true, 10 * time.Second, gt, false, 10 * time.Second},
		{"GT, timed, sooner", true, 5 * time.Second, gt, false, 10 * time.Second},
		{"GT, timed, past", true, 0, gt, false, 10 * time.Second},
		{"GT, timed, same after rounding", true, 10*time.Second - 300*time.Microsecond, gt, false, 10 * time.Second},

		// And everything is sooner than "never".
		{"LT, plain", false, 5 * time.Second, lt, true, 5 * time.Second},
		{"LT, plain, past", false, -time.Second, lt, true, gone},
		{"LT, timed, sooner", true, 5 * time.Second, lt, true, 5 * time.Second},
		{"LT, timed, same", true, 10 * time.Second, lt, false, 10 * time.Second},
		{"LT, timed, later", true, 20 * time.Second, lt, false, 10 * time.Second},
		{"LT, timed, past", true, 0, lt, true, gone},

		// XX with GT or LT: needs a deadline and the comparison to hold.
		{"XX GT, plain", false, 5 * time.Second, xxgt, false, none},
		{"XX GT, timed, later", true, 20 * time.Second, xxgt, true, 20 * time.Second},
		{"XX LT, plain", false, 5 * time.Second, xxlt, false, none},
		{"XX LT, timed, sooner", true, 5 * time.Second, xxlt, true, 5 * time.Second},
		{"XX LT, timed, later", true, 20 * time.Second, xxlt, false, 10 * time.Second},
	}
	for _, tc := range cases {
		for _, absolute := range []bool{false, true} {
			name := tc.name
			if absolute {
				name += " (ExpireAtWith)"
			}
			c := newClock()
			s := memStore(c)
			s.Set("k", "v")
			if tc.timed {
				s.Expire("k", 10*time.Second)
			}
			var got bool
			var err error
			if absolute {
				got, err = s.ExpireAtWith("k", c.now().Add(tc.d), tc.o)
			} else {
				got, err = s.ExpireWith("k", tc.d, tc.o)
			}
			if err != nil || got != tc.want {
				t.Errorf("%s: got %v, %v; want %v", name, got, err, tc.want)
				continue
			}
			left, has, err := s.TTL("k")
			switch tc.wantLeft {
			case gone:
				if err != ErrNotFound {
					t.Errorf("%s: key should be gone, TTL = %v, %v, %v", name, left, has, err)
				}
			case none:
				if err != nil || has {
					t.Errorf("%s: key should have no deadline, TTL = %v, %v, %v", name, left, has, err)
				}
			default:
				if err != nil || !has || left != tc.wantLeft {
					t.Errorf("%s: TTL = %v, %v, %v; want %v", name, left, has, err, tc.wantLeft)
				}
			}
		}
	}
}

func TestExpireConditionsEdgeCases(t *testing.T) {
	path := filepath.Join(t.TempDir(), "opts.log")
	s, _ := mustOpen(t, path, manualOpts())
	defer func() { s.Close() }()
	s.Set("k", "v")
	s.Expire("k", 10*time.Second)

	// Contradicting options are refused before anything is looked at.
	bad := []ExpireOptions{
		{IfNoDeadline: true, IfHasDeadline: true},
		{IfNoDeadline: true, IfLater: true},
		{IfNoDeadline: true, IfSooner: true},
		{IfLater: true, IfSooner: true},
		{IfHasDeadline: true, IfLater: true, IfSooner: true},
	}
	for _, o := range bad {
		if _, err := s.ExpireWith("k", time.Second, o); err != ErrBadExpireOptions {
			t.Errorf("ExpireWith(%+v): err = %v, want ErrBadExpireOptions", o, err)
		}
		if _, err := s.ExpireAtWith("k", time.Now().Add(time.Second), o); err != ErrBadExpireOptions {
			t.Errorf("ExpireAtWith(%+v): err = %v, want ErrBadExpireOptions", o, err)
		}
	}

	// A condition that says no writes nothing to the log.
	before := s.Stats().LogBytes
	s.ExpireWith("k", time.Hour, ExpireOptions{IfNoDeadline: true})
	s.ExpireWith("k", time.Hour, ExpireOptions{IfSooner: true})
	s.ExpireWith("missing", time.Hour, ExpireOptions{IfSooner: true})
	if after := s.Stats().LogBytes; after != before {
		t.Fatalf("refused EXPIREs wrote %d bytes to the log", after-before)
	}

	// The 100-year limit still comes first.
	if _, err := s.ExpireWith("k", MaxTTL+time.Second, ExpireOptions{IfLater: true}); err != ErrTTLTooLong {
		t.Fatalf("ExpireWith over MaxTTL: err = %v", err)
	}

	// An expired key counts as missing, so even LT can't touch it.
	s.Set("short", "x")
	s.Expire("short", time.Millisecond)
	time.Sleep(5 * time.Millisecond)
	if ok, _ := s.ExpireWith("short", -time.Second, ExpireOptions{IfSooner: true}); ok {
		t.Fatal("LT on an expired key reported true")
	}

	// A GT that went ahead survives a restart with the deadline it set.
	if ok, _ := s.ExpireWith("k", time.Hour, ExpireOptions{IfHasDeadline: true, IfLater: true}); !ok {
		t.Fatal("XX GT with a later deadline didn't go ahead")
	}
	want, _, _ := s.Deadline("k")
	s.Close()
	s, _ = mustOpen(t, path, manualOpts())
	if got, has, err := s.Deadline("k"); err != nil || !has || !got.Equal(want) {
		t.Fatalf("after restart: Deadline = %v, %v, %v; want %v", got, has, err, want)
	}
}
