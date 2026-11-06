package btree

import (
	"fmt"
	"math/rand"
	"sort"
	"strings"
	"testing"
)

// check verifies every B-tree rule and fails the test if one is broken.
func check(t *testing.T, tr *BTree) {
	t.Helper()
	if tr.root == nil {
		if tr.length != 0 {
			t.Fatalf("empty tree reports length %d", tr.length)
		}
		return
	}
	leafDepth := -1
	count := 0
	var walk func(n *node, depth int, lo, hi string, isRoot bool)
	walk = func(n *node, depth int, lo, hi string, isRoot bool) {
		if len(n.items) > tr.maxItems() {
			t.Fatalf("node has %d items, max is %d", len(n.items), tr.maxItems())
		}
		if !isRoot && len(n.items) < tr.minItems() {
			t.Fatalf("node has %d items, min is %d", len(n.items), tr.minItems())
		}
		for i, it := range n.items {
			if i > 0 && n.items[i-1].key >= it.key {
				t.Fatalf("items out of order: %q then %q", n.items[i-1].key, it.key)
			}
			if (lo != "" && it.key <= lo) || (hi != "" && it.key >= hi) {
				t.Fatalf("key %q outside its range (%q, %q)", it.key, lo, hi)
			}
		}
		count += len(n.items)
		if n.leaf() {
			if leafDepth == -1 {
				leafDepth = depth
			} else if depth != leafDepth {
				t.Fatalf("leaves at depths %d and %d", leafDepth, depth)
			}
			return
		}
		if len(n.children) != len(n.items)+1 {
			t.Fatalf("%d items but %d children", len(n.items), len(n.children))
		}
		for i, c := range n.children {
			clo, chi := lo, hi
			if i > 0 {
				clo = n.items[i-1].key
			}
			if i < len(n.items) {
				chi = n.items[i].key
			}
			walk(c, depth+1, clo, chi, false)
		}
	}
	walk(tr.root, 0, "", "", true)
	if count != tr.length {
		t.Fatalf("counted %d keys, Len() says %d", count, tr.length)
	}
}

func key(i int) string { return fmt.Sprintf("k%05d", i) }

func TestBasics(t *testing.T) {
	tr := New(2)
	if _, ok := tr.Get("x"); ok || tr.Delete("x") || tr.Len() != 0 || tr.Height() != 0 {
		t.Fatal("empty tree misbehaves")
	}
	if !tr.Set("name", "raiyaan") || tr.Set("name", "Abdul Raiyaan") {
		t.Fatal("Set should report new keys only")
	}
	if v, ok := tr.Get("name"); !ok || v != "Abdul Raiyaan" || tr.Len() != 1 {
		t.Fatalf("Get = %q, %v", v, ok)
	}
	if !tr.Delete("name") || tr.Len() != 0 || tr.root != nil {
		t.Fatal("delete of only key failed")
	}
}

// Compare thousands of random operations against a plain Go map, checking
// every B-tree rule after each one. Small degrees force lots of splits,
// borrows and merges.
func TestRandomAgainstMap(t *testing.T) {
	for _, degree := range []int{2, 3, 4, 8} {
		t.Run(fmt.Sprintf("degree%d", degree), func(t *testing.T) {
			rng := rand.New(rand.NewSource(int64(degree)))
			tr := New(degree)
			ref := map[string]string{}
			for op := 0; op < 4000; op++ {
				k := key(rng.Intn(300))
				switch rng.Intn(3) {
				case 0, 1:
					v := fmt.Sprint(op)
					_, existed := ref[k]
					if tr.Set(k, v) == existed {
						t.Fatalf("Set(%q) new=%v but existed=%v", k, !existed, existed)
					}
					ref[k] = v
				case 2:
					_, existed := ref[k]
					if tr.Delete(k) != existed {
						t.Fatalf("Delete(%q) mismatch", k)
					}
					delete(ref, k)
				}
				check(t, tr)
			}
			for k, v := range ref {
				if got, ok := tr.Get(k); !ok || got != v {
					t.Fatalf("Get(%q) = %q, %v; want %q", k, got, ok, v)
				}
			}
			// Delete everything: the tree must shrink back to empty.
			for k := range ref {
				tr.Delete(k)
				check(t, tr)
			}
			if tr.Len() != 0 || tr.root != nil {
				t.Fatal("tree not empty after deleting every key")
			}
		})
	}
}

func collect(tr *BTree, from, to string) []string {
	var out []string
	tr.Ascend(from, to, func(k, _ string) bool { out = append(out, k); return true })
	return out
}

func TestAscend(t *testing.T) {
	tr := New(2)
	var all []string
	for i := 0; i < 200; i += 2 { // even numbers only, so gaps exist
		tr.Set(key(i), "")
		all = append(all, key(i))
	}
	sort.Strings(all)

	cases := []struct{ from, to string }{
		{"", ""},           // everything
		{key(50), key(60)}, // range on existing keys
		{key(51), key(61)}, // range starting in a gap
		{key(198), ""},     // last key only
		{"k99999", ""},     // past the end
		{"", key(0)},       // before the start
		{key(10), key(10)}, // empty range
	}
	for _, c := range cases {
		var want []string
		for _, k := range all {
			if k >= c.from && (c.to == "" || k < c.to) {
				want = append(want, k)
			}
		}
		got := collect(tr, c.from, c.to)
		if strings.Join(got, ",") != strings.Join(want, ",") {
			t.Errorf("Ascend(%q, %q) = %v, want %v", c.from, c.to, got, want)
		}
	}

	// Stopping early.
	n := 0
	tr.Ascend("", "", func(string, string) bool { n++; return n < 5 })
	if n != 5 {
		t.Errorf("early stop visited %d keys, want 5", n)
	}
}

func TestPrefixScan(t *testing.T) {
	tr := New(DefaultDegree)
	for _, k := range []string{"user:1", "user:2", "user:10", "users", "order:1", "user;", "user"} {
		tr.Set(k, "")
	}
	// Every key starting with "user:" sorts between "user:" and "user;"
	// (';' is the character right after ':').
	got := collect(tr, "user:", "user;")
	if strings.Join(got, ",") != "user:1,user:10,user:2" {
		t.Fatalf("prefix scan = %v", got)
	}
}

func TestStaysShallow(t *testing.T) {
	tr := New(DefaultDegree)
	for i := 0; i < 100000; i++ {
		tr.Set(key(i), "v")
	}
	check(t, tr)
	if h := tr.Height(); h > 4 {
		t.Fatalf("100k keys gave height %d, want at most 4", h)
	}
}

func BenchmarkSet(b *testing.B) {
	tr := New(DefaultDegree)
	for i := 0; i < b.N; i++ {
		tr.Set(key(i%1000000), "v")
	}
}

func BenchmarkGet(b *testing.B) {
	tr := New(DefaultDegree)
	for i := 0; i < 100000; i++ {
		tr.Set(key(i), "v")
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		tr.Get(key(i % 100000))
	}
}
