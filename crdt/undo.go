package crdt

import (
	"sync"
	"time"
)

// StackItem represents one reversible unit on the undo or redo stack.
// It captures what was inserted and what was deleted by a set of consecutive
// local transactions so that Undo / Redo can invert those changes.
type StackItem struct {
	// beforeState is the document state vector before the captured transaction(s).
	// Items with clocks in [beforeState[c], afterState[c]) were inserted.
	beforeState StateVector
	// afterState is the document state vector after the captured transaction(s).
	afterState StateVector
	// deletions records items deleted by the captured transaction(s).
	// These are restored (un-deleted) when this item is applied.
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

// UndoManager tracks local transactions on one or more shared types and
// provides Undo / Redo operations. Only transactions originating on this
// peer (txn.Local == true) are captured; remote updates are ignored.
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

	onStackItemAdded []func(*StackItem, bool)
}

// NewUndoManager creates an UndoManager that tracks the listed shared types.
// scope must not be empty. Multiple types can be tracked simultaneously; any
// local transaction that touches at least one scope type is captured.
func NewUndoManager(doc *Doc, scope []sharedType, opts ...UndoManagerOption) *UndoManager {
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
func (u *UndoManager) Destroy() {
	if u.unsubscribe != nil {
		u.unsubscribe()
		u.unsubscribe = nil
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

// Undo inverts the most recently captured local change. Returns true if an
// item was popped and applied; false if the undo stack is empty.
func (u *UndoManager) Undo() bool {
	u.mu.Lock()
	if len(u.undoStack) == 0 {
		u.mu.Unlock()
		return false
	}
	item := u.undoStack[len(u.undoStack)-1]
	u.undoStack = u.undoStack[:len(u.undoStack)-1]
	u.mu.Unlock()

	redoItem := u.applyStackItem(item)

	u.mu.Lock()
	if redoItem != nil {
		u.redoStack = append(u.redoStack, redoItem)
		u.fireOnStackItemAdded(redoItem, true)
	}
	u.mu.Unlock()

	return true
}

// Redo re-applies the most recently undone change. Returns true if an item
// was popped and applied; false if the redo stack is empty.
func (u *UndoManager) Redo() bool {
	u.mu.Lock()
	if len(u.redoStack) == 0 {
		u.mu.Unlock()
		return false
	}
	item := u.redoStack[len(u.redoStack)-1]
	u.redoStack = u.redoStack[:len(u.redoStack)-1]
	u.mu.Unlock()

	undoItem := u.applyStackItem(item)

	u.mu.Lock()
	if undoItem != nil {
		u.undoStack = append(u.undoStack, undoItem)
		u.fireOnStackItemAdded(undoItem, false)
	}
	u.mu.Unlock()

	return true
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
	if !u.txnAffectsScope(txn) {
		return
	}

	item := &StackItem{
		beforeState: txn.beforeState.Clone(),
		afterState:  txn.afterState.Clone(),
		deletions:   cloneDeleteSet(txn.deleteSet),
	}

	u.mu.Lock()
	defer u.mu.Unlock()

	now := time.Now()
	if len(u.undoStack) > 0 && !u.lastTxnTime.IsZero() && now.Sub(u.lastTxnTime) <= u.captureTimeout {
		// Merge: extend the top stack item to cover this transaction too.
		top := u.undoStack[len(u.undoStack)-1]
		mergeStackItems(top, item)
	} else {
		u.undoStack = append(u.undoStack, item)
		u.fireOnStackItemAdded(item, false)
	}
	u.lastTxnTime = now

	// Any new local edit invalidates the redo stack.
	u.redoStack = u.redoStack[:0]
}

// applyStackItem executes the inverse of item as a new local transaction and
// returns a new StackItem representing what that inversion did (for the
// opposite stack). Returns nil if no changes were made (e.g. all referenced
// items were GC'd).
func (u *UndoManager) applyStackItem(item *StackItem) *StackItem {
	var resultItem *StackItem

	u.doc.Transact(func(txn *Transaction) {
		// Step 1: delete items that were inserted by the captured transaction
		// (items with clocks in [beforeState[c], afterState[c])).
		for client, afterClock := range item.afterState {
			beforeClock := item.beforeState.Clock(client)
			if afterClock <= beforeClock {
				continue
			}
			for _, storeItem := range u.doc.store.clients[client] {
				if storeItem.ID.Clock < beforeClock || storeItem.ID.Clock >= afterClock {
					continue
				}
				if !storeItem.Deleted && u.itemInScope(storeItem) {
					storeItem.delete(txn)
				}
			}
		}

		// Step 2: restore items that were deleted by the captured transaction.
		for client, ranges := range item.deletions.clients {
			for _, r := range ranges {
				for _, storeItem := range u.doc.store.clients[client] {
					if storeItem.ID.Clock < r.Clock || storeItem.ID.Clock >= r.Clock+r.Len {
						continue
					}
					if !storeItem.Deleted || !u.itemInScope(storeItem) {
						continue
					}
					// Skip items whose content was freed by GC — we can't restore them.
					if _, isGC := storeItem.Content.(*ContentDeleted); isGC {
						continue
					}
					storeItem.Deleted = false
					if storeItem.Content.IsCountable() {
						storeItem.Parent.length += storeItem.Content.Len()
						storeItem.Parent.invalidatePosCache()
					}
					txn.addChanged(storeItem.Parent, storeItem.ParentSub)
				}
			}
		}

		resultItem = &StackItem{
			beforeState: txn.beforeState.Clone(),
			afterState:  txn.afterState.Clone(),
			deletions:   cloneDeleteSet(txn.deleteSet),
		}
	}, u) // origin = u so captureTransaction skips this txn

	return resultItem
}

// txnAffectsScope reports whether txn touched at least one tracked type.
func (u *UndoManager) txnAffectsScope(txn *Transaction) bool {
	for t := range txn.changed {
		for _, s := range u.scope {
			if t == s {
				return true
			}
		}
	}
	return false
}

// itemInScope reports whether item's parent type is in the tracked scope.
func (u *UndoManager) itemInScope(item *Item) bool {
	if item.Parent == nil {
		return false
	}
	for _, s := range u.scope {
		if item.Parent == s {
			return true
		}
	}
	return false
}

// mergeStackItems extends dst to cover the time range of src by taking the
// earliest beforeState and latest afterState, and merging the deletion sets.
func mergeStackItems(dst, src *StackItem) {
	// beforeState: keep the minimum clock per client (earliest starting point).
	for client, srcClock := range src.beforeState {
		if dstClock, ok := dst.beforeState[client]; !ok || srcClock < dstClock {
			dst.beforeState[client] = srcClock
		}
	}
	// afterState: keep the maximum clock per client (latest ending point).
	for client, srcClock := range src.afterState {
		if dstClock, ok := dst.afterState[client]; !ok || srcClock > dstClock {
			dst.afterState[client] = srcClock
		}
	}
	// Merge deletion sets.
	dst.deletions.Merge(src.deletions)
}

// cloneDeleteSet returns a deep copy of ds.
func cloneDeleteSet(ds DeleteSet) DeleteSet {
	out := newDeleteSet()
	for client, ranges := range ds.clients {
		cp := make([]DeleteRange, len(ranges))
		copy(cp, ranges)
		out.clients[client] = cp
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
