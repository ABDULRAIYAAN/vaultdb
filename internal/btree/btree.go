// Package btree is an in-memory B-tree: a sorted map from string keys to
// string values.
//
// Why a B-tree? A Go map finds a key fast, but its keys come out in random
// order. Questions like "every key from A to M" or "every key starting with
// user:" would mean checking every key. A B-tree keeps keys sorted, so it can
// jump straight to the first match and walk forward. Postgres, MySQL and
// SQLite all use B-trees for their indexes.
//
// Shape of the tree:
//
//	          [ g | p ]                  <- root
//	         /    |    \
//	[a | c | e] [h | k] [q | t | x]      <- leaves
//
// Every node holds a sorted list of items. A node with n items has n+1
// children: everything in children[i] sorts between items[i-1] and items[i].
// All leaves are at the same depth, so every lookup takes the same number of
// steps: about log(n) nodes.
//
// The "degree" t sets the node size. Every node except the root holds
// between t-1 and 2t-1 items. When an insert would overfill a node, the node
// splits in two and pushes its middle item up. When a delete would leave a
// node too empty, it borrows an item from a sibling or merges with one. That
// is what keeps the tree balanced without ever needing a full rebuild.
package btree

import "sort"

// DefaultDegree gives nodes of 31 to 63 items: wide and shallow. A million
// keys fit in a tree only four levels deep.
const DefaultDegree = 32

type item struct {
	key, value string
}

type node struct {
	items    []item
	children []*node // empty for a leaf
}

func (n *node) leaf() bool { return len(n.children) == 0 }

// find returns the index of the first item whose key is >= key, and whether
// that item's key is exactly key. Binary search: O(log items).
func (n *node) find(key string) (int, bool) {
	i := sort.Search(len(n.items), func(i int) bool { return n.items[i].key >= key })
	return i, i < len(n.items) && n.items[i].key == key
}

// BTree is a sorted map. It is not safe for concurrent use; the store's lock
// protects it.
type BTree struct {
	root   *node
	degree int
	length int
}

// New creates an empty tree. A degree below 2 is raised to 2.
func New(degree int) *BTree {
	if degree < 2 {
		degree = 2
	}
	return &BTree{degree: degree}
}

func (t *BTree) maxItems() int { return 2*t.degree - 1 }
func (t *BTree) minItems() int { return t.degree - 1 }

// Len returns the number of keys.
func (t *BTree) Len() int { return t.length }

// Clear removes every key.
func (t *BTree) Clear() {
	t.root = nil
	t.length = 0
}

// Get returns the value for key.
func (t *BTree) Get(key string) (string, bool) {
	n := t.root
	for n != nil {
		i, found := n.find(key)
		if found {
			return n.items[i].value, true
		}
		if n.leaf() {
			return "", false
		}
		n = n.children[i]
	}
	return "", false
}

// Set inserts key or replaces its value. It returns true if the key is new.
func (t *BTree) Set(key, value string) bool {
	if t.root == nil {
		t.root = &node{items: []item{{key, value}}}
		t.length = 1
		return true
	}
	// A full root is split before descending. This is the only way the tree
	// grows taller, and it grows at the top, so all leaves stay level.
	if len(t.root.items) == t.maxItems() {
		old := t.root
		t.root = &node{children: []*node{old}}
		t.splitChild(t.root, 0)
	}
	added := t.insertNonFull(t.root, key, value)
	if added {
		t.length++
	}
	return added
}

// insertNonFull walks down from n, splitting any full child before stepping
// into it. That way, when the key reaches a leaf, the leaf has room.
func (t *BTree) insertNonFull(n *node, key, value string) bool {
	for {
		i, found := n.find(key)
		if found {
			n.items[i].value = value
			return false
		}
		if n.leaf() {
			n.items = insertAt(n.items, i, item{key, value})
			return true
		}
		if len(n.children[i].items) == t.maxItems() {
			t.splitChild(n, i)
			// The child's middle item moved up to items[i]; pick a side.
			switch {
			case key == n.items[i].key:
				n.items[i].value = value
				return false
			case key > n.items[i].key:
				i++
			}
		}
		n = n.children[i]
	}
}

// splitChild splits the full child parent.children[i] into two halves and
// moves its middle item up into parent.
//
//	parent:    [ ... ]               parent:   [ ...  m  ... ]
//	              |          =>                    /     \
//	child:  [a b c m x y z]               [a b c]   [x y z]
func (t *BTree) splitChild(parent *node, i int) {
	child := parent.children[i]
	mid := t.degree - 1
	middle := child.items[mid]

	right := &node{items: append([]item(nil), child.items[mid+1:]...)}
	if !child.leaf() {
		right.children = append([]*node(nil), child.children[mid+1:]...)
		clear(child.children[mid+1:]) // drop references so they can be freed
		child.children = child.children[:mid+1]
	}
	clear(child.items[mid:])
	child.items = child.items[:mid]

	parent.items = insertAt(parent.items, i, middle)
	parent.children = insertAt(parent.children, i+1, right)
}

// Delete removes key. It returns true if the key existed.
func (t *BTree) Delete(key string) bool {
	if t.root == nil {
		return false
	}
	removed := t.remove(t.root, key)
	// The tree shrinks at the top: an empty root is replaced by its only child.
	if len(t.root.items) == 0 {
		if t.root.leaf() {
			t.root = nil
		} else {
			t.root = t.root.children[0]
		}
	}
	if removed {
		t.length--
	}
	return removed
}

// remove deletes key from the subtree at n. Before stepping into any child it
// makes sure that child has more than the minimum number of items, so the
// final removal from a leaf can never leave a node too small.
func (t *BTree) remove(n *node, key string) bool {
	for {
		i, found := n.find(key)

		if n.leaf() {
			if !found {
				return false
			}
			n.items = removeAt(n.items, i)
			return true
		}

		if found {
			// The key sits in an internal node, between two children.
			left, right := n.children[i], n.children[i+1]
			switch {
			case len(left.items) > t.minItems():
				// Replace it with the largest key on its left, then delete that.
				pred := lastItem(left)
				n.items[i] = pred
				key, n = pred.key, left
			case len(right.items) > t.minItems():
				// Or with the smallest key on its right.
				succ := firstItem(right)
				n.items[i] = succ
				key, n = succ.key, right
			default:
				// Both sides are minimal: merge them around the key, then
				// delete it from the merged node.
				t.merge(n, i)
				n = left
			}
			continue
		}

		// The key, if present, is somewhere below children[i].
		if len(n.children[i].items) == t.minItems() {
			i = t.grow(n, i)
		}
		n = n.children[i]
	}
}

// grow gives the minimal child n.children[i] an extra item, by borrowing from
// a sibling if one can spare it, or else by merging with a sibling. It returns
// the index of the child that now holds the original child's keys.
func (t *BTree) grow(n *node, i int) int {
	switch {
	case i > 0 && len(n.children[i-1].items) > t.minItems():
		t.borrowFromLeft(n, i)
		return i
	case i < len(n.children)-1 && len(n.children[i+1].items) > t.minItems():
		t.borrowFromRight(n, i)
		return i
	case i < len(n.children)-1:
		t.merge(n, i)
		return i
	default:
		t.merge(n, i-1)
		return i - 1
	}
}

// borrowFromLeft rotates one item right: the parent's separator moves down
// into the child, and the left sibling's last item moves up to replace it.
func (t *BTree) borrowFromLeft(n *node, i int) {
	child, left := n.children[i], n.children[i-1]
	child.items = insertAt(child.items, 0, n.items[i-1])
	n.items[i-1] = left.items[len(left.items)-1]
	left.items = removeAt(left.items, len(left.items)-1)
	if !left.leaf() {
		child.children = insertAt(child.children, 0, left.children[len(left.children)-1])
		left.children = removeAt(left.children, len(left.children)-1)
	}
}

// borrowFromRight is the mirror image of borrowFromLeft.
func (t *BTree) borrowFromRight(n *node, i int) {
	child, right := n.children[i], n.children[i+1]
	child.items = append(child.items, n.items[i])
	n.items[i] = right.items[0]
	right.items = removeAt(right.items, 0)
	if !right.leaf() {
		child.children = append(child.children, right.children[0])
		right.children = removeAt(right.children, 0)
	}
}

// merge joins children[i], the separator items[i], and children[i+1] into
// children[i], and removes the separator and children[i+1] from n.
func (t *BTree) merge(n *node, i int) {
	left, right := n.children[i], n.children[i+1]
	left.items = append(left.items, n.items[i])
	left.items = append(left.items, right.items...)
	left.children = append(left.children, right.children...)
	n.items = removeAt(n.items, i)
	n.children = removeAt(n.children, i+1)
}

func lastItem(n *node) item {
	for !n.leaf() {
		n = n.children[len(n.children)-1]
	}
	return n.items[len(n.items)-1]
}

func firstItem(n *node) item {
	for !n.leaf() {
		n = n.children[0]
	}
	return n.items[0]
}

// Ascend calls fn for every key k with from <= k < to, in sorted order.
// An empty to means "no upper limit". Iteration stops early if fn returns
// false.
func (t *BTree) Ascend(from, to string, fn func(key, value string) bool) {
	if t.root != nil {
		t.root.ascend(from, to, fn)
	}
}

// ascend is an in-order walk that skips everything before from: children
// left of the first matching item only hold smaller keys.
func (n *node) ascend(from, to string, fn func(key, value string) bool) bool {
	i, _ := n.find(from)
	for ; i < len(n.items); i++ {
		if !n.leaf() && !n.children[i].ascend(from, to, fn) {
			return false
		}
		it := n.items[i]
		if to != "" && it.key >= to {
			return false
		}
		if !fn(it.key, it.value) {
			return false
		}
	}
	if !n.leaf() {
		return n.children[len(n.items)].ascend(from, to, fn)
	}
	return true
}

// Height returns the number of levels (0 for an empty tree).
func (t *BTree) Height() int {
	h := 0
	for n := t.root; n != nil; h++ {
		if n.leaf() {
			return h + 1
		}
		n = n.children[0]
	}
	return h
}

func insertAt[T any](s []T, i int, v T) []T {
	s = append(s, v)
	copy(s[i+1:], s[i:])
	s[i] = v
	return s
}

func removeAt[T any](s []T, i int) []T {
	copy(s[i:], s[i+1:])
	var zero T
	s[len(s)-1] = zero
	return s[:len(s)-1]
}
