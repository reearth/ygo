package crdt

import (
	"errors"
	"maps"
	"slices"
	"sort"
)

// errWholeOrNothingContent refuses a remote move or subdocument on a document
// with whole-or-nothing applies, since a move re-points items the document
// already holds and a subdocument is a document of its own, which the record
// of what an apply changed does not follow; so such a document never holds a
// pending move.
var errWholeOrNothingContent = errors.New("a document with whole-or-nothing applies takes no remote move or subdocument")

// applyJournal is what a remote apply on a document with whole-or-nothing
// applies keeps so that it can leave the document as it found it.
type applyJournal struct {
	start applyPoint
	// drain is the point the running drain of the parked queue began at, and
	// parkedAs each parked item the drain changed in place, as it found it.
	drain    *applyPoint
	parkedAs map[*Item]Item
	// named lists the root types the apply named, in order.
	named []string
	// missingCopied is whether the parked queue's missing map is the apply's
	// own copy, which parking may write into.
	missingCopied bool
	// slotsCopied is whether start.pendingItems is the apply's own copy of the
	// queue's slots, and queuedAs is each parked item a park's merge cut or
	// grew, as the apply found it (keepQueueFor).
	slotsCopied bool
	queuedAs    map[*Item]Item
}

// applyPoint is a point of an apply that a rollback can return the document to.
// The parked queue, its slots, its missing map and the pending deletes are kept
// by reference: the apply writes into none of them without copying it first.
// The queue's counts and the pending deletes' counts, which the apply changes
// in place, are kept as values.
type applyPoint struct {
	sv       StateVector
	deleted  DeleteSet
	changed  map[*abstractType]map[string]struct{}
	newItems int
	named    int
	// mapPrev and formats are each map key's value and each type's formatting
	// flags as they were at the point, recorded before their first change.
	mapPrev map[mapSlot]*Item
	formats map[*abstractType]formatFlags

	pending      *pendingUpdate
	pendingItems []*Item
	missing      StateVector
	queue        queueCounts
	pendingDs    parkedDeletesAt
	// parkedLeft is how many structs the park record listed as taken out of
	// the queue by a drain (transactionParked.left).
	parkedLeft int
}

// queueCounts are the fields of the parked queue that a park or an append
// changes in place: its emptied slots, its ready latch and its tail record.
type queueCounts struct {
	dead      int
	ready     bool
	tail      uint64
	tailValid bool
}

// parkedDeletesAt is the store's pending deletes as a point found them: the
// set, kept by reference, the count of its ranges, and a copy of the clients
// given a struct since the last retry, which an append writes in place.
type parkedDeletesAt struct {
	ds     DeleteSet
	ranges int
	moved  map[ClientID]struct{}
}

func parkedDeletesOf(s *StructStore) parkedDeletesAt {
	p := parkedDeletesAt{ds: s.pendingDs, ranges: s.pendingDsRanges}
	if len(s.pendingDsMoved) > 0 {
		p.moved = maps.Clone(s.pendingDsMoved)
	}
	return p
}

// restore puts the store's pending deletes back as p found them.
func (p parkedDeletesAt) restore(s *StructStore) {
	s.pendingDs, s.pendingDsRanges, s.pendingDsMoved = p.ds, p.ranges, p.moved
}

// mapSlot names one key of one map-like type.
type mapSlot struct {
	parent *abstractType
	key    string
}

// formatFlags are the fields of a type that integrating or deleting one of its
// format markers changes.
type formatFlags struct {
	has, clean bool
	gen        uint64
}

// beginRemoteApply starts txn's journal when its document has whole-or-nothing
// applies; sv is the store's state vector as the apply begins.
func beginRemoteApply(txn *Transaction, sv StateVector) {
	if !txn.doc.wholeOrNothing {
		return
	}
	j := &applyJournal{start: pointNow(txn, sv)}
	txn.doc.applying = j
}

// endRemoteApply ends txn's journal, and rolls the apply back when it failed.
func endRemoteApply(txn *Transaction, failed bool) {
	j := txn.doc.applying
	if j == nil {
		return
	}
	txn.doc.applying = nil
	if failed {
		j.rollBack(txn)
	}
}

// pointNow is txn's apply as it stands, with sv the store's state vector.
func pointNow(txn *Transaction, sv StateVector) applyPoint {
	s := txn.doc.store
	p := applyPoint{sv: sv, newItems: len(txn.newItems), pendingDs: parkedDeletesOf(s)}
	if j := txn.doc.applying; j != nil {
		p.named = len(j.named)
	}
	if len(txn.deleteSet.clients) > 0 {
		p.deleted = copyDeleteSet(txn.deleteSet)
	}
	if len(txn.changed) > 0 {
		p.changed = make(map[*abstractType]map[string]struct{}, len(txn.changed))
		for t, keys := range txn.changed {
			p.changed[t] = maps.Clone(keys)
		}
	}
	if q := s.pending; q != nil {
		p.pending, p.pendingItems, p.missing = q, q.items, q.missing
		p.queue = queueCounts{dead: q.dead, ready: q.ready, tail: q.tail, tailValid: q.tailValid}
	}
	if s.parked != nil {
		p.parkedLeft = len(s.parked.left)
	}
	return p
}

// parkedMissing is the parked queue's missing map, for a write that parks an
// item: under a journal, which keeps the map the apply found, the first write
// goes to a copy of it.
func parkedMissing(txn *Transaction) StateVector {
	p := txn.doc.store.pending
	if j := txn.doc.applying; j != nil && p == j.start.pending && !j.missingCopied {
		p.missing = maps.Clone(p.missing)
		j.missingCopied = true
	}
	return p.missing
}

// parkDeletes merges ds, delete ranges whose targets the store does not hold
// yet, into the pending deletes, under a journal into a set of its own
// (ownPendingDs).
func parkDeletes(txn *Transaction, ds DeleteSet) {
	ownPendingDs(txn, ds.clients)
	txn.doc.store.parkDeletes(ds)
}

// ownPendingDs gives the store's pending deletes a map of their own, and
// copies of the ranges of the clients in names, before a write into them under
// a journal, which keeps the set it found by reference: a merge (parkDeletes)
// appends to a client's ranges and sorts them in place, and a retry takes a
// client's ranges out of the map (StructStore.takeReadyDeletes).
func ownPendingDs(txn *Transaction, names map[ClientID][]DeleteRange) {
	if txn.doc.applying == nil {
		return
	}
	s := txn.doc.store
	kept := s.pendingDs
	s.pendingDs = DeleteSet{clients: maps.Clone(kept.clients)}
	if s.pendingDs.clients == nil {
		s.pendingDs.clients = make(map[ClientID][]DeleteRange)
	}
	if kept.order != nil {
		s.pendingDs.order = append([]ClientID{}, kept.order...)
	}
	for c := range names {
		if ranges, ok := kept.clients[c]; ok {
			s.pendingDs.clients[c] = append([]DeleteRange(nil), ranges...)
		}
	}
}

// keepQueueFor records, before a park merges arrivals into the queue the apply
// found (pendingUpdate.apply), what the merge changes in place: the queue's
// slots, which it empties and compacts, so the journal takes its own copy of
// them once, and each parked item it cuts or grows, as the apply found it.
func (j *applyJournal) keepQueueFor(p *pendingUpdate, pl mergePlan) {
	if p != j.start.pending {
		return
	}
	for i, q := range pl.held {
		k := pl.keepHeld[i]
		if k == (mergeKeep{start: q.ID.Clock, end: q.ID.Clock + q.Content.Len()}) {
			continue
		}
		if !j.slotsCopied {
			j.start.pendingItems, j.slotsCopied = slices.Clone(j.start.pendingItems), true
		}
		if _, kept := j.queuedAs[q.Item]; kept || !k.kept() {
			continue
		}
		if j.queuedAs == nil {
			j.queuedAs = make(map[*Item]Item)
		}
		j.queuedAs[q.Item] = itemAsFound(q.Item)
	}
}

// at is the point a first change to a map key or to formatting flags is
// recorded on: the drain's while it runs, else the apply's.
func (j *applyJournal) at() *applyPoint {
	if j.drain != nil {
		return j.drain
	}
	return &j.start
}

// tookMapKey records key's value in parent before a new item takes it.
func (j *applyJournal) tookMapKey(parent *abstractType, key string) {
	p := j.at()
	slot := mapSlot{parent, key}
	if _, seen := p.mapPrev[slot]; seen {
		return
	}
	if p.mapPrev == nil {
		p.mapPrev = make(map[mapSlot]*Item)
	}
	p.mapPrev[slot] = parent.itemMap[key]
}

// keepFormatting records t's formatting flags before a format marker of t is
// integrated or deleted.
func (j *applyJournal) keepFormatting(t *abstractType) {
	p := j.at()
	if _, seen := p.formats[t]; seen {
		return
	}
	if p.formats == nil {
		p.formats = make(map[*abstractType]formatFlags)
	}
	p.formats[t] = formatFlags{has: t.hasFormatting, clean: t.cleanFormatting, gen: t.fmtGen}
}

// keepParked records item, a parked item the running drain is about to change
// in place, as the drain found it.
func (j *applyJournal) keepParked(item *Item) {
	if j.drain == nil {
		return
	}
	if _, kept := j.parkedAs[item]; kept {
		return
	}
	if j.parkedAs == nil {
		j.parkedAs = make(map[*Item]Item)
	}
	j.parkedAs[item] = itemAsFound(item)
}

// itemAsFound is a copy of item, its content copied, that a rollback gives
// back to item.
func itemAsFound(item *Item) Item {
	as := *item
	// A nested type's content is never split, and a copy of it would be a new type.
	if _, nested := item.Content.(*ContentType); !nested {
		as.Content = item.Content.Copy()
	}
	return as
}

// beginDrain starts the drain's point.
func (j *applyJournal) beginDrain(txn *Transaction) {
	p := pointNow(txn, txn.doc.store.StateVector())
	j.drain = &p
}

// takeBackDrain returns the document to where the drain began, and gives each
// parked item the drain changed back as the drain found it.
func (j *applyJournal) takeBackDrain(txn *Transaction) {
	j.rollBackTo(txn, j.drain)
	for item, as := range j.parkedAs {
		*item = as
	}
	j.endDrain()
}

// endDrain ends the drain's point.
func (j *applyJournal) endDrain() { j.drain, j.parkedAs = nil, nil }

// rollBack leaves txn's document as it was when the apply began, and makes the
// transaction commit as one that changed nothing.
func (j *applyJournal) rollBack(txn *Transaction) {
	j.rollBackTo(txn, &j.start)
	if p := j.start.pending; p != nil {
		for item, as := range j.queuedAs {
			*item = as
		}
		// The apply's parks changed the queue's cover in place; the next park
		// that needs it builds it again from the items kept.
		p.cover = nil
	}
	if s := txn.doc.store; s.parked != nil {
		*s.parked = transactionParked{}
	}
	for target, mover := range txn.movedBefore {
		target.MovedBy = mover
		if target.Parent != nil {
			target.Parent.clearMarkers()
		}
	}
	txn.subdocsAdded, txn.subdocsRemoved, txn.subdocsLoaded = nil, nil, nil
	txn.rearbitrate, txn.movedBefore = nil, nil
	txn.rolledBack = true
}

// rollBackTo returns txn's document to point: what was added, deleted, named
// and parked since it is taken out, and the transaction's record of what
// changed is what it was there.
func (j *applyJournal) rollBackTo(txn *Transaction, point *applyPoint) {
	d := txn.doc
	s := d.store
	touched := make(map[*abstractType]struct{})

	// Every struct added since point: unlinked, and dropped from the store.
	for client, items := range s.clients {
		start := point.sv.Clock(client)
		cut := sort.Search(len(items), func(i int) bool { return items[i].ID.Clock >= start })
		for _, x := range items[cut:] {
			if x.Parent != nil {
				unlinkItem(x, touched)
			}
		}
		clear(items[cut:])
		if cut == 0 {
			delete(s.clients, client)
		} else {
			s.clients[client] = items[:cut]
		}
	}

	for slot, prev := range point.mapPrev {
		if prev == nil {
			delete(slot.parent.itemMap, slot.key)
		} else {
			slot.parent.itemMap[slot.key] = prev
		}
		touched[slot.parent] = struct{}{}
	}
	for t, f := range point.formats {
		t.hasFormatting, t.cleanFormatting, t.fmtGen = f.has, f.clean, f.gen
		t.attrCache = attrsAfterCache{}
	}

	// Items held at point that were deleted since come back.
	for client, ranges := range txn.deleteSet.clients {
		start := point.sv.Clock(client)
		items := s.clients[client]
		for _, r := range ranges {
			end := min(r.Clock+r.Len, start)
			if r.Clock >= end {
				continue
			}
			i := sort.Search(len(items), func(i int) bool {
				return items[i].ID.Clock+items[i].Content.Len() > r.Clock
			})
			for ; i < len(items) && items[i].ID.Clock < end; i++ {
				x := items[i]
				if !x.Deleted || point.deleted.IsDeleted(x.ID) {
					continue
				}
				x.Deleted = false
				// The delete detached the subdocument the item carries.
				if cd, ok := x.Content.(*ContentDoc); ok && cd.Doc != nil {
					cd.Doc.reattachSubdoc(x, d)
				}
				if x.Parent != nil {
					if x.Content.IsCountable() {
						x.Parent.length += nativeContentLen(x.Content)
					}
					touched[x.Parent] = struct{}{}
				}
			}
		}
	}

	for _, name := range j.named[point.named:] {
		delete(d.share, name)
	}
	j.named = j.named[:point.named]

	for parent := range touched {
		parent.clearMarkers()
		parent.invalidateFirstLiveCache()
	}

	if q := point.pending; q != nil {
		// The slots past the length kept are cleared, so the queue holds none of
		// what the apply parked.
		clear(point.pendingItems[len(point.pendingItems):cap(point.pendingItems)])
		q.items, q.missing = point.pendingItems, point.missing
		q.dead, q.ready, q.tail, q.tailValid = point.queue.dead, point.queue.ready, point.queue.tail, point.queue.tailValid
	}
	s.pending = point.pending
	point.pendingDs.restore(s)
	if s.parked != nil {
		clear(s.parked.left[point.parkedLeft:])
		s.parked.left = s.parked.left[:point.parkedLeft]
	}

	merge := txn.mergeStructs[:0]
	for _, half := range txn.mergeStructs {
		if half.ID.Clock < point.sv.Clock(half.ID.Client) {
			merge = append(merge, half)
		}
	}
	clear(txn.mergeStructs[len(merge):])
	txn.mergeStructs = merge
	if point.changed == nil {
		clear(txn.changed)
	} else {
		txn.changed = point.changed
	}
	clear(txn.newItems[point.newItems:])
	txn.newItems = txn.newItems[:point.newItems]
	if point.deleted.clients == nil {
		txn.deleteSet = newOrderedDeleteSet()
	} else {
		txn.deleteSet = point.deleted
	}
}

// unlinkItem takes x out of its parent's list, with what it added to the
// parent's length, and records the parent as touched.
func unlinkItem(x *Item, touched map[*abstractType]struct{}) {
	p := x.Parent
	if x.Left != nil {
		x.Left.Right = x.Right
	} else if p.start == x {
		p.start = x.Right
	}
	if x.Right != nil {
		x.Right.Left = x.Left
	}
	if !x.Deleted && x.Content.IsCountable() {
		p.length -= nativeContentLen(x.Content)
	}
	x.Left, x.Right = nil, nil
	touched[p] = struct{}{}
}

// copyDeleteSet copies ds as it is, ranges and order included.
func copyDeleteSet(ds DeleteSet) DeleteSet {
	out := DeleteSet{clients: make(map[ClientID][]DeleteRange, len(ds.clients))}
	for c, ranges := range ds.clients {
		out.clients[c] = append([]DeleteRange(nil), ranges...)
	}
	if ds.order != nil {
		out.order = append([]ClientID{}, ds.order...)
	}
	return out
}
