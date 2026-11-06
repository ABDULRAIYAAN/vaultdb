package store

import (
	"fmt"
	"sync"
	"testing"
)

func TestSetGet(t *testing.T) {
	s := New()
	if err := s.Set("name", "raiyaan"); err != nil {
		t.Fatal(err)
	}
	got, err := s.Get("name")
	if err != nil || got != "raiyaan" {
		t.Fatalf("Get = %q, %v; want raiyaan, nil", got, err)
	}
}

func TestOverwrite(t *testing.T) {
	s := New()
	s.Set("k", "a")
	s.Set("k", "b")
	if got, _ := s.Get("k"); got != "b" {
		t.Fatalf("got %q, want b", got)
	}
	if s.Len() != 1 {
		t.Fatalf("Len = %d, want 1", s.Len())
	}
}

func TestGetMissing(t *testing.T) {
	s := New()
	if _, err := s.Get("nope"); err != ErrNotFound {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

func TestEmptyKey(t *testing.T) {
	s := New()
	if err := s.Set("", "x"); err != ErrEmptyKey {
		t.Fatalf("err = %v, want ErrEmptyKey", err)
	}
}

func TestDeleteAndExists(t *testing.T) {
	s := New()
	s.Set("k", "v")
	if !s.Exists("k") {
		t.Fatal("expected k to exist")
	}
	if ok, err := s.Delete("k"); !ok || err != nil {
		t.Fatalf("Delete existing = %v, %v", ok, err)
	}
	if s.Exists("k") {
		t.Fatal("k still exists after delete")
	}
	if ok, _ := s.Delete("k"); ok {
		t.Fatal("Delete returned true for missing key")
	}
}

func TestKeysSorted(t *testing.T) {
	s := New()
	for _, k := range []string{"c", "a", "b"} {
		s.Set(k, "x")
	}
	got := fmt.Sprint(s.Keys())
	if got != "[a b c]" {
		t.Fatalf("Keys = %s, want [a b c]", got)
	}
}

func TestClear(t *testing.T) {
	s := New()
	s.Set("a", "1")
	s.Clear()
	if s.Len() != 0 {
		t.Fatalf("Len = %d after Clear", s.Len())
	}
}

// Many goroutines reading and writing at once. Run with -race to prove the
// locking is correct.
func TestConcurrentAccess(t *testing.T) {
	s := New()
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			key := fmt.Sprintf("key%d", i)
			s.Set(key, "v")
			s.Get(key)
			s.Exists(key)
			s.Keys()
		}(i)
	}
	wg.Wait()
	if s.Len() != 50 {
		t.Fatalf("Len = %d, want 50", s.Len())
	}
}

func TestScanPrefixAndLimits(t *testing.T) {
	s := New()
	for i := 0; i < 50; i++ {
		s.Set(fmt.Sprintf("item:%02d", i), fmt.Sprint(i))
	}
	s.Set("other", "x")

	pairs, more := s.Scan("item:10", "item:15", 0)
	if len(pairs) != 5 || pairs[0].Key != "item:10" || pairs[4].Key != "item:14" || more {
		t.Fatalf("Scan = %v, more=%v", pairs, more)
	}
	pairs, more = s.Prefix("item:", 10)
	if len(pairs) != 10 || !more || pairs[9].Key != "item:09" {
		t.Fatalf("Prefix with limit = %d pairs, more=%v", len(pairs), more)
	}
	pairs, more = s.Prefix("item:", 50)
	if len(pairs) != 50 || more {
		t.Fatalf("exact limit: %d pairs, more=%v", len(pairs), more)
	}
	if pairs, _ := s.Prefix("nothing", 0); len(pairs) != 0 {
		t.Fatalf("Prefix(nothing) = %v", pairs)
	}
}
