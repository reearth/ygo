package crdt

import (
	"fmt"
	"maps"
	"sort"
)

// A document with a scan bound (WithMaxIntegrateScan, SetMaxIntegrateScan)
// applies a remote update whole or not at all. Integration is not a pure
// function of the update: each item is placed by a scan of the list as the
// items before it in the same update left it, so whether an update's scans
// pass the bound is known only by integrating it. An update refused partway -
// by the bound, or for any other reason its apply returns an error - is
// therefore integrated as far as it got and then taken out again
// (applyJournal.rollBack) before the transaction commits, so the document is
// left as it was and the transaction fires no observer (Transaction.rolledBack):
// nothing the update carried is persisted, relayed or seen by anyone.
//
// The pending queue is not the update's. Items an earlier update parked behind
// a gap in its clocks integrate in the apply of the update that fills the gap
// (drainPending), and charged to that update, an earlier sender could make
// every later update that fills its gap be refused. So the drain is a phase of
// its own: it has an allowance of its own (the scan count starts again), and a
// drain that fails is taken back to where it began (applyPoint) and the parked
// queue dropped, while the update that filled the gap stands.
//
// What a rollback has to undo is what integrating and deleting change:
//   - every struct added since the point it rolls back to, which is every
//     struct at or past the point's state vector, unlinked from its parent's
//     list, with the parent's length, and dropped from the store;
//   - the previous value of each map key a new item took (applyPoint.mapPrev);
//   - every item deleted since the point, which the transaction's delete set
//     lists beyond the point's copy of it;
//   - root types named since the point;
//   - the transaction's record of what changed;
//   - for the whole apply, the pending queue, pending deletes and pending moves
//     as they stood, and which move holds each target (movedBefore). The
//     parked items, the clocks the queue waits on (its missing map) and the
//     pending deletes are kept by reference, not copied, so an apply costs
//     nothing in proportion to what earlier updates parked. The apply only
//     appends to the parked items' slice, past the length the journal keeps,
//     or replaces it with a slice of its own (drainPending); it writes the
//     missing map only to park an item (resolveWithinUpdatePending), and its
//     first such write copies the map (parkedMissing); and it replaces the
//     pending deletes rather than writing into them, but for the merge of a
//     delete range it parks (parkDeletes), which writes into copies of the
//     clients it names. The parked items themselves are not touched before
//     the drain, the apply's last step, and a drain that fails drops the
//     queue rather than restoring it, so their fields need no record.
//
// Splits of items the document already held stay: a split leaves the same
// content, and the transaction's commit merges each split back
// (tryMergeWithLefts) once the items between the halves are gone. A remote
// update that would integrate a subdocument or a move is refused before
// anything is integrated, since registering either reaches outside the
// document's lists (refuseOnBoundedDoc).

// applyJournal is what one remote apply on a document with a scan bound
// records so it can leave the document as it found it.
type applyJournal struct {
	start        applyPoint
	pending      *pendingUpdate
	pendingItems []*Item
	missing      StateVector
	// missingCopied is whether pending's missing map is the apply's own copy
	// (parkedMissing).
	missingCopied bool
	pendingDs     DeleteSet
	pendingMoves  map[ClientID][]*Item
	// drain is the point the drain began at, once it has; at is the point a
	// map key's previous value is recorded on.
	drain *applyPoint
	at    *applyPoint
	// integrating is the item integrate has linked into its parent's list and
	// not yet appended to the store, for a failure between the two.
	integrating *Item
}

// applyPoint is a point of an apply a rollback can return the document to.
type applyPoint struct {
	sv       StateVector
	deleted  DeleteSet
	changed  map[*abstractType]map[string]struct{}
	newItems int
	roots    map[string]struct{}
	mapPrev  map[mapSlot]*Item
}

// mapSlot names one key of one map-like type.
type mapSlot struct {
	parent *abstractType
	key    string
}

// pointNow is txn's apply as it stands, with the store's state vector sv.
func pointNow(txn *Transaction, sv StateVector) applyPoint {
	point := applyPoint{
		sv:       sv,
		deleted:  copyDeleteSet(txn.deleteSet),
		changed:  make(map[*abstractType]map[string]struct{}, len(txn.changed)),
		newItems: len(txn.newItems),
		roots:    make(map[string]struct{}, len(txn.doc.share)),
	}
	for t, keys := range txn.changed {
		point.changed[t] = maps.Clone(keys)
	}
	for name := range txn.doc.share {
		point.roots[name] = struct{}{}
	}
	return point
}

// beginRemoteApply starts txn's journal when its document has a scan bound;
// before is the store's state vector as the apply begins. The parked items,
// their missing map and the pending deletes are kept as they are, by reference
// (see the top of this file).
func beginRemoteApply(txn *Transaction, before StateVector) {
	d := txn.doc
	if d.integrateScanLimit() == 0 {
		return
	}
	s := d.store
	j := &applyJournal{start: pointNow(txn, before)}
	j.at = &j.start
	if s.pending != nil {
		j.pending = s.pending
		j.pendingItems = s.pending.items
		j.missing = s.pending.missing
	}
	j.pendingDs = s.pendingDs
	if s.pendingMoves != nil {
		j.pendingMoves = make(map[ClientID][]*Item, len(s.pendingMoves))
		for c, moves := range s.pendingMoves {
			j.pendingMoves[c] = append([]*Item(nil), moves...)
		}
	}
	txn.journal = j
}

// parkedMissing is the pending queue's missing map, for a write that parks an
// item. On a document with a scan bound the journal shares the map the apply
// found, so the apply's first write goes to a copy of it.
func parkedMissing(txn *Transaction) StateVector {
	p := txn.doc.store.pending
	if j := txn.journal; j != nil && p == j.pending && !j.missingCopied {
		p.missing = maps.Clone(p.missing)
		j.missingCopied = true
	}
	return p.missing
}

// parkDeletes merges ds, delete ranges whose targets the store does not hold
// yet, into the store's pending deletes. The merge appends to each client's
// ranges and sorts them in place, so on a document with a scan bound, whose
// journal shares the pending deletes as the apply found them, it writes into a
// set whose map is a copy and whose ranges are copies for the clients ds names.
func parkDeletes(txn *Transaction, ds DeleteSet) {
	s := txn.doc.store
	if txn.journal != nil {
		shared := s.pendingDs
		s.pendingDs = DeleteSet{clients: maps.Clone(shared.clients)}
		if s.pendingDs.clients == nil {
			s.pendingDs.clients = make(map[ClientID][]DeleteRange)
		}
		if shared.order != nil {
			s.pendingDs.order = append([]ClientID{}, shared.order...)
		}
		for c := range ds.clients {
			if ranges, ok := shared.clients[c]; ok {
				s.pendingDs.clients[c] = append([]DeleteRange(nil), ranges...)
			}
		}
	}
	s.pendingDs.Merge(ds)
}

// endRemoteApply ends txn's journal, rolling the whole apply back when it
// failed.
func endRemoteApply(txn *Transaction, failed bool) {
	j := txn.journal
	txn.journal = nil
	if j != nil && failed {
		j.rollBack(txn)
	}
}

// drainParked runs drain, the apply's drain of the pending queue and pending
// deletes. On a document with a scan bound it is a phase of its own, with an
// allowance of its own; one that fails is taken back to where it began and the
// parked queue is dropped, and the apply goes on as if nothing had been parked.
func drainParked(txn *Transaction, drain func()) {
	j := txn.journal
	if j == nil {
		drain()
		return
	}
	s := txn.doc.store
	point := pointNow(txn, s.StateVector())
	// The drain replaces the pending deletes rather than writing into them, so
	// they are kept as they are, by reference.
	pendingDs := s.pendingDs
	j.drain, j.at = &point, &point
	txn.scanned = 0
	defer func() {
		r := recover()
		j.at = &j.start
		if r == nil {
			return
		}
		j.rollBackTo(txn, &point)
		s.pending = nil
		s.pendingDs = pendingDs
	}()
	drain()
}

// tookMapKey records that a new item is about to replace the value of key in
// parent, the first time since the current point that key changes.
func (j *applyJournal) tookMapKey(parent *abstractType, key string) {
	slot := mapSlot{parent, key}
	if j.at.mapPrev == nil {
		j.at.mapPrev = make(map[mapSlot]*Item)
	}
	if _, seen := j.at.mapPrev[slot]; !seen {
		j.at.mapPrev[slot] = parent.itemMap[key]
	}
}

// rollBack leaves txn's document as it was when the journal began, and makes
// the transaction commit as one that changed nothing. It runs under the
// document lock, before the transaction commits.
func (j *applyJournal) rollBack(txn *Transaction) {
	if j.drain != nil {
		// The drain's own map keys go back first, to what they were when the
		// drain began, which is what the start's record then takes back further.
		j.restoreMapKeys(j.drain, map[*abstractType]struct{}{})
		j.drain.mapPrev = nil
	}
	j.rollBackTo(txn, &j.start)

	s := txn.doc.store
	for target, mover := range txn.movedBefore {
		target.MovedBy = mover
		if target.Parent != nil {
			target.Parent.clearMarkers()
		}
	}
	if j.pending != nil {
		// What the apply appended past the length kept is dropped, and its
		// slots cleared so the backing array holds none of the refused items.
		clear(j.pendingItems[len(j.pendingItems):cap(j.pendingItems)])
		j.pending.items = j.pendingItems
		j.pending.missing = j.missing
	}
	s.pending = j.pending
	s.pendingDs = j.pendingDs
	s.pendingMoves = j.pendingMoves

	txn.subdocsAdded, txn.subdocsRemoved, txn.subdocsLoaded = nil, nil, nil
	txn.rearbitrate, txn.movedBefore = nil, nil
	txn.rolledBack = true
}

// rollBackTo returns txn's document to point: what was added, deleted and
// named since it is taken out, and the transaction's record of what changed is
// what it was there.
func (j *applyJournal) rollBackTo(txn *Transaction, point *applyPoint) {
	d := txn.doc
	s := d.store
	touched := make(map[*abstractType]struct{})

	// The struct integrate linked and did not get to append.
	if x := j.integrating; x != nil {
		if x.Parent != nil && x.ID.Clock >= point.sv.Clock(x.ID.Client) {
			unlinkItem(x, touched)
		}
		j.integrating = nil
	}

	// Every struct added since point: unlinked, and dropped from the store.
	for client, items := range s.clients {
		start := point.sv.Clock(client)
		cut := sort.Search(len(items), func(i int) bool { return items[i].ID.Clock >= start })
		for _, x := range items[cut:] {
			if x.Parent != nil {
				unlinkItem(x, touched)
			}
			// An item the pending queue gets back is parked again as it was
			// taken: not deleted, its neighbours to be found on integration.
			if x.Parent != nil && x.Deleted && !point.deleted.IsDeleted(x.ID) && txn.deleteSet.IsDeleted(x.ID) {
				x.Deleted = false
			}
		}
		clear(items[cut:])
		if cut == 0 {
			delete(s.clients, client)
		} else {
			s.clients[client] = items[:cut]
		}
	}

	j.restoreMapKeys(point, touched)

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
				it := items[i]
				return it.ID.Clock+uint64(it.Content.Len()) > r.Clock
			})
			for ; i < len(items) && items[i].ID.Clock < end; i++ {
				x := items[i]
				if !x.Deleted || point.deleted.IsDeleted(x.ID) {
					continue
				}
				x.Deleted = false
				if x.Parent != nil {
					if x.Content.IsCountable() {
						x.Parent.length += x.Content.Len()
					}
					touched[x.Parent] = struct{}{}
				}
			}
		}
	}

	// Root types named since point.
	for name := range d.share {
		if _, held := point.roots[name]; !held {
			delete(d.share, name)
		}
	}

	for parent := range touched {
		parent.clearMarkers()
		parent.invalidateFirstLiveCache()
	}

	merge := txn.mergeStructs[:0]
	for _, half := range txn.mergeStructs {
		if half.ID.Clock < point.sv.Clock(half.ID.Client) {
			merge = append(merge, half)
		}
	}
	txn.mergeStructs = merge
	txn.changed = point.changed
	clear(txn.newItems[point.newItems:])
	txn.newItems = txn.newItems[:point.newItems]
	txn.deleteSet = point.deleted
}

// restoreMapKeys gives each map key a new item took since point its value
// there back.
func (j *applyJournal) restoreMapKeys(point *applyPoint, touched map[*abstractType]struct{}) {
	for slot, prev := range point.mapPrev {
		if prev == nil {
			delete(slot.parent.itemMap, slot.key)
		} else {
			slot.parent.itemMap[slot.key] = prev
		}
		touched[slot.parent] = struct{}{}
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
		p.length -= x.Content.Len()
	}
	x.Left, x.Right = nil, nil
	touched[p] = struct{}{}
}

// copyDeleteSet copies ds as it is, ranges and order included (cloneDeleteSet
// also sorts and compacts it).
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

// errUnboundableContent refuses, on a document with a scan bound, a remote
// update carrying a subdocument or a move, whose integration reaches outside
// the document's lists where a rollback cannot follow (applyJournal).
var errUnboundableContent = fmt.Errorf("%w: a document with a scan bound takes no subdocument or move from a remote update", ErrInvalidUpdate)

// refuseOnBoundedDoc is errUnboundableContent for content a remote update
// decoded into doc carries, when doc has a scan bound; nil otherwise.
func refuseOnBoundedDoc(doc *Doc, content Content) error {
	if doc.integrateScanLimit() == 0 {
		return nil
	}
	switch content.(type) {
	case *ContentDoc, *ContentMove:
		return errUnboundableContent
	}
	return nil
}
