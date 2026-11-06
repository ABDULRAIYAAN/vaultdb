package vector

import (
	"fmt"
	"math"
	"math/rand"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/ABDULRAIYAAN/vaultdb/internal/store"
)

func near(a, b float32) bool { return math.Abs(float64(a-b)) < 1e-5 }

func TestDistance(t *testing.T) {
	cases := []struct {
		m    Metric
		a, b Vector
		want float32
	}{
		{Cosine, Vector{1, 0}, Vector{1, 0}, 0},  // same direction
		{Cosine, Vector{1, 0}, Vector{5, 0}, 0},  // length doesn't matter
		{Cosine, Vector{1, 0}, Vector{0, 1}, 1},  // at right angles: unrelated
		{Cosine, Vector{1, 0}, Vector{-1, 0}, 2}, // opposite
		{Cosine, Vector{0, 0}, Vector{1, 0}, 1},  // zero vector
		{L2, Vector{0, 0}, Vector{3, 4}, 5},      // 3-4-5 triangle
		{L2, Vector{1, 2, 3}, Vector{1, 2, 3}, 0},
	}
	for _, c := range cases {
		if got := Distance(c.m, c.a, c.b); !near(got, c.want) {
			t.Errorf("%s(%v, %v) = %v, want %v", c.m, c.a, c.b, got, c.want)
		}
	}
}

func TestParseAndString(t *testing.T) {
	v, err := Parse("[0.5, -1, 2.25]")
	if err != nil || v.String() != "[0.5, -1, 2.25]" {
		t.Fatalf("Parse = %v, %v", v, err)
	}
	if v, err := Parse("1,2,3"); err != nil || len(v) != 3 {
		t.Fatalf("no brackets: %v, %v", v, err)
	}
	for _, bad := range []string{"", "[]", "[1, x]", "[1,,2]", "[NaN]", "[Inf]"} {
		if _, err := Parse(bad); err == nil {
			t.Errorf("Parse(%q) should fail", bad)
		}
	}
}

func TestEncodeRoundTrip(t *testing.T) {
	v := Vector{1.5, -0.25, 3e-7, 42}
	gv, gp, err := decodeItem(encodeItem(v, "The car broke down"))
	if err != nil || gv.String() != v.String() || gp != "The car broke down" {
		t.Fatalf("round trip: %v %q %v", gv, gp, err)
	}
	for _, bad := range []string{"", "ab", "\xff\xff\xff\x7f"} {
		if _, _, err := decodeItem(bad); err == nil {
			t.Errorf("decodeItem(%q) should fail", bad)
		}
	}
}

func randVec(rng *rand.Rand, dim int) Vector {
	v := make(Vector, dim)
	for i := range v {
		v[i] = rng.Float32()*2 - 1
	}
	return v
}

// The flat index must return exactly what sorting every distance returns.
func TestFlatSearchIsExact(t *testing.T) {
	rng := rand.New(rand.NewSource(1))
	for _, m := range []Metric{Cosine, L2} {
		f := NewFlat(m)
		all := map[string]Vector{}
		for i := 0; i < 500; i++ {
			id := fmt.Sprintf("v%03d", i)
			all[id] = randVec(rng, 16)
			f.Add(id, all[id])
		}
		for trial := 0; trial < 20; trial++ {
			q := randVec(rng, 16)
			var want []Hit
			for id, v := range all {
				want = append(want, Hit{ID: id, Distance: Distance(m, q, v)})
			}
			sort.Slice(want, func(i, j int) bool { return want[i].Distance < want[j].Distance })
			got := f.Search(q, 10)
			for i := range got {
				if got[i].ID != want[i].ID {
					t.Fatalf("%s rank %d: got %s, want %s", m, i+1, got[i].ID, want[i].ID)
				}
			}
		}
		if got := f.Search(randVec(rng, 16), 1000); len(got) != 500 {
			t.Fatalf("k larger than collection returned %d hits", len(got))
		}
	}
}

func TestManager(t *testing.T) {
	m := NewManager(store.New())
	if err := m.Create("docs", 3, Cosine, IndexFlat); err != nil {
		t.Fatal(err)
	}
	m.Add("docs", "car", Vector{1, 0, 0}, "The car broke down")
	m.Add("docs", "vehicle", Vector{0.9, 0.1, 0}, "My vehicle stopped working")
	m.Add("docs", "cake", Vector{0, 0, 1}, "I love chocolate cake")

	hits, err := m.Search("docs", Vector{1, 0.05, 0}, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 2 || hits[0].ID != "car" || hits[1].ID != "vehicle" || hits[0].Payload != "The car broke down" {
		t.Fatalf("hits = %+v", hits)
	}

	// Replacing keeps the count; deleting removes it from results.
	if isNew, _ := m.Add("docs", "car", Vector{0, 1, 0}, "moved"); isNew {
		t.Fatal("replacing should not count as new")
	}
	if ok, _ := m.Delete("docs", "vehicle"); !ok {
		t.Fatal("delete failed")
	}
	hits, _ = m.Search("docs", Vector{1, 0, 0}, 3)
	if len(hits) != 2 {
		t.Fatalf("after delete: %+v", hits)
	}
	v, payload, err := m.Get("docs", "car")
	if err != nil || v.String() != "[0, 1, 0]" || payload != "moved" {
		t.Fatalf("Get = %v %q %v", v, payload, err)
	}

	errs := map[string]error{
		"wrong dim on add":    func() error { _, err := m.Add("docs", "x", Vector{1, 2}, ""); return err }(),
		"wrong dim on search": func() error { _, err := m.Search("docs", Vector{1}, 1); return err }(),
		"missing collection":  func() error { _, err := m.Search("nope", Vector{1, 2, 3}, 1); return err }(),
		"duplicate create":    m.Create("docs", 3, Cosine, IndexFlat),
		"bad name":            m.Create("bad:name", 3, Cosine, IndexHNSW),
		"bad dim":             m.Create("big", MaxDim+1, Cosine, IndexHNSW),
		"empty id":            func() error { _, err := m.Add("docs", "", Vector{1, 2, 3}, ""); return err }(),
	}
	for what, err := range errs {
		if err == nil {
			t.Errorf("%s: expected an error", what)
		}
	}

	list, _ := m.List()
	if len(list) != 1 || list[0].Count != 2 || list[0].Dim != 3 {
		t.Fatalf("List = %+v", list)
	}
	if n, err := m.Drop("docs"); err != nil || n != 2 {
		t.Fatalf("Drop = %d, %v", n, err)
	}
	if list, _ := m.List(); len(list) != 0 {
		t.Fatalf("after drop: %+v", list)
	}
}

// Vectors are keys, so they survive a restart and the index is rebuilt.
func TestVectorsSurviveRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "vaultdb.log")
	st, _, err := store.Open(path, store.DefaultOptions())
	if err != nil {
		t.Fatal(err)
	}
	m := NewManager(st)
	m.Create("notes", 2, L2, IndexHNSW)
	m.Add("notes", "a", Vector{0, 0}, "origin")
	m.Add("notes", "b", Vector{10, 10}, "far away")
	st.Close()

	st2, _, err := store.Open(path, store.DefaultOptions())
	if err != nil {
		t.Fatal(err)
	}
	defer st2.Close()
	hits, err := NewManager(st2).Search("notes", Vector{1, 1}, 1)
	if err != nil || len(hits) != 1 || hits[0].ID != "a" || hits[0].Payload != "origin" {
		t.Fatalf("after restart: %+v, %v", hits, err)
	}
}

func TestIDsMayContainColons(t *testing.T) {
	m := NewManager(store.New())
	m.Create("c", 1, L2, IndexHNSW)
	m.Add("c", "doc:1:chunk:3", Vector{1}, "x")
	m.Reset() // force a reload from the store
	if hits, _ := m.Search("c", Vector{1}, 1); len(hits) != 1 || hits[0].ID != "doc:1:chunk:3" {
		t.Fatalf("hits = %+v", hits)
	}
	if !strings.Contains(fmt.Sprint(Vector{1}), "1") {
		t.Fatal("String")
	}
}

func BenchmarkFlatSearch10k(b *testing.B) {
	rng := rand.New(rand.NewSource(1))
	f := NewFlat(Cosine)
	for i := 0; i < 10000; i++ {
		f.Add(fmt.Sprint(i), randVec(rng, 128))
	}
	q := randVec(rng, 128)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		f.Search(q, 10)
	}
}
