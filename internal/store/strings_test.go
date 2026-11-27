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

func TestSetRange(t *testing.T) {
	s := New()
	// The examples from the Redis docs.
	s.Set("key1", "Hello World")
	if n, err := s.SetRange("key1", 6, "Redis"); n != 11 || err != nil {
		t.Fatalf("SetRange(key1, 6, Redis) = %d, %v; want 11", n, err)
	}
	if v, _ := s.Get("key1"); v != "Hello Redis" {
		t.Fatalf("key1 = %q, want \"Hello Redis\"", v)
	}
	if n, err := s.SetRange("key2", 6, "Redis"); n != 11 || err != nil {
		t.Fatalf("SetRange(key2, 6, Redis) = %d, %v; want 11", n, err)
	}
	if v, _ := s.Get("key2"); v != "\x00\x00\x00\x00\x00\x00Redis" {
		t.Fatalf("key2 = %q, want six zero bytes then Redis", v)
	}

	steps := []struct {
		offset int64
		text   string
		want   int
		val    string
	}{
		{0, "abc", 3, "abc"},          // missing key, no padding
		{1, "X", 3, "aXc"},            // in the middle: the tail is kept
		{2, "YZ", 4, "aXYZ"},          // runs past the end: grows
		{4, "!", 5, "aXYZ!"},          // right at the end: like APPEND
		{7, "?", 8, "aXYZ!\x00\x00?"}, // past the end: zero-padded
		{0, "", 8, "aXYZ!\x00\x00?"},  // empty text: no change
		{5, "é", 8, "aXYZ!\xc3\xa9?"}, // offsets count bytes
	}
	for _, st := range steps {
		n, err := s.SetRange("k", st.offset, st.text)
		if n != st.want || err != nil {
			t.Fatalf("SetRange(k, %d, %q) = %d, %v; want %d", st.offset, st.text, n, err, st.want)
		}
		if v, _ := s.Get("k"); v != st.val {
			t.Fatalf("after SetRange(k, %d, %q): %q, want %q", st.offset, st.text, v, st.val)
		}
	}

	// An empty text on a missing key doesn't create it, unlike APPEND.
	if n, err := s.SetRange("nothing", 5, ""); n != 0 || err != nil {
		t.Fatalf("SetRange(nothing, 5, \"\") = %d, %v", n, err)
	}
	if s.Exists("nothing") {
		t.Fatal("SetRange of \"\" created a missing key")
	}

	// Patching one digit of a number leaves INCR working.
	s.Set("n", "100")
	s.SetRange("n", 0, "4")
	if got, err := s.IncrBy("n", 1); got != 401 || err != nil {
		t.Fatalf("IncrBy after SetRange = %d, %v; want 401", got, err)
	}

	if _, err := s.SetRange("k", -1, "x"); err != ErrOffsetRange {
		t.Fatalf("negative offset: err = %v, want ErrOffsetRange", err)
	}
	if _, err := s.SetRange("", 0, "x"); err != ErrEmptyKey {
		t.Fatalf("empty key: err = %v", err)
	}
	if v, _ := s.Get("k"); v != "aXYZ!\xc3\xa9?" {
		t.Fatalf("a refused SetRange changed the value to %q", v)
	}
}

func TestSetRangeKeepsDeadline(t *testing.T) {
	c := newClock()
	path := filepath.Join(t.TempDir(), "setrange.log")
	opts := Options{Sync: false, Now: c.now}
	s, _ := mustOpen(t, path, opts)
	defer func() { s.Close() }()

	s.SetWith("slot", "....", SetOptions{TTL: time.Minute})
	c.advance(15 * time.Second)
	if n, _ := s.SetRange("slot", 1, "ab"); n != 4 {
		t.Fatalf("SetRange = %d, want 4", n)
	}
	if left, has, _ := s.TTL("slot"); !has || left != 45*time.Second {
		t.Fatalf("TTL after SetRange = %v, %v; want 45s kept", left, has)
	}

	// The deadline went to the log with the value: it's still there after
	// a restart, and after a compaction followed by a restart.
	s.Close()
	s, _ = mustOpen(t, path, opts)
	if v, _ := s.Get("slot"); v != ".ab." {
		t.Fatalf("after restart: %q, want \".ab.\"", v)
	}
	if left, has, _ := s.TTL("slot"); !has || left != 45*time.Second {
		t.Fatalf("after restart TTL = %v, %v; want 45s", left, has)
	}
	if _, err := s.Compact(); err != nil {
		t.Fatal(err)
	}
	s.Close()
	s, _ = mustOpen(t, path, opts)
	if v, _ := s.Get("slot"); v != ".ab." {
		t.Fatalf("after compaction and restart: %q, want \".ab.\"", v)
	}
	if left, has, _ := s.TTL("slot"); !has || left != 45*time.Second {
		t.Fatalf("after compaction and restart TTL = %v, %v; want 45s", left, has)
	}

	// Writes that leave the bytes as they were don't touch the log.
	before := s.Stats().LogBytes
	for _, st := range []struct {
		offset int64
		text   string
	}{{0, ""}, {1, "ab"}, {0, ".ab."}} {
		if n, _ := s.SetRange("slot", st.offset, st.text); n != 4 {
			t.Fatalf("SetRange(slot, %d, %q) = %d, want 4", st.offset, st.text, n)
		}
	}
	if after := s.Stats().LogBytes; after != before {
		t.Fatalf("SetRange with no change grew the log from %d to %d bytes", before, after)
	}

	// Once it has expired, SetRange starts again from "" with no deadline.
	c.advance(45 * time.Second)
	if n, _ := s.SetRange("slot", 2, "z"); n != 3 {
		t.Fatalf("SetRange after expiry = %d, want 3", n)
	}
	if v, _ := s.Get("slot"); v != "\x00\x00z" {
		t.Fatalf("after expiry: %q, want \"\\x00\\x00z\"", v)
	}
	if _, has, _ := s.TTL("slot"); has {
		t.Fatal("SetRange on an expired key kept the old deadline")
	}
}

func TestSetRangeTooLarge(t *testing.T) {
	s := New()
	last := int64(aol.MaxFieldSize - 1)
	if n, err := s.SetRange("big", last, "y"); n != aol.MaxFieldSize || err != nil {
		t.Fatalf("SetRange up to the limit = %d, %v", n, err)
	}
	for _, st := range []struct {
		offset int64
		text   string
	}{
		{last, "yz"},
		{last + 1, "z"},
		{math.MaxInt64, "z"}, // must be refused, not overflow or allocate
		{math.MaxInt64 - 1, "zz"},
	} {
		if _, err := s.SetRange("big", st.offset, st.text); err != aol.ErrTooLarge {
			t.Fatalf("SetRange(big, %d, %q): err = %v, want ErrTooLarge", st.offset, st.text, err)
		}
	}
	if n := s.StrLen("big"); n != aol.MaxFieldSize {
		t.Fatalf("a refused SetRange changed the length to %d", n)
	}
	// Overwriting inside a full-size value is still fine.
	if n, err := s.SetRange("big", 0, "start"); n != aol.MaxFieldSize || err != nil {
		t.Fatalf("SetRange inside a full value = %d, %v", n, err)
	}
	if _, err := s.SetRange("missing", math.MaxInt64, "z"); err != aol.ErrTooLarge {
		t.Fatalf("huge offset on a missing key: err = %v", err)
	}
	if s.Exists("missing") {
		t.Fatal("a refused SetRange created the key")
	}

	// With a deadline the value has to leave room for it in the record.
	room := int64(aol.MaxFieldSize - aol.DeadlineSize)
	s.SetWith("bigttl", "", SetOptions{TTL: time.Hour})
	if n, err := s.SetRange("bigttl", room-1, "y"); int64(n) != room || err != nil {
		t.Fatalf("SetRange up to the limit with a deadline = %d, %v", n, err)
	}
	if _, err := s.SetRange("bigttl", room, "z"); err != aol.ErrTooLarge {
		t.Fatalf("SetRange past the limit with a deadline: err = %v, want ErrTooLarge", err)
	}
	if n := int64(s.StrLen("bigttl")); n != room {
		t.Fatalf("a refused SetRange changed the length to %d", n)
	}
}

func TestSetRangeNoLostUpdates(t *testing.T) {
	path := filepath.Join(t.TempDir(), "setrange.log")
	s, _ := mustOpen(t, path, Options{Sync: false})
	const workers, each = 8, 100
	// Each worker fills its own stretch of one shared value, one byte at a
	// time, so a lost update would leave a zero byte behind.
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			piece := string(rune('a' + w))
			for i := 0; i < each; i++ {
				if _, err := s.SetRange("grid", int64(w*each+i), piece); err != nil {
					t.Error(err)
					return
				}
			}
		}(w)
	}
	wg.Wait()
	var want strings.Builder
	for w := 0; w < workers; w++ {
		want.WriteString(strings.Repeat(string(rune('a'+w)), each))
	}
	check := func(when string) {
		if v, _ := s.Get("grid"); v != want.String() {
			t.Fatalf("%s: got %q, want %q: some writes were lost", when, v, want.String())
		}
	}
	check("before restart")
	s.Close()
	s, _ = mustOpen(t, path, Options{Sync: false})
	defer s.Close()
	check("after restart")
}

// FuzzOverwriteAt checks overwriteAt against a slow version that works out
// each byte of the result on its own.
func FuzzOverwriteAt(f *testing.F) {
	f.Add("Hello World", int64(6), "Redis", 64)
	f.Add("", int64(3), "x", 64)
	f.Add("abc", int64(0), "", 64)
	f.Add("abc", int64(1), "longer", 5)
	f.Add("abc", int64(math.MaxInt64), "x", 64)
	f.Fuzz(func(t *testing.T, cur string, offset int64, text string, limit int) {
		if limit < 0 || limit > 1<<16 || offset > 1<<20 {
			t.Skip() // keep the slow version cheap
		}
		got, err := overwriteAt(cur, offset, text, limit)
		if offset < 0 {
			if err != ErrOffsetRange {
				t.Fatalf("offset %d: err = %v, want ErrOffsetRange", offset, err)
			}
			return
		}
		n := int64(len(cur))
		if end := offset + int64(len(text)); end > n {
			n = end
		}
		if n > int64(limit) {
			if err != aol.ErrTooLarge {
				t.Fatalf("length %d over limit %d: err = %v, want ErrTooLarge", n, limit, err)
			}
			return
		}
		if err != nil {
			t.Fatalf("overwriteAt(%q, %d, %q, %d): unexpected err %v", cur, offset, text, limit, err)
		}
		want := make([]byte, n)
		for i := int64(0); i < n; i++ {
			switch {
			case i >= offset && i < offset+int64(len(text)):
				want[i] = text[i-offset]
			case i < int64(len(cur)):
				want[i] = cur[i]
			}
		}
		if got != string(want) {
			t.Fatalf("overwriteAt(%q, %d, %q, %d) = %q, want %q", cur, offset, text, limit, got, want)
		}
	})
}
