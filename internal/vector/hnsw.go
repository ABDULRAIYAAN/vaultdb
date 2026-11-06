package vector

import (
	"container/heap"
	"math"
	"math/rand"
	"sort"
	"sync"
)

// HNSW (Hierarchical Navigable Small World) finds nearest neighbours without
// checking every vector. It is the index behind Pinecone, Weaviate, Qdrant
// and pgvector. Paper: Malkov & Yashunin, 2016, arXiv:1603.09320.
//
// The idea is a stack of graphs, like a map with motorways on top and
// local roads at the bottom:
//
//	layer 2:   A ----------------------- F               few nodes, long jumps
//	layer 1:   A ------- C ------- E --- F
//	layer 0:   A -- B -- C -- D -- E --- F -- G -- H     every node, short links
//
// Every vector is on layer 0. Each one also appears on higher layers with
// falling probability (half as many per layer up, roughly), like a coin
// flipped until it lands tails.
//
// Searching: start at the top layer, walk greedily towards the query (always
// step to whichever neighbour is closer) until no neighbour is closer, then
// drop a layer and repeat. On layer 0, do a wider search that keeps the ef
// best candidates. Each layer narrows the area, so a search touches a few
// hundred vectors instead of all of them.
//
// Adding: pick the new vector's top layer at random, search down to it the
// same way, then on each layer from there to 0, link it to its nearest
// neighbours. Links go both ways; if a neighbour ends up with too many, it
// keeps only the best ones.
//
// The result is approximate: it can occasionally miss a true nearest
// neighbour. "Recall" measures how often it finds the right ones; with the
// defaults below it is typically above 95%.
type HNSW struct {
	metric Metric
	m      int     // links per node on layers above 0
	m0     int     // links per node on layer 0 (twice as many: it's the detailed map)
	efCons int     // candidates considered while adding (higher = better graph, slower adds)
	efSrch int     // candidates considered while searching (higher = better recall, slower)
	levelK float64 // 1/ln(m): sets how quickly higher layers thin out
	rng    *rand.Rand

	nodes   []*hnode
	ids     map[string]int32 // live id -> node
	entry   int32            // node to start every search from (-1 when empty)
	top     int              // highest layer in the graph
	deleted int              // nodes marked deleted but still in the graph

	visited sync.Pool // reusable "already seen" sets, so searches can run in parallel
}

type hnode struct {
	id      string
	vec     Vector
	links   [][]int32 // links[layer] = neighbours on that layer
	deleted bool
}

// HNSW defaults, the values most HNSW libraries use.
const (
	DefaultM              = 16
	DefaultEfConstruction = 200
	DefaultEfSearch       = 64
)

// NewHNSW creates an empty HNSW index.
func NewHNSW(metric Metric, m, efConstruction, efSearch int) *HNSW {
	if m < 2 {
		m = DefaultM
	}
	if efConstruction < m {
		efConstruction = DefaultEfConstruction
	}
	if efSearch < 1 {
		efSearch = DefaultEfSearch
	}
	h := &HNSW{
		metric: metric, m: m, m0: 2 * m, efCons: efConstruction, efSrch: efSearch,
		levelK: 1 / math.Log(float64(m)),
		rng:    rand.New(rand.NewSource(42)), // fixed seed: the same data builds the same graph
		ids:    map[string]int32{},
		entry:  -1,
	}
	h.visited.New = func() any { return &visitedSet{} }
	return h
}

// Len returns the number of live vectors.
func (h *HNSW) Len() int { return len(h.ids) }

// prepare stores cosine vectors at length 1, so cosine distance becomes the
// cheaper 1 - (a·b) at search time.
func (h *HNSW) prepare(v Vector) Vector {
	if h.metric != Cosine {
		return v
	}
	var sum float32
	for _, f := range v {
		sum += f * f
	}
	out := make(Vector, len(v))
	if sum == 0 {
		return out
	}
	inv := float32(1 / math.Sqrt(float64(sum)))
	for i, f := range v {
		out[i] = f * inv
	}
	return out
}

func (h *HNSW) dist(a, b Vector) float32 {
	if h.metric == Cosine {
		var dot float32
		for i := range a {
			dot += a[i] * b[i]
		}
		return 1 - dot
	}
	return Distance(L2, a, b)
}

// randomLevel draws a layer: 0 most of the time, 1 about 1 in m times,
// 2 about 1 in m*m times, and so on.
func (h *HNSW) randomLevel() int {
	return int(math.Floor(-math.Log(1-h.rng.Float64()) * h.levelK))
}

// Add inserts a vector. Adding an id that exists replaces it.
func (h *HNSW) Add(id string, v Vector) {
	if _, ok := h.ids[id]; ok {
		h.Remove(id)
	}
	vec := h.prepare(v)
	level := h.randomLevel()
	n := &hnode{id: id, vec: vec, links: make([][]int32, level+1)}
	idx := int32(len(h.nodes))
	h.nodes = append(h.nodes, n)
	h.ids[id] = idx

	if h.entry < 0 {
		h.entry, h.top = idx, level
		return
	}

	// 1. Above the new node's top layer: greedy walk to get close.
	ep := h.entry
	for l := h.top; l > level; l-- {
		ep = h.greedy(vec, ep, l)
	}
	// 2. From its top layer down to 0: find neighbours and link both ways.
	eps := []candidate{{ep, h.dist(vec, h.nodes[ep].vec)}}
	for l := min(level, h.top); l >= 0; l-- {
		found := h.searchLayer(vec, eps, h.efCons, l)
		maxLinks := h.maxLinks(l)
		chosen := h.selectNeighbours(found, h.m)
		n.links[l] = make([]int32, 0, len(chosen))
		for _, c := range chosen {
			n.links[l] = append(n.links[l], c.node)
			h.link(c.node, idx, l, maxLinks)
		}
		eps = found
	}
	if level > h.top {
		h.entry, h.top = idx, level
	}
}

func (h *HNSW) maxLinks(layer int) int {
	if layer == 0 {
		return h.m0
	}
	return h.m
}

// link adds a link from node `from` to node `to` on layer l. If `from` now
// has too many links, it keeps the best ones (chosen by the same heuristic
// used for new nodes).
func (h *HNSW) link(from, to int32, l, maxLinks int) {
	f := h.nodes[from]
	f.links[l] = append(f.links[l], to)
	if len(f.links[l]) <= maxLinks {
		return
	}
	cands := make([]candidate, len(f.links[l]))
	for i, nb := range f.links[l] {
		cands[i] = candidate{nb, h.dist(f.vec, h.nodes[nb].vec)}
	}
	sort.Slice(cands, func(i, j int) bool { return cands[i].dist < cands[j].dist })
	kept := h.selectNeighbours(cands, maxLinks)
	f.links[l] = f.links[l][:0]
	for _, c := range kept {
		f.links[l] = append(f.links[l], c.node)
	}
}

// selectNeighbours picks up to m links from candidates (sorted nearest
// first). Plain "closest m" tends to link a node only to one tight cluster.
// The heuristic from the paper skips a candidate if it is closer to an
// already chosen neighbour than to the new node: that neighbour already
// leads there. This spreads links in different directions, which keeps the
// graph navigable. Leftover slots are then filled with the closest skipped
// candidates.
func (h *HNSW) selectNeighbours(cands []candidate, m int) []candidate {
	if len(cands) <= m {
		return cands
	}
	chosen := make([]candidate, 0, m)
	var skipped []candidate
	for _, c := range cands {
		if len(chosen) == m {
			break
		}
		good := true
		for _, r := range chosen {
			if h.dist(h.nodes[c.node].vec, h.nodes[r.node].vec) < c.dist {
				good = false
				break
			}
		}
		if good {
			chosen = append(chosen, c)
		} else {
			skipped = append(skipped, c)
		}
	}
	for _, c := range skipped {
		if len(chosen) == m {
			break
		}
		chosen = append(chosen, c)
	}
	return chosen
}

// greedy walks layer l from ep, always stepping to a closer neighbour, until
// none is closer. Used on the upper layers, where only direction matters.
func (h *HNSW) greedy(q Vector, ep int32, l int) int32 {
	best, bestD := ep, h.dist(q, h.nodes[ep].vec)
	for changed := true; changed; {
		changed = false
		for _, nb := range h.nodes[best].links[l] {
			if d := h.dist(q, h.nodes[nb].vec); d < bestD {
				best, bestD, changed = nb, d, true
			}
		}
	}
	return best
}

type candidate struct {
	node int32
	dist float32
}

// searchLayer is the core search (Algorithm 2 in the paper). It keeps two
// heaps: candidates still to explore (nearest first) and the best ef found
// so far (farthest on top, so it can be dropped when something closer turns
// up). It stops when the nearest unexplored candidate is farther than the
// worst of the best ef: nothing left can improve the result. Returns up to
// ef results, nearest first.
func (h *HNSW) searchLayer(q Vector, eps []candidate, ef, l int) []candidate {
	seen := h.visited.Get().(*visitedSet)
	defer h.visited.Put(seen)
	seen.reset(len(h.nodes))

	toVisit := &minCands{}
	best := &maxCands{}
	for _, e := range eps {
		seen.mark(e.node)
		heap.Push(toVisit, e)
		heap.Push(best, e)
		if best.Len() > ef {
			heap.Pop(best)
		}
	}
	for toVisit.Len() > 0 {
		c := heap.Pop(toVisit).(candidate)
		if c.dist > (*best)[0].dist && best.Len() >= ef {
			break
		}
		for _, nb := range h.nodes[c.node].links[l] {
			if seen.has(nb) {
				continue
			}
			seen.mark(nb)
			d := h.dist(q, h.nodes[nb].vec)
			if best.Len() < ef || d < (*best)[0].dist {
				heap.Push(toVisit, candidate{nb, d})
				heap.Push(best, candidate{nb, d})
				if best.Len() > ef {
					heap.Pop(best)
				}
			}
		}
	}
	out := make([]candidate, best.Len())
	for i := len(out) - 1; i >= 0; i-- {
		out[i] = heap.Pop(best).(candidate)
	}
	return out
}

// Search returns the k nearest live vectors, nearest first.
func (h *HNSW) Search(q Vector, k int) []Hit {
	if h.entry < 0 || len(h.ids) == 0 {
		return nil
	}
	qv := h.prepare(q)
	ep := h.entry
	for l := h.top; l > 0; l-- {
		ep = h.greedy(qv, ep, l)
	}
	// Ask for extra results when there are deleted nodes, since they are
	// filtered out afterwards.
	ef := max(h.efSrch, k+min(h.deleted, k))
	found := h.searchLayer(qv, []candidate{{ep, h.dist(qv, h.nodes[ep].vec)}}, ef, 0)
	hits := make([]Hit, 0, k)
	for _, c := range found {
		if n := h.nodes[c.node]; !n.deleted {
			hits = append(hits, Hit{ID: n.id, Distance: c.dist})
			if len(hits) == k {
				break
			}
		}
	}
	return hits
}

// Remove marks a vector as deleted. Removing a node from the graph for real
// would break the paths that run through it, so it stays as a stepping
// stone and is just left out of results (a "tombstone", as most HNSW
// libraries do). When tombstones outnumber live vectors, the graph is
// rebuilt without them.
func (h *HNSW) Remove(id string) {
	idx, ok := h.ids[id]
	if !ok {
		return
	}
	h.nodes[idx].deleted = true
	delete(h.ids, id)
	h.deleted++
	if h.deleted > 1000 && h.deleted > len(h.ids) {
		h.rebuild()
	}
}

func (h *HNSW) rebuild() {
	live := make([]*hnode, 0, len(h.ids))
	for _, n := range h.nodes {
		if !n.deleted {
			live = append(live, n)
		}
	}
	h.nodes, h.ids, h.entry, h.top, h.deleted = nil, map[string]int32{}, -1, 0, 0
	for _, n := range live {
		h.Add(n.id, n.vec) // already normalised; normalising again changes nothing
	}
}

// visitedSet remembers which nodes a search has seen. Instead of clearing
// the whole array for every search, each search gets a new stamp number and
// a node counts as seen only if it carries the current stamp.
type visitedSet struct {
	stamp []uint32
	cur   uint32
}

func (v *visitedSet) reset(n int) {
	if len(v.stamp) < n {
		v.stamp = make([]uint32, n+n/2)
		v.cur = 0
	}
	v.cur++
	if v.cur == 0 { // wrapped around after 4 billion searches
		clear(v.stamp)
		v.cur = 1
	}
}
func (v *visitedSet) mark(i int32)     { v.stamp[i] = v.cur }
func (v *visitedSet) has(i int32) bool { return v.stamp[i] == v.cur }

type minCands []candidate

func (c minCands) Len() int           { return len(c) }
func (c minCands) Less(i, j int) bool { return c[i].dist < c[j].dist }
func (c minCands) Swap(i, j int)      { c[i], c[j] = c[j], c[i] }
func (c *minCands) Push(x any)        { *c = append(*c, x.(candidate)) }
func (c *minCands) Pop() any {
	old := *c
	x := old[len(old)-1]
	*c = old[:len(old)-1]
	return x
}

type maxCands []candidate

func (c maxCands) Len() int           { return len(c) }
func (c maxCands) Less(i, j int) bool { return c[i].dist > c[j].dist }
func (c maxCands) Swap(i, j int)      { c[i], c[j] = c[j], c[i] }
func (c *maxCands) Push(x any)        { *c = append(*c, x.(candidate)) }
func (c *maxCands) Pop() any {
	old := *c
	x := old[len(old)-1]
	*c = old[:len(old)-1]
	return x
}
