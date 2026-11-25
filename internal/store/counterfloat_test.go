package store

import (
	"math"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"
)

func TestIncrByFloat(t *testing.T) {
	s := New()
	steps := []struct {
		delta float64
		want  string
	}{{10.5, "10.5"}, {0.1, "10.6"}, {-5, "5.6"}, {-5.6, "0"}, {2, "2"}, {-2.25, "-0.25"}, {0.25, "0"}}
	for _, st := range steps {
		if got, err := s.IncrByFloat("bal", st.delta); got != st.want || err != nil {
			t.Fatalf("IncrByFloat(bal, %v) = %q, %v; want %q", st.delta, got, err, st.want)
		}
		if v, _ := s.Get("bal"); v != st.want {
			t.Fatalf("stored %q, replied %q", v, st.want)
		}
	}

	// Every plain decimal form is a number, and whole results have no point,
	// so INCR keeps working on them.
	for v, want := range map[string]string{
		"41": "42", "-1": "0", "+5": "6", "05": "6", "-0": "1", "1.5": "2.5",
		".5": "1.5", "5.": "6", "1e3": "1001", "1E3": "1001", "2.5e-1": "1.25",
		"-1e+2": "-99", "0.1": "1.1",
	} {
		s.Set("n", v)
		if got, err := s.IncrByFloat("n", 1); got != want || err != nil {
			t.Errorf("IncrByFloat on %q = %q, %v; want %q", v, got, err, want)
		}
	}
	// Past 2^53 a float64 can't hold every whole number, so big integer
	// counters lose their last digits here. That's the float trade-off; INCR
	// is exact all the way to 2^63.
	s.Set("n", "9007199254740993")
	if got, _ := s.IncrByFloat("n", 1); got != "9007199254740992" {
		t.Fatalf("2^53 + 1 + 1 = %q, want the float64 answer 9007199254740992", got)
	}
	s.Set("i", "7")
	s.IncrByFloat("i", 3)
	if got, err := s.IncrBy("i", 1); got != 11 || err != nil {
		t.Fatalf("INCR after INCRBYFLOAT = %d, %v; want 11", got, err)
	}

	// Never an exponent, never "-0".
	for _, c := range []struct {
		f    float64
		want string
	}{{1e21, "1000000000000000000000"}, {1.5e-7, "0.00000015"}, {math.Copysign(0, -1), "0"}, {-3, "-3"}} {
		if got := FormatDecimal(c.f); got != c.want {
			t.Errorf("FormatDecimal(%v) = %q, want %q", c.f, got, c.want)
		}
	}
	if got, _ := s.IncrByFloat("negzero", math.Copysign(0, -1)); got != "0" {
		t.Fatalf("0 + -0 stored as %q", got)
	}

	// Anything else is refused and left alone.
	for _, v := range []string{"", "abc", " 5", "5 ", "1e", "1e+", ".", "+", "-", "e3", "1.2.3", "0x10", "0x1p-2",
		"1_000", "inf", "-Inf", "NaN", "Infinity", "1e400", "-1e309", "1,5", "５"} {
		s.Set("bad", v)
		if got, err := s.IncrByFloat("bad", 1); err != ErrNotFloat || got != "" {
			t.Errorf("IncrByFloat on %q = %q, %v; want ErrNotFloat", v, got, err)
		}
		if got, _ := s.Get("bad"); got != v {
			t.Errorf("a refused IncrByFloat changed %q to %q", v, got)
		}
		if _, ok := ParseDecimal(v); ok {
			t.Errorf("ParseDecimal(%q) accepted it", v)
		}
	}

	// The form check on its own, without strconv.ParseFloat behind it.
	for v, want := range map[string]bool{"1": true, "-.5": true, "5.e2": true, "1e": false, "1e+": false,
		"1E-": false, ".": false, ".e1": false, "1e1.5": false, "--1": false, "1e400": true} {
		if got := isDecimal(v); got != want {
			t.Errorf("isDecimal(%q) = %v, want %v", v, got, want)
		}
	}

	// A result that isn't finite is refused, and so is a NaN step.
	s.Set("huge", strconv.FormatFloat(math.MaxFloat64, 'f', -1, 64))
	for _, d := range []float64{math.MaxFloat64, math.Inf(1), math.NaN()} {
		if _, err := s.IncrByFloat("huge", d); err != ErrFloatOverflow {
			t.Errorf("MaxFloat64 + %v: err = %v, want ErrFloatOverflow", d, err)
		}
	}
	if v, _ := s.Get("huge"); v != strconv.FormatFloat(math.MaxFloat64, 'f', -1, 64) {
		t.Fatalf("a refused IncrByFloat changed the value to %q", v)
	}
	if got, err := s.IncrByFloat("huge", -math.MaxFloat64); got != "0" || err != nil {
		t.Fatalf("MaxFloat64 - MaxFloat64 = %q, %v", got, err)
	}
	if _, err := s.IncrByFloat("", 1); err != ErrEmptyKey {
		t.Fatalf("IncrByFloat with an empty key: err = %v", err)
	}
}

func TestIncrByFloatKeepsDeadline(t *testing.T) {
	c := newClock()
	path := filepath.Join(t.TempDir(), "float.log")
	opts := Options{Sync: false, Now: c.now}
	s, _ := mustOpen(t, path, opts)
	defer func() { s.Close() }()

	s.Set("spend", "1.25")
	s.Expire("spend", time.Minute)
	c.advance(15 * time.Second)
	if got, _ := s.IncrByFloat("spend", 0.5); got != "1.75" {
		t.Fatalf("spend = %q, want 1.75", got)
	}
	if left, has, _ := s.TTL("spend"); !has || left != 45*time.Second {
		t.Fatalf("TTL after IncrByFloat = %v, %v; want 45s kept", left, has)
	}

	s.Close()
	s, _ = mustOpen(t, path, opts)
	if v, _ := s.Get("spend"); v != "1.75" {
		t.Fatalf("after restart: %q, want 1.75", v)
	}
	if left, has, _ := s.TTL("spend"); !has || left != 45*time.Second {
		t.Fatalf("after restart TTL = %v, %v; want 45s", left, has)
	}
	if _, err := s.Compact(); err != nil {
		t.Fatal(err)
	}
	s.Close()
	s, _ = mustOpen(t, path, opts)
	if left, has, _ := s.TTL("spend"); !has || left != 45*time.Second {
		t.Fatalf("after compaction and restart TTL = %v, %v; want 45s", left, has)
	}

	// Past the deadline the key is gone, so it starts again from 0 with no
	// deadline.
	c.advance(45 * time.Second)
	if got, _ := s.IncrByFloat("spend", 0.5); got != "0.5" {
		t.Fatalf("after the window = %q, want 0.5", got)
	}
	if _, has, _ := s.TTL("spend"); has {
		t.Fatal("a value that restarted from 0 kept the old deadline")
	}

	// Refused increments write nothing.
	s.Set("name", "raiyaan")
	s.Set("max", strconv.FormatFloat(math.MaxFloat64, 'f', -1, 64))
	before := s.Stats().LogBytes
	s.IncrByFloat("name", 1)
	s.IncrByFloat("max", math.MaxFloat64)
	if after := s.Stats().LogBytes; after != before {
		t.Fatalf("refused IncrByFloat grew the log from %d to %d bytes", before, after)
	}
}

func TestIncrByFloatNoLostUpdates(t *testing.T) {
	path := filepath.Join(t.TempDir(), "float.log")
	s, _ := mustOpen(t, path, Options{Sync: false})
	const workers, each = 8, 500
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < each; i++ {
				// 0.5 and 0.25 are exact in binary, so the total is exact
				// whatever order the adds land in.
				delta := 0.5
				if w%2 == 1 {
					delta = 0.25
				}
				if _, err := s.IncrByFloat("total", delta); err != nil {
					t.Error(err)
					return
				}
			}
		}(w)
	}
	wg.Wait()
	want := "1500" // 4 workers adding 0.5 and 4 adding 0.25, 500 times each
	if v, _ := s.Get("total"); v != want {
		t.Fatalf("total = %s, want %s: some increments were lost", v, want)
	}
	s.Close()
	s, _ = mustOpen(t, path, Options{Sync: false})
	defer s.Close()
	if v, _ := s.Get("total"); v != want {
		t.Fatalf("after restart total = %s, want %s", v, want)
	}
}

// FuzzParseDecimal checks that whatever ParseDecimal accepts is finite, and
// that what FormatDecimal writes is accepted and reads back as the same
// float64.
func FuzzParseDecimal(f *testing.F) {
	for _, s := range []string{"0", "-1.5", ".5", "5.", "1e3", "+2E-3", "inf", "0x10", "1e400", " 1", "1_0"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, v string) {
		n, ok := ParseDecimal(v)
		if !ok {
			return
		}
		if math.IsNaN(n) || math.IsInf(n, 0) {
			t.Fatalf("ParseDecimal(%q) = %v", v, n)
		}
		out := FormatDecimal(n)
		back, ok := ParseDecimal(out)
		if !ok || back != n && !(back == 0 && n == 0) {
			t.Fatalf("%q -> %v -> %q -> %v, %v", v, n, out, back, ok)
		}
		if FormatDecimal(back) != out {
			t.Fatalf("FormatDecimal isn't stable for %q", out)
		}
	})
}
