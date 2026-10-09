package crdt

import "container/heap"

// Each blocked group watches one missing dependency and its own coverage.
// Replacing a head removes both old watches; stale waiters cannot accumulate
// while overlapping groups cover successive structs of a long blocked tail.
type pendingWatch struct {
	clock        uint64
	group, index int
	owner        *pendingWatchHeap
}

type pendingWatchHeap struct {
	client ClientID
	items  []*pendingWatch
}

func (h *pendingWatchHeap) Len() int           { return len(h.items) }
func (h *pendingWatchHeap) Less(i, j int) bool { return h.items[i].clock < h.items[j].clock }
func (h *pendingWatchHeap) Swap(i, j int) {
	h.items[i], h.items[j] = h.items[j], h.items[i]
	h.items[i].index = i
	h.items[j].index = j
}
func (h *pendingWatchHeap) Push(value any) {
	w := value.(*pendingWatch)
	w.owner = h
	w.index = len(h.items)
	h.items = append(h.items, w)
}
func (h *pendingWatchHeap) Pop() any {
	last := len(h.items) - 1
	w := h.items[last]
	h.items[last] = nil
	h.items = h.items[:last]
	w.owner = nil
	return w
}

type pendingGroupWorklist struct {
	groups       []pendingGroup
	waits        []pendingWatch
	byClient     map[ClientID]*pendingWatchHeap
	queue        []int
	front, count int
}

func (w *pendingGroupWorklist) cancel(index int) {
	wait := &w.waits[index]
	if owner := wait.owner; owner != nil {
		heap.Remove(owner, wait.index)
		if owner.Len() == 0 {
			delete(w.byClient, owner.client)
		}
	}
}

func (w *pendingGroupWorklist) enqueue(group int) {
	w.cancel(2 * group)
	w.cancel(2*group + 1)
	g := &w.groups[group]
	if g.done || g.queued {
		return
	}
	g.queued = true
	w.queue[(w.front+w.count)%len(w.queue)] = group
	w.count++
}

func (w *pendingGroupWorklist) watch(index int, client ClientID, clock uint64, group int) {
	h := w.byClient[client]
	if h == nil {
		h = &pendingWatchHeap{client: client}
		w.byClient[client] = h
	}
	wait := &w.waits[index]
	wait.clock, wait.group = clock, group
	heap.Push(h, wait)
}

func (w *pendingGroupWorklist) advance(client ClientID, clock uint64) {
	for {
		h := w.byClient[client]
		if h == nil || h.Len() == 0 || h.items[0].clock > clock {
			return
		}
		w.enqueue(h.items[0].group)
	}
}

func resolvePendingGroups(groups []pendingGroup, known StateVector) error {
	w := pendingGroupWorklist{groups: groups, waits: make([]pendingWatch, 2*len(groups)), byClient: make(map[ClientID]*pendingWatchHeap, len(groups)), queue: make([]int, len(groups))}
	for i := range groups {
		w.enqueue(i)
	}
	for w.count > 0 {
		index := w.queue[w.front]
		w.front = (w.front + 1) % len(w.queue)
		w.count--
		g := &groups[index]
		g.queued = false
		for !g.done {
			if g.end > known.Clock(g.client) {
				if client, clock, missing := g.missing(known); missing {
					w.watch(2*index, client, clock, index)
					w.watch(2*index+1, g.client, g.end, index)
					break
				}
				known[g.client] = g.end
				w.advance(g.client, g.end)
			}
			if err := g.next(); err != nil {
				return err
			}
		}
	}
	return nil
}
