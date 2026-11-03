package crdt

import (
	"math/rand/v2"
	"slices"
	"sort"
)

// seqFlat is the length up to which a seq stays a slice, where moving the
// elements after an insert costs less than a tree's steps.
const seqFlat = 256

// seq is a sequence that takes an insert or a cut at any position: a slice
// while short, then a treap, so each step costs O(log n), and filling or
// emptying a queue one struct per update stays linear.
type seq[T any] struct {
	flat []T
	tree *treap[T]
}

// seqOf takes vals as a seq.
func seqOf[T any](vals []T) seq[T] {
	if len(vals) <= seqFlat {
		return seq[T]{flat: vals}
	}
	t := &treap[T]{nodes: make([]treapNode[T], 1, len(vals)+1)}
	t.root = t.build(vals)
	return seq[T]{tree: t}
}

func (s seq[T]) len() int {
	if s.tree != nil {
		return s.tree.size(s.tree.root)
	}
	return len(s.flat)
}

func (s seq[T]) at(i int) T {
	if s.tree != nil {
		return s.tree.at(i)
	}
	return s.flat[i]
}

// search is the first position whose element satisfies pred, which is false
// then true along the sequence, or len.
func (s seq[T]) search(pred func(T) bool) int {
	if s.tree != nil {
		return s.tree.search(pred)
	}
	return sort.Search(len(s.flat), func(i int) bool { return pred(s.flat[i]) })
}

// each calls fn on the elements at [from, to) in order.
func (s seq[T]) each(from, to int, fn func(T)) {
	if s.tree != nil {
		s.tree.each(s.tree.root, from, to, fn)
		return
	}
	for _, v := range s.flat[from:to] {
		fn(v)
	}
}

// slice is the elements as a slice, s's own while s is one.
func (s seq[T]) slice() []T {
	if s.tree == nil {
		return s.flat
	}
	out := make([]T, 0, s.len())
	s.each(0, s.len(), func(v T) { out = append(out, v) })
	return out
}

// replace puts vals in place of the elements at [from, to).
func (s *seq[T]) replace(from, to int, vals []T) {
	if s.tree == nil {
		if s.flat = slices.Replace(s.flat, from, to, vals...); len(s.flat) > seqFlat {
			*s = seqOf(s.flat)
		}
		return
	}
	t := s.tree
	a, rest := t.split(t.root, from)
	cut, b := t.split(rest, to-from)
	t.release(cut)
	t.root = t.join(t.join(a, t.build(vals)), b)
}

func (s *seq[T]) insert(i int, v T) { s.replace(i, i, []T{v}) }

// bulk reports whether adding k elements costs less as one pass over the
// sequence than as k steps.
func (s seq[T]) bulk(k int) bool { return s.tree == nil || 16*k >= s.len() }

// mergeIn adds add, sorted by compare as s is, each after the elements equal
// to it.
func (s *seq[T]) mergeIn(add []T, compare func(a, b T) int) {
	switch {
	case len(add) == 0:
	case s.bulk(len(add)):
		*s = seqOf(mergeSorted(s.slice(), add, compare))
	default:
		for _, v := range add {
			s.insert(s.search(func(x T) bool { return compare(x, v) > 0 }), v)
		}
	}
}

// treap holds a long seq in nodes, found by index, nodes[0] standing for the
// empty tree; random priorities keep it shallow whatever order of steps a
// peer chooses.
type treap[T any] struct {
	nodes []treapNode[T]
	root  int32
	free  int32 // released nodes, linked through left
}

type treapNode[T any] struct {
	val         T
	left, right int32
	size        int32
	prio        uint32
}

func (t *treap[T]) size(n int32) int { return int(t.nodes[n].size) }

func (t *treap[T]) fix(n int32) {
	t.nodes[n].size = 1 + t.nodes[t.nodes[n].left].size + t.nodes[t.nodes[n].right].size
}

func (t *treap[T]) node(v T) int32 {
	n := t.free
	if n != 0 {
		t.free = t.nodes[n].left
	} else {
		n = int32(len(t.nodes))
		t.nodes = append(t.nodes, treapNode[T]{})
	}
	t.nodes[n] = treapNode[T]{val: v, size: 1, prio: rand.Uint32()}
	return n
}

// release returns n's nodes to the free list.
func (t *treap[T]) release(n int32) {
	if n == 0 {
		return
	}
	t.release(t.nodes[n].left)
	t.release(t.nodes[n].right)
	t.nodes[n] = treapNode[T]{left: t.free}
	t.free = n
}

// build makes a treap of vals in one pass, keeping its right spine on a stack;
// a node's subtree is whole once it leaves the spine.
func (t *treap[T]) build(vals []T) int32 {
	var stack [64]int32
	spine := stack[:0]
	for _, v := range vals {
		n := t.node(v)
		var last int32
		for len(spine) > 0 && t.nodes[spine[len(spine)-1]].prio < t.nodes[n].prio {
			last = spine[len(spine)-1]
			spine = spine[:len(spine)-1]
			t.fix(last)
		}
		t.nodes[n].left = last
		if len(spine) > 0 {
			t.nodes[spine[len(spine)-1]].right = n
		}
		spine = append(spine, n)
	}
	if len(spine) == 0 {
		return 0
	}
	for i := len(spine) - 1; i >= 0; i-- {
		t.fix(spine[i])
	}
	return spine[0]
}

// split returns the treap of n's first k elements and that of the rest.
func (t *treap[T]) split(n int32, k int) (int32, int32) {
	if n == 0 {
		return 0, 0
	}
	if l := t.size(t.nodes[n].left); k > l {
		a, b := t.split(t.nodes[n].right, k-l-1)
		t.nodes[n].right = a
		t.fix(n)
		return n, b
	}
	a, b := t.split(t.nodes[n].left, k)
	t.nodes[n].left = b
	t.fix(n)
	return a, n
}

// join returns the treap of a's elements then b's.
func (t *treap[T]) join(a, b int32) int32 {
	switch {
	case a == 0:
		return b
	case b == 0:
		return a
	case t.nodes[a].prio > t.nodes[b].prio:
		t.nodes[a].right = t.join(t.nodes[a].right, b)
		t.fix(a)
		return a
	default:
		t.nodes[b].left = t.join(a, t.nodes[b].left)
		t.fix(b)
		return b
	}
}

func (t *treap[T]) at(i int) T {
	n := t.root
	for {
		l := t.size(t.nodes[n].left)
		switch {
		case i < l:
			n = t.nodes[n].left
		case i == l:
			return t.nodes[n].val
		default:
			i -= l + 1
			n = t.nodes[n].right
		}
	}
}

func (t *treap[T]) search(pred func(T) bool) int {
	found, at := t.size(t.root), 0
	for n := t.root; n != 0; {
		l := t.size(t.nodes[n].left)
		if pred(t.nodes[n].val) {
			found = at + l
			n = t.nodes[n].left
		} else {
			at += l + 1
			n = t.nodes[n].right
		}
	}
	return found
}

// each calls fn on the elements of n at [from, to) in order.
func (t *treap[T]) each(n int32, from, to int, fn func(T)) {
	for n != 0 && from < to {
		l := t.size(t.nodes[n].left)
		if to <= l {
			n = t.nodes[n].left
			continue
		}
		if from < l {
			t.each(t.nodes[n].left, from, l, fn)
		}
		if from <= l {
			fn(t.nodes[n].val)
		}
		from, to = max(from-l-1, 0), to-l-1
		n = t.nodes[n].right
	}
}
