package store

import (
	"math"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ABDULRAIYAAN/vaultdb/internal/aol"
)

func TestAppend(t *testing.T) {
	s := New()
	steps := []struct {
		suffix string
		want   int
		val    string
	}{
		{"hello", 5, "hello"},
		{" ", 6, "hello "},
		{"world", 11, "hello world"},
		{"", 11, "hello world"},
		{"é", 13, "hello worldé"}, // 2 bytes in UTF-8: lengths count bytes
	}
	for _, st := range steps {
		n, err := s.Append("greeting", st.suffix)
		if n != st.want || err != nil {
			t.Fatalf("Append(greeting, %q) = %d, %v; want %d", st.suffix, n, err, st.want)
		}
		if v, _ := s.Get("greeting"); v != st.val {
			t.Fatalf("after Append(%q): %q, want %q", st.suffix, v, st.val)
		}
	}

	// Appending "" to a missing key creates it, empty, like Redis.
	if n, err := s.Append("empty", ""); n != 0 || err != nil {
		t.Fatalf("Append(empty, \"\") = %d, %v", n, err)
	}
	if !s.Exists("empty") {
		t.Fatal("Append of \"\" to a missing key didn't create it")
	}

	// Works on whatever a value holds, numbers included, and INCR still
	// sees a number afterwards when the result is one.
	s.Set("n", "4")
	s.Append("n", "2")
	if got, err := s.IncrBy("n", 1); got != 43 || err != nil {
		t.Fatalf("IncrBy after Append = %d, %v; want 43", got, err)
	}

	if _, err := s.Append("", "x"); err != ErrEmptyKey {
		t.Fatalf("Append with an empty key: err = %v", err)
	}
}

func TestAppendKeepsDeadline(t *testing.T) {
	c := newClock()
	path := filepath.Join(t.TempDir(), "append.log")
	opts := Options{Sync: false, Now: c.now}
	s, _ := mustOpen(t, path, opts)
	defer func() { s.Close() }()

	s.SetWith("log:today", "a", SetOptions{TTL: time.Minute})
	c.advance(15 * time.Second)
	if n, _ := s.Append("log:today", "b"); n != 2 {
		t.Fatalf("Append = %d, want 2", n)
	}
	if left, has, _ := s.TTL("log:today"); !has || left != 45*time.Second {
		t.Fatalf("TTL after Append = %v, %v; want 45s kept", left, has)
	}

	// The deadline went to the log with the value: it's still there after
	// a restart, and after a compaction followed by a restart.
	s.Close()
	s, _ = mustOpen(t, path, opts)
	if v, _ := s.Get("log:today"); v != "ab" {
		t.Fatalf("after restart: %q, want \"ab\"", v)
	}
	if left, has, _ := s.TTL("log:today"); !has || left != 45*time.Second {
		t.Fatalf("after restart TTL = %v, %v; want 45s", left, has)
	}
	if _, err := s.Compact(); err != nil {
		t.Fatal(err)
	}
	s.Close()
	s, _ = mustOpen(t, path, opts)
	if v, _ := s.Get("log:today"); v != "ab" {
		t.Fatalf("after compaction and restart: %q, want \"ab\"", v)
	}
	if left, has, _ := s.TTL("log:today"); !has || left != 45*time.Second {
		t.Fatalf("after compaction and restart TTL = %v, %v; want 45s", left, has)
	}

	// Once it has expired, Append starts again from "" with no deadline.
	c.advance(45 * time.Second)
	if n, _ := s.Append("log:today", "c"); n != 1 {
		t.Fatalf("Append after expiry = %d, want 1", n)
	}
	if v, _ := s.Get("log:today"); v != "c" {
		t.Fatalf("after expiry: %q, want \"c\"", v)
	}
	if _, has, _ := s.TTL("log:today"); has {
		t.Fatal("Append to an expired key kept the old deadline")
	}

	// Appending nothing to an existing key writes nothing.
	before := s.Stats().LogBytes
	if n, _ := s.Append("log:today", ""); n != 1 {
		t.Fatalf("Append(\"\") = %d, want 1", n)
	}
	if after := s.Stats().LogBytes; after != before {
		t.Fatalf("Append of \"\" grew the log from %d to %d bytes", before, after)
	}
}

func TestAppendTooLarge(t *testing.T) {
	s := New()
	big := strings.Repeat("x", aol.MaxFieldSize-1)
	s.Set("big", big)
	if n, err := s.Append("big", "y"); n != aol.MaxFieldSize || err != nil {
		t.Fatalf("Append up to the limit = %d, %v", n, err)
	}
	if _, err := s.Append("big", "z"); err != aol.ErrTooLarge {
		t.Fatalf("Append past the limit: err = %v, want ErrTooLarge", err)
	}
	if n := s.StrLen("big"); n != aol.MaxFieldSize {
		t.Fatalf("a refused Append changed the length to %d", n)
	}

	// With a deadline the value has to leave room for it in the record.
	s.SetWith("bigttl", big[:aol.MaxFieldSize-aol.DeadlineSize], SetOptions{TTL: time.Hour})
	if _, err := s.Append("bigttl", "z"); err != aol.ErrTooLarge {
		t.Fatalf("Append past the limit with a deadline: err = %v, want ErrTooLarge", err)
	}
	if n := s.StrLen("bigttl"); n != aol.MaxFieldSize-aol.DeadlineSize {
		t.Fatalf("a refused Append changed the length to %d", n)
	}
}

func TestAppendNoLostUpdates(t *testing.T) {
	path := filepath.Join(t.TempDir(), "append.log")
	s, _ := mustOpen(t, path, Options{Sync: false})
	const workers, each = 8, 200
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			piece := string(rune('a' + w))
			for i := 0; i < each; i++ {
				if _, err := s.Append("trail", piece); err != nil {
					t.Error(err)
					return
				}
			}
		}(w)
	}
	wg.Wait()
	check := func(when string) {
		v, _ := s.Get("trail")
		if len(v) != workers*each {
			t.Fatalf("%s: length %d, want %d: some appends were lost", when, len(v), workers*each)
		}
		for w := 0; w < workers; w++ {
			if n := strings.Count(v, string(rune('a'+w))); n != each {
				t.Fatalf("%s: worker %d's piece is in there %d times, want %d", when, w, n, each)
			}
		}
	}
	check("before restart")
	s.Close()
	s, _ = mustOpen(t, path, Options{Sync: false})
	defer s.Close()
	check("after restart")
}

func TestStrLen(t *testing.T) {
	c := newClock()
	s, _ := mustOpen(t, filepath.Join(t.TempDir(), "strlen.log"), Options{Sync: false, Now: c.now})
	defer s.Close()
	s.Set("name", "raiyaan")
	s.Set("cafe", "café")
	s.Set("empty", "")
	s.SetWith("soon", "gone", SetOptions{TTL: time.Second})
	for k, want := range map[string]int{"name": 7, "cafe": 5, "empty": 0, "missing": 0, "soon": 4} {
		if got := s.StrLen(k); got != want {
			t.Errorf("StrLen(%s) = %d, want %d", k, got, want)
		}
	}
	c.advance(time.Second)
	if got := s.StrLen("soon"); got != 0 {
		t.Errorf("StrLen of an expired key = %d, want 0", got)
	}
}

func TestGetRange(t *testing.T) {
	s := New()
	s.Set("k", "This is a string")
	cases := []struct {
		start, end int64
		want       string
	}{
		// The examples from the Redis docs.
		{0, 3, "This"},
		{-3, -1, "ing"},
		{0, -1, "This is a string"},
		{10, 100, "string"},
		// Edges.
		{0, 0, "T"},
		{15, 15, "g"},
		{0, 16, "This is a string"}, // end one past the last byte
		{-17, 3, "This"},            // start one before the first byte
		{16, 20, ""},                // starts past the end
		{5, 4, ""},                  // start after end
		{-1, -1, "g"},               // the last byte
		{-100, 3, "This"},
		{-100, -100, ""}, // both before the start
		{-100, -16, "T"},
		{-100, -17, ""},
		{3, -14, ""}, // -14 is byte 2, before start
		{math.MinInt64, math.MaxInt64, "This is a string"},
		{math.MaxInt64, math.MinInt64, ""},
	}
	for _, c := range cases {
		if got := s.GetRange("k", c.start, c.end); got != c.want {
			t.Errorf("GetRange(k, %d, %d) = %q, want %q", c.start, c.end, got, c.want)
		}
	}
	if got := s.GetRange("missing", 0, -1); got != "" {
		t.Errorf("GetRange on a missing key = %q", got)
	}
	s.Set("empty", "")
	if got := s.GetRange("empty", 0, -1); got != "" {
		t.Errorf("GetRange on an empty value = %q", got)
	}
	s.Set("cafe", "café")
	if got := s.GetRange("cafe", 0, 3); got != "caf\xc3" {
		t.Errorf("GetRange counts bytes: got %q, want \"caf\\xc3\"", got)
	}
}

// FuzzByteRange checks byteRange against a slow version that walks every
// byte and keeps the ones whose position falls in the range.
func FuzzByteRange(f *testing.F) {
	f.Add("This is a string", int64(0), int64(-1))
	f.Add("abc", int64(-5), int64(1))
	f.Add("", int64(0), int64(0))
	f.Add("x", int64(math.MinInt64), int64(math.MaxInt64))
	f.Fuzz(func(t *testing.T, v string, start, end int64) {
		got := byteRange(v, start, end)
		n := int64(len(v))
		// Resolve negative offsets the same way, then keep every byte i with
		// start <= i <= end.
		if start < 0 {
			start += n
		}
		if end < 0 {
			end += n
		}
		var want []byte
		for i := int64(0); i < n; i++ {
			if start <= i && i <= end {
				want = append(want, v[i])
			}
		}
		if got != string(want) {
			t.Fatalf("byteRange(%q, %d, %d) = %q, want %q", v, start, end, got, want)
		}
	})
}
