// Command vaultdb-bench measures VaultDB's vector search: how fast HNSW is
// compared with exact search, and how often it finds the true nearest
// neighbours (recall), at different collection sizes and efSearch settings.
//
//	go run ./cmd/vaultdb-bench                         default run
//	go run ./cmd/vaultdb-bench -sizes 10000,100000 -dim 384 -csv results.csv
//
// Two kinds of data are generated:
//
//	clustered  points scattered around 100 random centres, which is how
//	           real text embeddings behave (topics form clusters)
//	uniform    points spread evenly through space: the hardest case for
//	           any approximate index
package main

import (
	"encoding/csv"
	"flag"
	"fmt"
	"math/rand"
	"os"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/ABDULRAIYAAN/vaultdb/internal/vector"
)

type result struct {
	data       string
	n, dim     int
	ef         int
	buildSec   float64
	flatUs     float64 // average exact search time, microseconds
	hnswUs     float64 // average HNSW search time, microseconds
	recall     float64
	bytesPerVc float64
}

func main() {
	sizes := flag.String("sizes", "10000,50000,100000", "collection sizes to test, comma-separated")
	dim := flag.Int("dim", 128, "vector dimensions")
	queries := flag.Int("queries", 200, "queries per measurement")
	k := flag.Int("k", 10, "neighbours to find")
	efs := flag.String("ef", "16,32,64,128,256", "efSearch values to test")
	data := flag.String("data", "clustered,uniform", "datasets: clustered, uniform")
	csvPath := flag.String("csv", "", "also write results to this CSV file")
	flag.Parse()

	var results []result
	for _, kind := range strings.Split(*data, ",") {
		for _, n := range parseInts(*sizes) {
			results = append(results, run(kind, n, *dim, *queries, *k, parseInts(*efs))...)
		}
	}

	fmt.Printf("\n| Data | Vectors | efSearch | Exact search | HNSW search | Speed-up | Recall@%d |\n", *k)
	fmt.Println("|---|---|---|---|---|---|---|")
	for _, r := range results {
		fmt.Printf("| %s | %s | %d | %s | %s | %.0fx | %.1f%% |\n",
			r.data, comma(r.n), r.ef, dur(r.flatUs), dur(r.hnswUs), r.flatUs/r.hnswUs, 100*r.recall)
	}

	if *csvPath != "" {
		if err := writeCSV(*csvPath, results); err != nil {
			fmt.Fprintln(os.Stderr, "csv:", err)
			os.Exit(1)
		}
		fmt.Println("\nwrote", *csvPath)
	}
}

func run(kind string, n, dim, nq, k int, efs []int) []result {
	rng := rand.New(rand.NewSource(1))
	gen := generator(kind, dim, rng)
	vecs := make([]vector.Vector, n)
	for i := range vecs {
		vecs[i] = gen()
	}
	qs := make([]vector.Vector, nq)
	for i := range qs {
		qs[i] = gen()
	}

	fmt.Printf("%s, %s vectors x %d dims: building HNSW... ", kind, comma(n), dim)
	before := heapBytes()
	start := time.Now()
	h := vector.NewHNSW(vector.Cosine, vector.DefaultM, vector.DefaultEfConstruction, vector.DefaultEfSearch)
	for i, v := range vecs {
		h.Add(strconv.Itoa(i), v)
	}
	build := time.Since(start).Seconds()
	perVec := float64(heapBytes()-before) / float64(n)
	fmt.Printf("%.1fs\n", build)

	// Exact answers, timed: the ground truth for recall.
	f := vector.NewFlat(vector.Cosine)
	for i, v := range vecs {
		f.Add(strconv.Itoa(i), v)
	}
	truth := make([]map[string]bool, nq)
	start = time.Now()
	for i, q := range qs {
		truth[i] = map[string]bool{}
		for _, hit := range f.Search(q, k) {
			truth[i][hit.ID] = true
		}
	}
	flatUs := float64(time.Since(start).Microseconds()) / float64(nq)

	var out []result
	for _, ef := range efs {
		h.SetEfSearch(ef)
		found := 0
		start = time.Now()
		hits := make([][]vector.Hit, nq)
		for i, q := range qs {
			hits[i] = h.Search(q, k)
		}
		hnswUs := float64(time.Since(start).Microseconds()) / float64(nq)
		for i := range qs {
			for _, hit := range hits[i] {
				if truth[i][hit.ID] {
					found++
				}
			}
		}
		out = append(out, result{
			data: kind, n: n, dim: dim, ef: ef, buildSec: build,
			flatUs: flatUs, hnswUs: hnswUs, recall: float64(found) / float64(nq*k), bytesPerVc: perVec,
		})
		fmt.Printf("  ef=%-4d %8s per search, recall %.1f%%\n", ef, dur(hnswUs), 100*float64(found)/float64(nq*k))
	}
	return out
}

// generator returns a function producing random vectors of the given kind.
func generator(kind string, dim int, rng *rand.Rand) func() vector.Vector {
	if kind == "uniform" {
		return func() vector.Vector {
			v := make(vector.Vector, dim)
			for i := range v {
				v[i] = rng.Float32()*2 - 1
			}
			return v
		}
	}
	centres := make([]vector.Vector, 100)
	for c := range centres {
		centres[c] = make(vector.Vector, dim)
		for i := range centres[c] {
			centres[c][i] = float32(rng.NormFloat64())
		}
	}
	// Noise about 70% the size of the centre vectors: clusters overlap, as
	// real topics do, so the index can't win by just picking a cluster.
	spread := float32(0.7)
	return func() vector.Vector {
		c := centres[rng.Intn(len(centres))]
		v := make(vector.Vector, dim)
		for i := range v {
			v[i] = c[i] + spread*float32(rng.NormFloat64())
		}
		return v
	}
}

func heapBytes() uint64 {
	runtime.GC()
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	return m.HeapAlloc
}

func writeCSV(path string, rs []result) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	w := csv.NewWriter(f)
	w.Write([]string{"data", "vectors", "dim", "ef_search", "build_seconds", "exact_us", "hnsw_us", "speedup", "recall", "bytes_per_vector"})
	sort.SliceStable(rs, func(i, j int) bool { return rs[i].data < rs[j].data })
	for _, r := range rs {
		w.Write([]string{r.data, strconv.Itoa(r.n), strconv.Itoa(r.dim), strconv.Itoa(r.ef),
			fmt.Sprintf("%.2f", r.buildSec), fmt.Sprintf("%.1f", r.flatUs), fmt.Sprintf("%.1f", r.hnswUs),
			fmt.Sprintf("%.1f", r.flatUs/r.hnswUs), fmt.Sprintf("%.4f", r.recall), fmt.Sprintf("%.0f", r.bytesPerVc)})
	}
	w.Flush()
	if err := w.Error(); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

func parseInts(s string) []int {
	var out []int
	for _, p := range strings.Split(s, ",") {
		if n, err := strconv.Atoi(strings.TrimSpace(p)); err == nil && n > 0 {
			out = append(out, n)
		}
	}
	return out
}

func dur(us float64) string {
	if us >= 1000 {
		return fmt.Sprintf("%.2f ms", us/1000)
	}
	return fmt.Sprintf("%.0f µs", us)
}

func comma(n int) string {
	s := strconv.Itoa(n)
	for i := len(s) - 3; i > 0; i -= 3 {
		s = s[:i] + "," + s[i:]
	}
	return s
}
