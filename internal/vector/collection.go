package vector

import (
	"container/heap"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"sync"

	"github.com/ABDULRAIYAAN/vaultdb/internal/store"
)

// How vectors are stored
//
// Like SQL tables, vector collections are ordinary keys in the store, so
// they are saved to disk, crash-safe and compacted like everything else:
//
//	vec:meta:docs          -> {"dim":768,"metric":"COSINE"}
//	vec:item:docs:note1    -> binary vector + the original text
//
// On top of that, each collection keeps an index in memory for fast search.
// The index is rebuilt from the stored items when VaultDB starts.

const (
	metaPrefix = "vec:meta:"
	itemPrefix = "vec:item:"
)

// Store is the part of the key-value store this package needs.
type Store interface {
	Get(key string) (string, error)
	Set(key, value string) error
	Delete(key string) (bool, error)
	Prefix(prefix string, limit int) ([]store.KV, bool)
}

// Hit is one search result.
type Hit struct {
	ID       string
	Distance float32
	Payload  string
}

// Index finds the nearest vectors. The flat index below checks every
// vector; the HNSW index (next step) jumps through a graph instead.
type Index interface {
	Add(id string, v Vector)
	Remove(id string)
	Search(q Vector, k int) []Hit // nearest first; Payload left empty
	Len() int
}

// Info describes a collection.
type Info struct {
	Name   string
	Dim    int
	Metric Metric
	Count  int
}

type collection struct {
	info     Info
	index    Index
	payloads map[string]string
}

// Manager owns every collection.
type Manager struct {
	st Store

	mu     sync.RWMutex
	colls  map[string]*collection
	loaded bool
}

// NewManager creates a manager on top of st. Collections are loaded from the
// store the first time they are needed.
func NewManager(st Store) *Manager { return &Manager{st: st} }

var validName = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

// MaxDim is the largest vector accepted. Big embedding models use about 3,000.
const MaxDim = 8192

type meta struct {
	Dim    int    `json:"dim"`
	Metric Metric `json:"metric"`
}

// load reads every collection from the store. Caller holds m.mu for writing.
func (m *Manager) load() error {
	if m.loaded {
		return nil
	}
	colls := map[string]*collection{}
	metas, _ := m.st.Prefix(metaPrefix, 0)
	for _, kv := range metas {
		name := strings.TrimPrefix(kv.Key, metaPrefix)
		var md meta
		if err := json.Unmarshal([]byte(kv.Value), &md); err != nil {
			return fmt.Errorf("collection %s: damaged settings: %v", name, err)
		}
		c := newCollection(name, md)
		items, _ := m.st.Prefix(itemPrefix+name+":", 0)
		for _, it := range items {
			v, payload, err := decodeItem(it.Value)
			if err != nil {
				return fmt.Errorf("collection %s: %v", name, err)
			}
			id := strings.TrimPrefix(it.Key, itemPrefix+name+":")
			c.index.Add(id, v)
			c.payloads[id] = payload
		}
		colls[name] = c
	}
	m.colls = colls
	m.loaded = true
	return nil
}

func newCollection(name string, md meta) *collection {
	return &collection{
		info:     Info{Name: name, Dim: md.Dim, Metric: md.Metric},
		index:    NewFlat(md.Metric),
		payloads: map[string]string{},
	}
}

// Reset forgets everything in memory, so the next call reloads from the
// store. Used after the whole store is cleared.
func (m *Manager) Reset() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.colls, m.loaded = nil, false
}

func (m *Manager) get(name string) (*collection, error) {
	c, ok := m.colls[name]
	if !ok {
		return nil, fmt.Errorf("vector collection %s does not exist (create it with VCREATE)", name)
	}
	return c, nil
}

// Create makes a new, empty collection.
func (m *Manager) Create(name string, dim int, metric Metric) error {
	if !validName.MatchString(name) {
		return errors.New("collection names may use letters, digits, _ and -, up to 64 characters")
	}
	if dim < 1 || dim > MaxDim {
		return fmt.Errorf("dimension must be between 1 and %d", MaxDim)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.load(); err != nil {
		return err
	}
	if _, exists := m.colls[name]; exists {
		return fmt.Errorf("vector collection %s already exists", name)
	}
	md := meta{Dim: dim, Metric: metric}
	raw, _ := json.Marshal(md)
	if err := m.st.Set(metaPrefix+name, string(raw)); err != nil {
		return err
	}
	m.colls[name] = newCollection(name, md)
	return nil
}

// Drop deletes a collection and all its vectors. It returns how many were
// deleted.
func (m *Manager) Drop(name string) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.load(); err != nil {
		return 0, err
	}
	c, err := m.get(name)
	if err != nil {
		return 0, err
	}
	n := len(c.payloads)
	// Delete the settings key first: if we crash half way, the leftover items
	// belong to no collection and are ignored, rather than a collection
	// silently missing some of its items.
	if _, err := m.st.Delete(metaPrefix + name); err != nil {
		return 0, err
	}
	delete(m.colls, name)
	for id := range c.payloads {
		if _, err := m.st.Delete(itemPrefix + name + ":" + id); err != nil {
			return 0, err
		}
	}
	return n, nil
}

// Add stores a vector under id, replacing any vector already there. It
// returns true if id is new.
func (m *Manager) Add(coll, id string, v Vector, payload string) (bool, error) {
	if id == "" {
		return false, errors.New("id must not be empty")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.load(); err != nil {
		return false, err
	}
	c, err := m.get(coll)
	if err != nil {
		return false, err
	}
	if len(v) != c.info.Dim {
		return false, fmt.Errorf("collection %s holds %d-dimensional vectors, got %d numbers", coll, c.info.Dim, len(v))
	}
	// Disk first, memory second, as everywhere in VaultDB.
	if err := m.st.Set(itemPrefix+coll+":"+id, encodeItem(v, payload)); err != nil {
		return false, err
	}
	_, existed := c.payloads[id]
	if existed {
		c.index.Remove(id)
	}
	c.index.Add(id, v)
	c.payloads[id] = payload
	return !existed, nil
}

// Get returns the vector and text stored under id.
func (m *Manager) Get(coll, id string) (Vector, string, error) {
	if err := m.ensureLoaded(); err != nil {
		return nil, "", err
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	if _, err := m.get(coll); err != nil {
		return nil, "", err
	}
	raw, err := m.st.Get(itemPrefix + coll + ":" + id)
	if err != nil {
		return nil, "", fmt.Errorf("no vector with id %s in %s", id, coll)
	}
	return decodeItem(raw)
}

// Delete removes id and reports whether it existed.
func (m *Manager) Delete(coll, id string) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.load(); err != nil {
		return false, err
	}
	c, err := m.get(coll)
	if err != nil {
		return false, err
	}
	if _, ok := c.payloads[id]; !ok {
		return false, nil
	}
	if _, err := m.st.Delete(itemPrefix + coll + ":" + id); err != nil {
		return false, err
	}
	c.index.Remove(id)
	delete(c.payloads, id)
	return true, nil
}

// Search returns the k vectors in coll nearest to q, nearest first.
func (m *Manager) Search(coll string, q Vector, k int) ([]Hit, error) {
	if err := m.ensureLoaded(); err != nil {
		return nil, err
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	c, err := m.get(coll)
	if err != nil {
		return nil, err
	}
	if len(q) != c.info.Dim {
		return nil, fmt.Errorf("collection %s holds %d-dimensional vectors, the query has %d numbers", coll, c.info.Dim, len(q))
	}
	if k < 1 {
		return nil, errors.New("k must be at least 1")
	}
	hits := c.index.Search(q, k)
	for i := range hits {
		hits[i].Payload = c.payloads[hits[i].ID]
	}
	return hits, nil
}

// List describes every collection, sorted by name.
func (m *Manager) List() ([]Info, error) {
	if err := m.ensureLoaded(); err != nil {
		return nil, err
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]Info, 0, len(m.colls))
	for _, c := range m.colls {
		info := c.info
		info.Count = c.index.Len()
		out = append(out, info)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func (m *Manager) ensureLoaded() error {
	m.mu.RLock()
	loaded := m.loaded
	m.mu.RUnlock()
	if loaded {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.load()
}

// Flat is the simplest index: it measures the distance to every vector and
// keeps the k smallest. Exact, but slow for big collections: 1 million
// vectors means 1 million distance calculations per search.
type Flat struct {
	metric Metric
	vecs   map[string]Vector
}

// NewFlat creates an empty flat index.
func NewFlat(metric Metric) *Flat { return &Flat{metric: metric, vecs: map[string]Vector{}} }

func (f *Flat) Add(id string, v Vector) { f.vecs[id] = v }
func (f *Flat) Remove(id string)        { delete(f.vecs, id) }
func (f *Flat) Len() int                { return len(f.vecs) }

// Search keeps the best k results in a max-heap: the worst of the current
// best k sits on top, so each new vector is compared with it in one step and
// replaces it if closer. That costs O(n log k) instead of sorting all n.
func (f *Flat) Search(q Vector, k int) []Hit {
	h := &maxHeap{}
	for id, v := range f.vecs {
		d := Distance(f.metric, q, v)
		if h.Len() < k {
			heap.Push(h, Hit{ID: id, Distance: d})
		} else if d < (*h)[0].Distance || (d == (*h)[0].Distance && id < (*h)[0].ID) {
			(*h)[0] = Hit{ID: id, Distance: d}
			heap.Fix(h, 0)
		}
	}
	out := make([]Hit, h.Len())
	for i := len(out) - 1; i >= 0; i-- {
		out[i] = heap.Pop(h).(Hit)
	}
	return out
}

// maxHeap keeps the farthest hit on top. Ties are broken by id so results
// are the same every time.
type maxHeap []Hit

func (h maxHeap) Len() int { return len(h) }
func (h maxHeap) Less(i, j int) bool {
	if h[i].Distance != h[j].Distance {
		return h[i].Distance > h[j].Distance
	}
	return h[i].ID > h[j].ID
}
func (h maxHeap) Swap(i, j int) { h[i], h[j] = h[j], h[i] }
func (h *maxHeap) Push(x any)   { *h = append(*h, x.(Hit)) }
func (h *maxHeap) Pop() any {
	old := *h
	x := old[len(old)-1]
	*h = old[:len(old)-1]
	return x
}
