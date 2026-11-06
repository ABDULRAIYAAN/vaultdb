package vector

import (
	"fmt"
	"math/rand"
	"sync"
	"testing"
)

// recall measures how many of the true k nearest neighbours HNSW finds,
// averaged over many queries. 1.0 means it always matches exact search.
func recall(t *testing.T, metric Metric, n, dim, k int) float64 {
	t.Helper()
	rng := rand.New(rand.NewSource(7))
	h := NewHNSW(metric, DefaultM, DefaultEfConstruction, DefaultEfSearch)
	f := NewFlat(metric)
	for i := 0; i < n; i++ {
		v := randVec(rng, dim)
		id := fmt.Sprint(i)
		h.Add(id, v)
		f.Add(id, v)
	}
	found, total := 0, 0
	for q := 0; q < 100; q++ {
		query := randVec(rng, dim)
		truth := map[string]bool{}
		for _, hit := range f.Search(query, k) {
			truth[hit.ID] = true
		}
		for _, hit := range h.Search(query, k) {
			if truth[hit.ID] {
				found++
			}
		}
		total += k
	}
	return float64(found) / float64(total)
}

func TestHNSWRecall(t *testing.T) {
	for _, m := range []Metric{Cosine, L2} {
		if r := recall(t, m, 3000, 32, 10); r < 0.95 {
			t.Errorf("%s recall@10 = %.3f, want at least 0.95", m, r)
		} else {
			t.Logf("%s recall@10 = %.3f", m, r)
		}
	}
}

func TestHNSWDistancesMatchFlat(t *testing.T) {
	rng := rand.New(rand.NewSource(3))
	h := NewHNSW(Cosine, 8, 50, 50)
	vs := map[string]Vector{}
	for i := 0; i < 200; i++ {
		vs[fmt.Sprint(i)] = randVec(rng, 8)
		h.Add(fmt.Sprint(i), vs[fmt.Sprint(i)])
	}
	q := randVec(rng, 8)
	for _, hit := range h.Search(q, 5) {
		if want := Distance(Cosine, q, vs[hit.ID]); !near(hit.Distance, want) {
			t.Fatalf("distance for %s = %v, exact %v", hit.ID, hit.Distance, want)
		}
	}
}

func TestHNSWEdgeCases(t *testing.T) {
	h := NewHNSW(L2, 4, 20, 20)
	if hits := h.Search(Vector{1}, 3); len(hits) != 0 {
		t.Fatal("empty index returned hits")
	}
	h.Add("only", Vector{5})
	if hits := h.Search(Vector{0}, 3); len(hits) != 1 || hits[0].ID != "only" || hits[0].Distance != 5 {
		t.Fatalf("single node: %+v", hits)
	}
	// Replacing an id moves it.
	h.Add("only", Vector{1})
	if hits := h.Search(Vector{0}, 3); len(hits) != 1 || hits[0].Distance != 1 || h.Len() != 1 {
		t.Fatalf("after replace: %+v len %d", hits, h.Len())
	}
	h.Remove("only")
	h.Remove("never-existed")
	if hits := h.Search(Vector{0}, 3); len(hits) != 0 || h.Len() != 0 {
		t.Fatalf("after remove: %+v", hits)
	}
	// Zero vector with cosine: no direction, distance 1 to everything.
	c := NewHNSW(Cosine, 4, 20, 20)
	c.Add("z", Vector{0, 0})
	if hits := c.Search(Vector{1, 0}, 1); len(hits) != 1 || !near(hits[0].Distance, 1) {
		t.Fatalf("zero vector: %+v", hits)
	}
}

// Deleted vectors never appear, and heavy deleting triggers a clean rebuild.
func TestHNSWDeletes(t *testing.T) {
	rng := rand.New(rand.NewSource(5))
	h := NewHNSW(L2, 8, 64, 64)
	for i := 0; i < 3000; i++ {
		h.Add(fmt.Sprint(i), randVec(rng, 8))
	}
	for i := 0; i < 2500; i++ { // delete most of them
		h.Remove(fmt.Sprint(i))
	}
	if h.Len() != 500 {
		t.Fatalf("Len = %d, want 500", h.Len())
	}
	if h.deleted >= 2500 {
		t.Fatalf("expected a rebuild to clear tombstones, still %d", h.deleted)
	}
	for q := 0; q < 50; q++ {
		for _, hit := range h.Search(randVec(rng, 8), 10) {
			var n int
			fmt.Sscan(hit.ID, &n)
			if n < 2500 {
				t.Fatalf("deleted vector %s returned", hit.ID)
			}
		}
	}
}

// Many searches at once must be safe (run with -race).
func TestHNSWConcurrentSearch(t *testing.T) {
	rng := rand.New(rand.NewSource(9))
	h := NewHNSW(Cosine, DefaultM, 100, 50)
	for i := 0; i < 1000; i++ {
		h.Add(fmt.Sprint(i), randVec(rng, 16))
	}
	queries := make([]Vector, 32)
	for i := range queries {
		queries[i] = randVec(rng, 16)
	}
	var wg sync.WaitGroup
	for _, q := range queries {
		wg.Add(1)
		go func(q Vector) {
			defer wg.Done()
			if len(h.Search(q, 5)) != 5 {
				t.Error("short result")
			}
		}(q)
	}
	wg.Wait()
}

func buildBench(b *testing.B, n, dim int) (*HNSW, *Flat, []Vector) {
	b.Helper()
	rng := rand.New(rand.NewSource(1))
	h := NewHNSW(Cosine, DefaultM, DefaultEfConstruction, DefaultEfSearch)
	f := NewFlat(Cosine)
	for i := 0; i < n; i++ {
		v := randVec(rng, dim)
		h.Add(fmt.Sprint(i), v)
		f.Add(fmt.Sprint(i), v)
	}
	qs := make([]Vector, 100)
	for i := range qs {
		qs[i] = randVec(rng, dim)
	}
	return h, f, qs
}

func BenchmarkSearch10k(b *testing.B) {
	h, f, qs := buildBench(b, 10000, 128)
	b.Run("flat", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			f.Search(qs[i%len(qs)], 10)
		}
	})
	b.Run("hnsw", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			h.Search(qs[i%len(qs)], 10)
		}
	})
}
