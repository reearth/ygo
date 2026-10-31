package crdt

import (
	"context"
	"slices"
	"sort"
)

// Transaction batches a set of insertions and deletions into a single atomic
// operation. Observers fire once per transaction, not once per operation,
// which keeps event handler overhead proportional to transactions not edits.
type Transaction struct {
	doc         *Doc
	Origin      any  // user-supplied tag forwarded to update observers
	Local       bool // true when the change originated on this peer; false while applying a remote update
	deleteSet   DeleteSet
	beforeState StateVector
	afterState  StateVector
	// changed tracks which types (and which map keys within them) were modified.
	changed map[*abstractType]map[string]struct{}
	// newItems collects ContentString items integrated during this transaction.
	// Used by squashRuns to merge same-client runs before the transaction's
	// update is encoded.
	newItems []*Item
	// mergeStructs collects right-halves produced by splitItem during this
	// transaction. mergeTxnStructs tries to merge each with its neighbours at
	// commit, so a split that turned out to be transient (no item was inserted
	// between the halves) is reversed. Mirrors Yjs JS's `_mergeStructs`.
	mergeStructs []*Item
	// rearbitrate queues, per parent, move targets whose winning ContentMove
	// was tombstoned; rearbitrateMoves resolves them in one pass at commit.
	rearbitrate map[*abstractType]map[*Item]struct{}
	// movedBefore holds each move target's MovedBy as it was before this
	// transaction first changed it, so YArray deltas can diff the old render.
	movedBefore map[*Item]*Item
	// subdocsAdded/subdocsRemoved/subdocsLoaded track subdocument lifecycle
	// changes made during this transaction (#63). Populated by Item.integrate
	// and Item.delete when the item's Content is a *ContentDoc. Reconciled
	// into Doc.subdocs and turned into a SubdocsEvent by buildPhase2 at commit.
	// An add followed by a delete of the same doc within the same transaction
	// cancels out (removed from subdocsAdded/subdocsLoaded, never added to
	// subdocsRemoved) so no event fires for a net no-op.
	subdocsAdded   map[*Doc]struct{}
	subdocsRemoved map[*Doc]struct{}
	subdocsLoaded  map[*Doc]struct{}
	// done is set (under d.mu, just before the commit releases it) once this
	// transaction has committed. The transaction-scoped root accessors check
	// it and fail loudly instead of silently mutating d.share without the
	// lock: the realistic misuses — an OnAfterTransaction observer (which
	// receives the *Transaction after unlock) or code retaining the txn past
	// its callback — run on the same goroutine that committed, so program
	// order guarantees they observe done == true. A transaction smuggled to
	// another goroutine while still live is unaffected (that is already
	// undefined for every txn method); for one used cross-goroutine after
	// commit the check is best-effort.
	done bool
	// ctx is the context associated with this transaction. Set to
	// context.Background() by Transact and to the caller's ctx by
	// TransactContext. Exposed via the Ctx() method so fn can poll for
	// cancellation.
	ctx context.Context
	// formatCleanup marks a remote transaction whose format cleanup runs
	// once its observers have fired; afterTypeObservers, when set, decides
	// it after the type observers (typeObserved: there are some), where
	// Yjs does.
	formatCleanup      bool
	typeObserved       bool
	afterTypeObservers func()
}

// Ctx returns the context associated with this transaction. Transactions
// started via Transact return context.Background(); transactions started
// via TransactContext return the caller's ctx. fn can poll Ctx().Err()
// or <-Ctx().Done() to detect cancellation and return early.
//
// Returning early from fn commits whatever mutations have been made so
// far — there is no rollback. Callers needing atomicity should recover
// and reconcile via sync or recreate the doc from persistence.
func (t *Transaction) Ctx() context.Context {
	return t.ctx
}

// Transaction-scoped root accessors (issue #138).
//
// The Doc-level GetText/GetMap/GetArray/GetXmlFragment take the document
// lock, which Transact already holds for the whole callback — calling them
// inside fn self-deadlocks a non-reentrant lock. These methods resolve (and
// create-on-miss) the same root types WITHOUT re-locking, reusing the lock
// the transaction holds — so the natural in-transaction call simply works,
// mirroring yrs, where root handles are resolved through the transaction.
//
// They are valid ONLY inside the Transact callback that received this
// Transaction (the same lifetime rule as passing txn to Insert/Set/Delete).
// After the transaction commits — in an observer, or via a retained txn —
// they PANIC instead of mutating d.share without the lock.

// assertLive panics when the transaction has already committed. See the
// Transaction.done field for why this is deterministic for the realistic
// (same-goroutine) misuses.
func (t *Transaction) assertLive(method string) {
	if t.done {
		panic("crdt: Transaction." + method + " used after commit (inside an observer or " +
			"retained past the Transact callback) — resolve root types via doc." + method +
			" there (issue #138)")
	}
}

// GetText returns the named root YText, creating it if it does not exist.
// Safe to call inside the Transact callback; see the section comment above.
func (t *Transaction) GetText(name string) *YText {
	checkUTF8("Transaction.GetText", "name", name)
	t.assertLive("GetText")
	return t.doc.getTextLocked(name)
}

// GetMap returns the named root YMap, creating it if it does not exist.
// Safe to call inside the Transact callback; see the section comment above.
func (t *Transaction) GetMap(name string) *YMap {
	checkUTF8("Transaction.GetMap", "name", name)
	t.assertLive("GetMap")
	return t.doc.getMapLocked(name)
}

// GetArray returns the named root YArray, creating it if it does not exist.
// Safe to call inside the Transact callback; see the section comment above.
func (t *Transaction) GetArray(name string) *YArray {
	checkUTF8("Transaction.GetArray", "name", name)
	t.assertLive("GetArray")
	return t.doc.getArrayLocked(name)
}

// GetXmlFragment returns the named root YXmlFragment, creating it if it does
// not exist. Safe to call inside the Transact callback; see the section
// comment above.
func (t *Transaction) GetXmlFragment(name string) *YXmlFragment {
	checkUTF8("Transaction.GetXmlFragment", "name", name)
	t.assertLive("GetXmlFragment")
	return t.doc.getXmlFragmentLocked(name)
}

// squashRuns merges adjacent ContentString items that were both created in this
// transaction and form a contiguous clock run from the same client.
//
// It runs before the transaction's update is encoded, so the update carries
// each new text run as one struct; mergeTxnStructs runs after the encode and
// also merges a new run into the item it continues. Only items with ID.Clock
// >= beforeState.Clock(client) are eligible, so the encode still starts at an
// item boundary.
//
// Two items merge under Item.mergeable, Yjs's Item.mergeWith conditions: the
// merged struct must still place every character where it was.
//
// Store compaction is a single O(n) filter pass per client.
func squashRuns(txn *Transaction) {
	if len(txn.newItems) == 0 {
		return
	}

	// Group new ContentString items by client.
	byClient := make(map[ClientID][]*Item, 4)
	for _, item := range txn.newItems {
		if !item.Deleted {
			byClient[item.ID.Client] = append(byClient[item.ID.Client], item)
		}
	}

	store := txn.doc.store

	// removedByClient collects items squashed into their left neighbour.
	// Items are appended in clock order (squashRuns processes them that way),
	// so the compaction pass can use a two-pointer merge instead of a hash
	// lookup — avoiding 182k map-insert operations on the hot decode path.
	var removedByClient map[ClientID][]*Item

	for client, items := range byClient {
		if len(items) < 2 {
			continue
		}
		sort.Slice(items, func(i, j int) bool {
			return items[i].ID.Clock < items[j].ID.Clock
		})
		beforeClock := txn.beforeState.Clock(client)

		i := 0
		for i < len(items) {
			left := items[i]

			// Skip ineligible run starts.
			if left.Deleted || left.ID.Clock < beforeClock {
				i++
				continue
			}

			j := i + 1
			for ; j < len(items); j++ {
				right := items[j]
				// Only store neighbours merge: right's clock must follow left's.
				if right.ID.Clock != left.ID.Clock+uint64(left.Content.Len()) || !left.mergeable(right) {
					break
				}
				left.absorb(right)
				// Schedule for store removal (appended in clock order).
				if removedByClient == nil {
					removedByClient = make(map[ClientID][]*Item, 1)
				}
				removedByClient[client] = append(removedByClient[client], right)
			}
			if j > i+1 {
				// Compact items slice: skip over all absorbed entries.
				items = append(items[:i+1], items[j:]...)
			}
			i++
		}
	}

	// Single O(n) compaction pass per client using a two-pointer merge.
	// removed is already in clock order (squashRuns processes items that way),
	// matching the clock-sorted order of storeItems — no hash lookup needed.
	for client, removed := range removedByClient {
		storeItems := store.clients[client]
		n, ri := 0, 0
		for _, item := range storeItems {
			if ri < len(removed) && item == removed[ri] {
				ri++ // skip this squashed item
			} else {
				storeItems[n] = item
				n++
			}
		}
		// Zero out the tail to release GC references.
		for k := n; k < len(storeItems); k++ {
			storeItems[k] = nil
		}
		store.clients[client] = storeItems[:n]
	}
}

// addChanged records that a type was modified, optionally under a specific key.
func (txn *Transaction) addChanged(t *abstractType, key string) {
	keys, ok := txn.changed[t]
	if !ok {
		keys = make(map[string]struct{})
		txn.changed[t] = keys
	}
	keys[key] = struct{}{}
}

// addSubdocAdded records that d was newly embedded (integrated) during this
// transaction (#63).
func (t *Transaction) addSubdocAdded(d *Doc) {
	if t.subdocsAdded == nil {
		t.subdocsAdded = map[*Doc]struct{}{}
	}
	t.subdocsAdded[d] = struct{}{}
}

// addSubdocLoaded records that d should be reported as loaded in this
// transaction's SubdocsEvent (#63).
func (t *Transaction) addSubdocLoaded(d *Doc) {
	if t.subdocsLoaded == nil {
		t.subdocsLoaded = map[*Doc]struct{}{}
	}
	t.subdocsLoaded[d] = struct{}{}
}

// addSubdocRemoved records that d was detached (deleted) during this
// transaction (#63).
func (t *Transaction) addSubdocRemoved(d *Doc) {
	if t.subdocsRemoved == nil {
		t.subdocsRemoved = map[*Doc]struct{}{}
	}
	t.subdocsRemoved[d] = struct{}{}
}

// cleanupTxnLocked finishes txn as Yjs cleanupTransactions does once its
// observer deltas are built: it collects the content txn deleted and then
// merges structs with mergeTxnStructs, which runs even when the GC does not.
// Caller must hold d.mu.
//
// The GC is skipped while an UndoManager is attached: undoing a deletion
// re-inserts a copy of the deleted item's content (applyStackItem → redoItem),
// which needs that content. Yjs keeps such items with a per-item keep flag;
// ygo suspends the transaction GC for the undo manager's lifetime instead.
// RunGC remains available as the explicit manual entry point.
func cleanupTxnLocked(d *Doc, txn *Transaction) {
	if d.gc && d.undoManagerCount == 0 {
		gcTxnDeleteSet(d, txn)
	}
	mergeTxnStructs(txn)
}

// mergeTxnStructs merges structs at the end of txn as Yjs cleanupTransactions
// does once the transaction's deleted content is collected: each struct in
// txn's delete set, each struct txn added and each split half it recorded
// merges with the structs on its left while each pair encodes as one struct.
// The merge also reaches a struct of an earlier transaction, so a run typed
// or deleted one transaction at a time stays one struct.
func mergeTxnStructs(txn *Transaction) {
	for client := range txn.deleteSet.clients {
		mergeClientStructs(txn, client)
	}
	for client, clock := range txn.afterState {
		if _, done := txn.deleteSet.clients[client]; !done && txn.beforeState.Clock(client) != clock {
			mergeClientStructs(txn, client)
		}
	}
	forHalfClients(txn, func(client ClientID) {
		_, deleted := txn.deleteSet.clients[client]
		if !deleted && txn.beforeState.Clock(client) == txn.afterState.Clock(client) {
			mergeClientStructs(txn, client)
		}
	})
}

// mergeSplitHalves merges each split half txn recorded back into its left
// half. It runs before the transaction's update is encoded, so the update
// carries a transient split as one struct; mergeTxnStructs merges the rest
// later.
func mergeSplitHalves(txn *Transaction) {
	store := txn.doc.store
	for _, half := range txn.mergeStructs {
		structs := store.clients[half.ID.Client]
		pos := findStructIndex(structs, half.ID.Clock)
		if pos < 1 || structs[pos] != half {
			continue
		}
		if left := structs[pos-1]; left.mergeable(half) {
			left.absorb(half)
			store.clients[half.ID.Client] = slices.Delete(structs, pos, pos+1)
		}
	}
}

// forHalfClients calls fn once for each client that has a split half in txn.
func forHalfClients(txn *Transaction, fn func(ClientID)) {
	var done map[ClientID]struct{}
	for _, half := range txn.mergeStructs {
		client := half.ID.Client
		if _, ok := done[client]; ok {
			continue
		}
		if done == nil {
			done = make(map[ClientID]struct{}, 1)
		}
		done[client] = struct{}{}
		fn(client)
	}
}

// mergeSpan is a range of indices, lo to hi inclusive, into a client's
// structs: index p names the pair of structs p-1 and p.
type mergeSpan struct{ lo, hi int }

// mergeClientStructs merges the pairs of client's structs that Yjs's
// cleanupTransactions tries for txn: tryMergeDeleteSet for txn's deleted
// ranges, then tryToMergeWithLefts for the structs txn added and for each
// split half txn recorded.
//
// Whether two neighbours merge does not depend on the merges around them, and
// every change that makes two neighbours mergeable (adding, deleting or
// splitting a struct) puts the pair in a span. So no mergeable pair is left
// from earlier transactions, and one forward pass gives Yjs's result and moves
// the client's structs once. The exception is a change of a struct's MovedBy,
// an ygo extension Yjs does not have: a pair it makes mergeable stays apart
// until a later transaction tries it.
func mergeClientStructs(txn *Transaction, client ClientID) {
	store := txn.doc.store
	structs := store.clients[client]
	n := len(structs)
	if n < 2 {
		return
	}
	ranges := txn.deleteSet.clients[client]
	spans := make([]mergeSpan, 0, len(ranges)+1)
	for _, r := range ranges {
		// From the struct after the range down to the range's first struct.
		lo := max(sort.Search(n, func(i int) bool { return structs[i].ID.Clock >= r.Clock }), 1)
		if hi := min(n-1, findStructIndex(structs, r.Clock+r.Len-1)+1); lo <= hi {
			spans = append(spans, mergeSpan{lo, hi})
		}
	}
	if before := txn.beforeState.Clock(client); before != txn.afterState.Clock(client) {
		// The added structs, the first of them with the struct before it.
		spans = append(spans, mergeSpan{max(findStructIndex(structs, before), 1), n - 1})
	}
	for _, half := range txn.mergeStructs {
		if half.ID.Client == client {
			// The half with the struct before it and the struct after it.
			pos := findStructIndex(structs, half.ID.Clock)
			spans = append(spans, mergeSpan{max(pos, 1), min(pos+1, n-1)})
		}
	}
	if len(spans) == 0 {
		return
	}
	// A merge carries on to the left while the pairs there merge.
	for i := range spans {
		for spans[i].lo > 1 && structs[spans[i].lo-1].mergeable(structs[spans[i].lo]) {
			spans[i].lo--
		}
	}
	slices.SortFunc(spans, func(a, b mergeSpan) int { return a.lo - b.lo })
	w, next := 0, 0 // structs[:w] is done; structs[next:] is not yet visited
	for i := 0; i < len(spans); {
		lo, hi := spans[i].lo, spans[i].hi
		for i++; i < len(spans) && spans[i].lo <= hi+1; i++ {
			hi = max(hi, spans[i].hi)
		}
		if w != next {
			copy(structs[w:], structs[next:lo])
		}
		w += lo - next
		for r := lo; r <= hi; r++ {
			if structs[w-1].mergeable(structs[r]) {
				structs[w-1].absorb(structs[r])
			} else {
				structs[w] = structs[r]
				w++
			}
		}
		next = hi + 1
	}
	if w == next {
		return
	}
	w += copy(structs[w:], structs[next:])
	clear(structs[w:])
	store.clients[client] = structs[:w]
}
