package crdt

import (
	"context"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"sync"
	"time"
)

// StackItem represents one reversible unit on the undo or redo stack.
// It captures what was inserted and what was deleted by a set of consecutive
// local transactions so that Undo / Redo can invert those changes.
type StackItem struct {
	// insertions records the clocks each captured transaction inserted, so
	// remote items landing between merged transactions are never undone.
	insertions DeleteSet
	// deletions records items deleted by the captured transaction(s).
	// These are restored (re-inserted) when this item is applied.
	deletions DeleteSet

	// Meta holds arbitrary user data attached to this stack item.
	// Useful for storing cursor positions, selection ranges, etc.
	// around the undo boundary.
	Meta map[string]any
}

// UndoManagerOption configures an UndoManager at creation time.
type UndoManagerOption func(*UndoManager)

// WithCaptureTimeout sets the window within which consecutive local
// transactions are merged into a single undo stack item. The default is
// 500 ms, which matches the Yjs reference implementation.
func WithCaptureTimeout(d time.Duration) UndoManagerOption {
	return func(u *UndoManager) { u.captureTimeout = d }
}

// WithTrackedOrigins restricts the UndoManager to only capture transactions
// whose Origin matches one of the provided values. By default (no option set)
// all local transactions are captured regardless of origin. Applied remote
// updates (txn.Local == false) are never captured, even with a tracked origin.
//
// This is useful for multi-user documents where each user has a distinct
// origin tag and should only be able to undo their own changes.
//
// Matching is Go interface equality (==), so origin values must be
// DISTINGUISHABLE under ==. Values that compare equal are one origin, not
// two — most surprisingly for pointers to zero-size types: Go satisfies
// every zero-size allocation from one address (runtime.zerobase), so
//
//	a := new(struct{})
//	b := new(struct{})
//	// a == b — both may point at runtime.zerobase
//
// two "unique" tokens minted that way alias, and tracking one silently
// captures the other's transactions too. This exact aliasing once disabled
// relay publishing inside provider/websocket for six releases (#203, and
// see relayOriginSentinel's doc in that package). Because the library
// cannot make caller-supplied values distinct after the fact,
// WithTrackedOrigins PANICS when given a pointer to a zero-size type.
//
// Safe token shapes:
//   - a pointer to a NON-zero-size type — e.g. type token struct{ _ byte };
//     &token{} — every allocation is a distinct origin;
//   - distinct named types compared by value — originA{} and originB{}
//     never alias each other (interface equality compares the dynamic type
//     first), though every originA{} is the same origin as every other;
//   - ordinary comparable values (strings, ints) with the usual value
//     semantics: "alice" from anywhere is the origin "alice".
func WithTrackedOrigins(origins ...any) UndoManagerOption {
	for _, o := range origins {
		if t := reflect.TypeOf(o); t != nil && t.Kind() == reflect.Pointer && t.Elem().Size() == 0 {
			panic(fmt.Sprintf(
				"crdt: WithTrackedOrigins: %T is a pointer to a zero-size type and cannot serve as a unique origin token "+
					"(all zero-size allocations may share one address, runtime.zerobase, so two such tokens compare ==); "+
					"use a pointer to a non-zero-size type instead, e.g. type token struct{ _ byte }", o))
		}
	}
	return func(u *UndoManager) {
		u.trackedOrigins = make(map[any]struct{}, len(origins))
		for _, o := range origins {
			u.trackedOrigins[o] = struct{}{}
		}
	}
}

// UndoManager tracks local transactions on one or more shared types and
// provides Undo / Redo operations. Only transactions originating on this
// peer (txn.Local == true) are captured; applied remote updates are ignored
// whatever their origin. (Yjs gates on trackedOrigins alone.)
//
// Undo inverts the most recent captured change: insertions are deleted and
// deletions are restored. Redo re-applies the most recently undone change.
//
// Call Destroy when the UndoManager is no longer needed to stop tracking and
// release the subscription held on the document.
//
// Note: UndoManager cannot restore items whose content has been freed by
// RunGC. If you need full undo history, either disable GC (WithGC(false)) or
// avoid calling RunGC while the UndoManager is active.
type UndoManager struct {
	doc            *Doc
	scope          []*abstractType
	undoStack      []*StackItem
	redoStack      []*StackItem
	mu             sync.Mutex
	unsubscribe    func()
	captureTimeout time.Duration
	lastTxnTime    time.Time
	// captures counts captured transactions, so pop can tell whether an edit
	// committed while its stack item was being applied.
	captures uint64

	// trackedOrigins, when non-nil, limits capture to transactions whose
	// Origin matches one of the keys. When nil, all local transactions are
	// captured (default behaviour).
	trackedOrigins map[any]struct{}

	onStackItemAdded []func(*StackItem, bool)
}

// NewUndoManager creates an UndoManager that tracks the listed shared types.
// scope must not be empty. Multiple types can be tracked simultaneously; any
// local transaction that touches a scope type, or a type nested in one, is
// captured.
func NewUndoManager(doc *Doc, scope []SharedType, opts ...UndoManagerOption) *UndoManager {
	u := &UndoManager{
		doc:            doc,
		captureTimeout: 500 * time.Millisecond,
	}
	for _, t := range scope {
		u.scope = append(u.scope, t.baseType())
	}
	for _, opt := range opts {
		opt(u)
	}

	u.unsubscribe = doc.OnAfterTransaction(func(txn *Transaction) {
		// Skip undo/redo operations to avoid re-capturing our own inversions.
		if txn.Origin == u {
			return
		}
		u.captureTransaction(txn)
	})

	// Suppress transaction-commit auto-GC (#78 H1) while this UndoManager is
	// attached. Otherwise applyStackItem can't restore items the user deletes
	// — their Content would have been replaced with a length-only tombstone.
	doc.mu.Lock()
	doc.undoManagerCount++
	doc.mu.Unlock()

	return u
}

// OnStackItemAdded registers fn to be called whenever a new StackItem is
// pushed onto the undo stack (isRedo=false) or the redo stack (isRedo=true).
// Use this to attach cursor metadata (e.g. selection before/after) to each
// stack item via item.Meta.
func (u *UndoManager) OnStackItemAdded(fn func(*StackItem, bool)) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.onStackItemAdded = append(u.onStackItemAdded, fn)
}

// Destroy stops tracking transactions and releases the document subscription.
// After Destroy, Undo and Redo are no-ops.
//
// Destroy also re-enables transaction-commit auto-GC (#78 H1) when no other
// UndoManager remains attached to the doc.
func (u *UndoManager) Destroy() {
	if u.unsubscribe != nil {
		u.unsubscribe()
		u.unsubscribe = nil
		u.doc.mu.Lock()
		if u.doc.undoManagerCount > 0 {
			u.doc.undoManagerCount--
		}
		u.doc.mu.Unlock()
	}
}

// UndoStackSize returns the number of items currently on the undo stack.
func (u *UndoManager) UndoStackSize() int {
	u.mu.Lock()
	defer u.mu.Unlock()
	return len(u.undoStack)
}

// RedoStackSize returns the number of items currently on the redo stack.
func (u *UndoManager) RedoStackSize() int {
	u.mu.Lock()
	defer u.mu.Unlock()
	return len(u.redoStack)
}

// Undo inverts the most recently captured local change. Stack items that
// no longer change anything (e.g. their insertions were already deleted) are
// discarded and the next one is tried, as in Yjs. Returns true if a change
// was applied; false if the undo stack ran out first.
func (u *UndoManager) Undo() bool {
	return u.pop(false)
}

// Redo re-applies the most recently undone change, skipping no-op stack
// items like Undo. Returns true if a change was applied; false if the redo
// stack ran out first.
func (u *UndoManager) Redo() bool {
	return u.pop(true)
}

// pop applies the top of the undo (or redo) stack, pushing its inverse onto
// the opposite stack, until one item performs a change.
func (u *UndoManager) pop(redo bool) bool {
	for {
		u.mu.Lock()
		stack := &u.undoStack
		if redo {
			stack = &u.redoStack
		}
		if len(*stack) == 0 {
			u.mu.Unlock()
			return false
		}
		item := (*stack)[len(*stack)-1]
		*stack = (*stack)[:len(*stack)-1]
		// Snapshot the deletions under u.mu: capture replaces them (copy-on-write)
		// once the doc unlocks.
		others := make([]DeleteSet, 0, len(u.undoStack)+len(u.redoStack))
		for _, st := range [][]*StackItem{u.undoStack, u.redoStack} {
			for _, s := range st {
				others = append(others, s.deletions)
			}
		}
		if !redo {
			// The next edit must not merge into the next-older item (Yjs stops
			// capturing after an undo, not a redo).
			u.lastTxnTime = time.Time{}
		}
		captures, undoLen := u.captures, len(u.undoStack)
		u.mu.Unlock()

		inverse := u.applyStackItem(item, others)
		if inverse == nil {
			continue
		}
		u.mu.Lock()
		raced := u.captures != captures
		switch {
		case redo && raced:
			// Edits captured during the apply came after it: keep them on top.
			at := min(undoLen, len(u.undoStack))
			u.undoStack = append(u.undoStack[:at], append([]*StackItem{inverse}, u.undoStack[at:]...)...)
		case redo:
			u.undoStack = append(u.undoStack, inverse)
		case raced:
			// An edit captured during the apply invalidated redo.
			u.mu.Unlock()
			return true
		default:
			u.redoStack = append(u.redoStack, inverse)
		}
		u.fireOnStackItemAdded(inverse, !redo)
		u.mu.Unlock()
		return true
	}
}

// UndoContext is the context-aware variant of Undo. If ctx is already
// cancelled, the undo is NOT attempted, false is returned, and ctx.Err()
// is the second return value. Otherwise the undo proceeds and the result
// (whether anything was undone) is returned with nil error.
//
// Like TransactContext, mid-call ctx cancellation is cooperative; this
// only guards the entry point.
func (u *UndoManager) UndoContext(ctx context.Context) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	return u.Undo(), nil
}

// RedoContext is the context-aware variant of Redo. See UndoContext for
// the ctx semantics.
func (u *UndoManager) RedoContext(ctx context.Context) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	return u.Redo(), nil
}

// Clear discards all items from both stacks without applying them.
func (u *UndoManager) Clear() {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.undoStack = u.undoStack[:0]
	u.redoStack = u.redoStack[:0]
}

// StopCapturing prevents the next transaction from being merged with the
// current top of the undo stack, forcing it to become a new stack item.
// Call this to create an explicit undo boundary between two operations that
// would otherwise be grouped by the capture timeout.
func (u *UndoManager) StopCapturing() {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.lastTxnTime = time.Time{}
}

// ── internal ─────────────────────────────────────────────────────────────────

// captureTransaction examines txn and either appends a new StackItem to
// undoStack or merges the transaction into the existing top item.
func (u *UndoManager) captureTransaction(txn *Transaction) {
	if !txn.Local {
		return
	}
	// If tracked origins are configured, only capture transactions whose Origin
	// is in the set. When the set is empty, capture all local transactions.
	if len(u.trackedOrigins) > 0 {
		if _, ok := u.trackedOrigins[txn.Origin]; !ok {
			return
		}
	}
	if !u.txnAffectsScope(txn) {
		return
	}

	item := &StackItem{
		insertions: insertedRanges(txn.beforeState, txn.afterState),
		deletions:  cloneDeleteSet(txn.deleteSet),
	}

	u.mu.Lock()
	defer u.mu.Unlock()

	now := time.Now()
	if len(u.undoStack) > 0 && !u.lastTxnTime.IsZero() && now.Sub(u.lastTxnTime) <= u.captureTimeout {
		// Copy-on-write: a concurrent Undo may be reading a snapshot of
		// top.deletions.
		top := u.undoStack[len(u.undoStack)-1]
		top.insertions.Merge(item.insertions)
		merged := cloneDeleteSet(top.deletions)
		merged.Merge(item.deletions)
		top.deletions = merged
	} else {
		u.undoStack = append(u.undoStack, item)
		u.fireOnStackItemAdded(item, false)
	}
	u.lastTxnTime = now
	u.captures++

	// Any new local edit invalidates the redo stack.
	u.redoStack = u.redoStack[:0]
}

// applyStackItem executes the inverse of item as a new local transaction and
// returns a new StackItem representing what that inversion did (for the
// opposite stack). Returns nil if it changed nothing (e.g. every referenced
// item was already deleted or GC'd). others are the remaining stack items'
// deletions, consulted by the map-key conflict check.
func (u *UndoManager) applyStackItem(item *StackItem, others []DeleteSet) *StackItem {
	var resultItem *StackItem

	u.doc.Transact(func(txn *Transaction) {
		// Step 1: delete items that were inserted by the captured transaction(s).
		// An inserted item whose deletion was later undone lives on as its redone
		// copy, which is what must be deleted (Yjs followRedone).
		var toDelete []*Item
		u.iterateItems(txn, item.insertions, func(it *Item) { toDelete = append(toDelete, it) })
		performed := false
		for _, it := range toDelete {
			it = u.followRedone(txn, it)
			if it != nil && !it.Deleted && u.itemInScope(it) {
				it.delete(txn)
				performed = true
			}
		}

		// Step 2: restore items that were deleted by the captured transaction by
		// RE-INSERTING a copy of their content as new items (redoItem). Flipping
		// Deleted=false in place produced no wire record, so the restoration never
		// propagated and a back-sync from a peer (which still had the tombstone)
		// re-deleted it locally. Re-inserting makes undo a real, convergent insert.
		// Collect targets first, then redo (integrate appends new items to the
		// store, which we must not visit as restore targets). Items inserted in
		// the same stack item were created and deleted within it, so they stay
		// deleted (Yjs popStackItem).
		var toRedo []*Item
		pass := &redoPass{set: make(map[*Item]struct{}), insertions: item.insertions, others: others}
		u.iterateItems(txn, item.deletions, func(it *Item) {
			if !it.Deleted || !u.itemInScope(it) || item.insertions.IsDeleted(it.ID) {
				return
			}
			// Content freed by GC cannot be restored.
			if _, isGC := it.Content.(*ContentDeleted); isGC {
				return
			}
			toRedo = append(toRedo, it)
			pass.set[it] = struct{}{}
		})
		// Yjs restores a run it holds as one struct as one item, which places a
		// concurrent insert differently than per-item copies, so group runs
		// before any redo splices between them. A restored move redoes its
		// target first; that target ends its run.
		merged := make([]bool, len(toRedo))
		for i := 1; i < len(toRedo); i++ {
			merged[i] = yjsMergeable(toRedo[i-1], toRedo[i])
		}
		for i := 0; i < len(toRedo); {
			j := i + 1
			for toRedo[i].redone == nil && j < len(toRedo) && merged[j] && toRedo[j].redone == nil {
				j++
			}
			if u.redoItem(txn, toRedo[i], toRedo[i+1:j], pass) != nil {
				performed = true
			}
			i = j
		}
		if !performed {
			return
		}

		// The item.delete calls above bypass deleteRange's marker shift, so
		// cached positions after a deleted item would overshoot (Yjs
		// popStackItem clears _searchMarker the same way).
		for t := range txn.changed {
			t.clearMarkers()
		}

		// txn.afterState is only set at commit, so read the live store to
		// record what this inversion re-inserted (redoItem).
		resultItem = &StackItem{
			insertions: insertedRanges(txn.beforeState, u.doc.store.StateVector()),
			deletions:  cloneDeleteSet(txn.deleteSet),
		}
	}, u) // origin = u so captureTransaction skips this txn

	return resultItem
}

// redoItem re-inserts a copy of a deleted item's content as a NEW item so that
// undoing the deletion propagates to peers as a real insert (rather than an
// in-place tombstone flip, which never syncs). The new item is positioned via
// the original's neighbours, following redone chains across already-restored
// neighbours; for root and live-nested parents this reduces to the original
// (now-tombstoned) neighbours, which preserves order. A child of a deleted
// nested type is placed into the type's redone copy, redoing the container
// first when it is in redoSet. Mirrors Yjs redoItem.
// rest are items following item that Yjs holds merged with it; their content
// is appended to the copy. Returns the new item, or nil if it cannot be placed.
func (u *UndoManager) redoItem(txn *Transaction, item *Item, rest []*Item, p *redoPass) *Item {
	if item.redone != nil {
		return u.doc.store.getItemCleanStart(txn, *item.redone)
	}
	parent := item.Parent
	if parent == nil {
		return nil
	}
	if pi := parent.item; pi != nil && pi.Deleted {
		if pi.redone == nil {
			if _, ok := p.set[pi]; !ok || u.redoItem(txn, pi, nil, p) == nil {
				return nil
			}
		}
		if pi = u.followRedone(txn, pi); pi == nil {
			return nil
		}
		ct, ok := pi.Content.(*ContentType)
		if !ok || ct.Type == nil {
			return nil
		}
		parent = ct.Type
	}

	content := item.Content.Copy()
	if cm, ok := content.(*ContentMove); ok && cm.Target != nil {
		// A restored move targets its target's restored copy, redoing that first
		// when this undo restores it too.
		if t := u.doc.store.Find(*cm.Target); t != nil {
			if _, ok := p.set[t]; ok && t.redone == nil {
				u.redoItem(txn, t, nil, p)
			}
			if t = u.followRedone(txn, t); t != nil && t.ID != *cm.Target {
				// The copy may hold a merged run; TargetLen trims it.
				content = NewContentMove(&t.ID, cm.TargetLen)
			}
		}
	}

	var left, right *Item
	if item.ParentSub == nil {
		// Sequence element: position between the original left neighbour and the
		// item itself, following redone pointers across neighbours that now belong
		// to a different (re-inserted) parent.
		left = item.Left
		for left != nil {
			lt := left
			for lt != nil && lt.Parent != parent {
				if lt.redone == nil {
					lt = nil
				} else {
					lt = u.doc.store.Find(*lt.redone)
				}
			}
			if lt != nil && lt.Parent == parent {
				left = lt
				break
			}
			left = left.Left
		}
		right = item
		for right != nil {
			rt := right
			for rt != nil && rt.Parent != parent {
				if rt.redone == nil {
					rt = nil
				} else {
					rt = u.doc.store.Find(*rt.redone)
				}
			}
			if rt != nil && rt.Parent == parent {
				right = rt
				break
			}
			right = right.Right
		}
	} else if nextSameKey(item) != nil {
		// Map entry since overwritten: walk past values this undo deletes or
		// that are undo/redo history; any other later value is a remote edit,
		// which undo must not overwrite (Yjs redoItem).
		left = item
		for r := nextSameKey(left); r != nil && (r.redone != nil || p.insertions.IsDeleted(r.ID) || deletedByStacks(p.others, r.ID) || collectedWithParent(r)); r = nextSameKey(left) {
			left = u.followRedone(txn, r)
		}
		if left == nil || nextSameKey(left) != nil {
			return nil
		}
		// The walk ran in the deleted container; chain after the key's entry in
		// its redone copy, since peers derive the parent from the origin.
		if left.Parent != parent {
			left = parent.itemMap[*item.ParentSub]
		}
	} else if existing, ok := parent.itemMap[*item.ParentSub]; ok {
		// Map entry: chain after the key's current entry. right stays nil.
		left = existing
	}

	origin, originRight := neighbourOrigins(left, right)
	content = appendContents(content, rest)
	ni := &Item{
		ID:          ID{Client: txn.doc.clientID, Clock: txn.doc.store.NextClock(txn.doc.clientID)},
		Origin:      origin,
		OriginRight: originRight,
		Left:        left,
		Parent:      parent,
		ParentSub:   item.ParentSub,
		Content:     content,
	}
	nid := ni.ID
	item.redone = &nid
	off := uint64(item.Content.Len())
	for _, r := range rest {
		r.redone = &ID{Client: nid.Client, Clock: nid.Clock + off}
		off += uint64(r.Content.Len())
	}
	ni.integrate(txn, 0)
	if item.MovedBy != nil && item.Parent == parent {
		for _, mv := range p.movesOf(u.doc.store, item) {
			u.redoMove(txn, mv, ni)
		}
	}
	return ni
}

// redoPass is the state one applyStackItem shares across its redoItem calls.
type redoPass struct {
	set        map[*Item]struct{} // items this undo restores
	insertions DeleteSet          // the stack item's insertions
	others     []DeleteSet        // the other stack items' deletions
	moves      map[*abstractType]map[*Item][]*Item
}

// movesOf returns the live moves of target in its parent, lowest priority
// (moveBeats) first. Each parent is indexed once per pass.
func (p *redoPass) movesOf(store *StructStore, target *Item) []*Item {
	parent := target.Parent
	idx, ok := p.moves[parent]
	if !ok {
		idx = make(map[*Item][]*Item)
		for it := parent.start; it != nil; it = it.Right {
			if cm, ok := it.Content.(*ContentMove); ok && !it.Deleted && cm.Target != nil {
				if t := store.Find(*cm.Target); t != nil {
					idx[t] = append(idx[t], it)
				}
			}
		}
		for _, ms := range idx {
			sort.Slice(ms, func(i, j int) bool { return moveBeats(ms[j], ms[i]) })
		}
		if p.moves == nil {
			p.moves = make(map[*abstractType]map[*Item][]*Item)
		}
		p.moves[parent] = idx
	}
	return idx[target]
}

// redoMove gives target, the restored copy of mv's deleted target, a new move
// right after mv. Callers pass the original's live moves in rising priority,
// so the copies rank as the originals did. mv is tombstoned and redone as the
// new move, so undoing mv's insertion deletes that and a redone link never
// sits on a live item.
func (u *UndoManager) redoMove(txn *Transaction, mv, target *Item) {
	origin, originRight := neighbourOrigins(mv, mv.Right)
	m := &Item{
		ID:          ID{Client: txn.doc.clientID, Clock: txn.doc.store.NextClock(txn.doc.clientID)},
		Origin:      origin,
		OriginRight: originRight,
		Left:        mv,
		Parent:      mv.Parent,
		Content:     NewContentMove(&target.ID, mv.Content.(*ContentMove).TargetLen),
	}
	mid := m.ID
	mv.redone = &mid
	m.integrate(txn, 0)
	mv.delete(txn)
}

// yjsMergeable reports whether Yjs would hold left and right as one struct
// (Item.mergeWith): adjacent, clock-contiguous, same origins and state, and
// content that merges. Map entries and moved items are never grouped.
func yjsMergeable(left, right *Item) bool {
	n := left.Content.Len()
	if left.Right != right || left.ID.Client != right.ID.Client || n == 0 ||
		left.ID.Clock+uint64(n) != right.ID.Clock || left.Deleted != right.Deleted ||
		left.redone != nil || right.redone != nil || left.MovedBy != nil || right.MovedBy != nil ||
		left.ParentSub != nil || right.ParentSub != nil ||
		!originIDEquals(right.Origin, &ID{Client: left.ID.Client, Clock: left.ID.Clock + uint64(n) - 1}) ||
		!originIDEquals(left.OriginRight, right.OriginRight) {
		return false
	}
	switch left.Content.(type) {
	case *ContentAny:
		_, ok := right.Content.(*ContentAny)
		return ok
	case *ContentString:
		_, ok := right.Content.(*ContentString)
		return ok
	case *ContentJSON:
		_, ok := right.Content.(*ContentJSON)
		return ok
	}
	return false
}

// appendContents returns dst, an item's content copy, extended by the content
// of rest, all of dst's mergeable kind (yjsMergeable). Linear in the run.
func appendContents(dst Content, rest []*Item) Content {
	if len(rest) == 0 {
		return dst
	}
	switch d := dst.(type) {
	case *ContentAny:
		for _, r := range rest {
			d.Vals = append(d.Vals, r.Content.(*ContentAny).Vals...)
		}
	case *ContentString:
		var b strings.Builder
		n := len(d.Str)
		for _, r := range rest {
			n += len(r.Content.(*ContentString).Str)
		}
		b.Grow(n)
		b.WriteString(d.Str)
		for _, r := range rest {
			b.WriteString(r.Content.(*ContentString).Str)
		}
		return NewContentString(b.String())
	case *ContentJSON:
		for _, r := range rest {
			d.Vals = append(d.Vals, r.Content.(*ContentJSON).Vals...)
		}
	}
	return dst
}

// nextSameKey returns the next item to the right holding the same map key
// (Yjs's item.right; ygo interleaves keys in one list).
func nextSameKey(item *Item) *Item {
	// itemMap holds the key's rightmost item, so nothing follows it.
	if item.Parent.itemMap[*item.ParentSub] == item {
		return nil
	}
	for r := item.Right; r != nil; r = r.Right {
		if parentSubEqual(r.ParentSub, item.ParentSub) {
			return r
		}
	}
	return nil
}

// collectedWithParent reports whether r ends its key's chain in content-less
// tombstones inside a deleted container: what a sender that collected the
// container sends, where Yjs sends GC structs that join no list and so never
// block undo. A collected value that a later live one overwrote still blocks.
func collectedWithParent(r *Item) bool {
	if pi := r.Parent.item; pi == nil || !pi.Deleted {
		return false
	}
	for ; r != nil; r = nextSameKey(r) {
		if _, gced := r.Content.(*ContentDeleted); !gced {
			return false
		}
	}
	return true
}

// deletedByStacks reports whether any stack item's deletions include id (Yjs
// isDeletedByUndoStack).
func deletedByStacks(stacks []DeleteSet, id ID) bool {
	for _, ds := range stacks {
		if ds.IsDeleted(id) {
			return true
		}
	}
	return false
}

// followRedone returns the end of item's redone chain (item itself when
// it was never redone).
func (u *UndoManager) followRedone(txn *Transaction, item *Item) *Item {
	for item != nil && item.redone != nil {
		item = u.doc.store.getItemCleanStart(txn, *item.redone)
	}
	return item
}

// iterateItems calls fn for every store item inside ds, splitting items at
// range boundaries (Yjs iterateDeletedStructs). Clients are visited in the
// order they were first deleted, as in Yjs, so when two restored values share
// a map key the same one wins.
func (u *UndoManager) iterateItems(txn *Transaction, ds DeleteSet, fn func(*Item)) {
	store := u.doc.store
	for _, client := range ds.orderedClients() {
		for _, r := range ds.clients[client] {
			if r.Len == 0 || store.Find(ID{Client: client, Clock: r.Clock}) == nil {
				continue
			}
			end := r.Clock + r.Len
			store.getItemCleanStart(txn, ID{Client: client, Clock: r.Clock})
			store.getItemCleanEnd(txn, client, end-1)
			items := store.clients[client]
			i := sort.Search(len(items), func(i int) bool { return items[i].ID.Clock >= r.Clock })
			// Collect first: fn may append to the store.
			var span []*Item
			for ; i < len(items) && items[i].ID.Clock < end; i++ {
				span = append(span, items[i])
			}
			for _, it := range span {
				fn(it)
			}
		}
	}
}

// insertedRanges returns the clocks inserted between before and after.
func insertedRanges(before, after StateVector) DeleteSet {
	ds := newDeleteSet()
	for client, end := range after {
		if start := before.Clock(client); end > start {
			ds.add(ID{Client: client, Clock: start}, int(end-start))
		}
	}
	return ds
}

// neighbourOrigins computes the Origin / OriginRight IDs for a new item placed
// immediately between left and right: left's last ID and right's first.
func neighbourOrigins(left, right *Item) (origin, originRight *ID) {
	if left != nil {
		id := left.lastID()
		origin = &id
	}
	if right != nil {
		id := right.ID
		originRight = &id
	}
	return
}

// txnAffectsScope reports whether txn changed a tracked type or a live type
// nested in one (Yjs changedParentTypes).
func (u *UndoManager) txnAffectsScope(txn *Transaction) bool {
	for t := range txn.changed {
		if t.item != nil && t.item.Deleted {
			continue
		}
		if u.typeInScope(t) {
			return true
		}
	}
	return false
}

// itemInScope reports whether item lives (at any depth) inside a tracked
// type, so a nested type's children are restored with it (Yjs isParentOf).
func (u *UndoManager) itemInScope(item *Item) bool {
	return item.Parent != nil && u.typeInScope(item.Parent)
}

// typeInScope reports whether t is a tracked type or nested in one.
func (u *UndoManager) typeInScope(t *abstractType) bool {
	for p := t; p != nil; {
		for _, s := range u.scope {
			if p == s {
				return true
			}
		}
		if p.item == nil {
			return false
		}
		p = p.item.Parent
	}
	return false
}

// cloneDeleteSet returns a sorted, compacted deep copy of ds (a cascade
// delete appends ranges out of clock order).
func cloneDeleteSet(ds DeleteSet) DeleteSet {
	out := newDeleteSet()
	out.order = ds.orderedClients()
	for client, ranges := range ds.clients {
		cp := make([]DeleteRange, len(ranges))
		copy(cp, ranges)
		out.clients[client] = cp
		out.sortAndCompact(client)
	}
	return out
}

// fireOnStackItemAdded calls all registered OnStackItemAdded callbacks.
// Must be called with u.mu held.
func (u *UndoManager) fireOnStackItemAdded(item *StackItem, redo bool) {
	for _, fn := range u.onStackItemAdded {
		fn(item, redo)
	}
}
