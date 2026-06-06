package crdt

import (
	"context"
	"sort"
	"strings"
)

// Transaction batches a set of insertions and deletions into a single atomic
// operation. Observers fire once per transaction, not once per operation,
// which keeps event handler overhead proportional to transactions not edits.
type Transaction struct {
	doc         *Doc
	Origin      any  // user-supplied tag forwarded to update observers
	Local       bool // true when the change originated on this peer
	deleteSet   DeleteSet
	beforeState StateVector
	afterState  StateVector
	// changed tracks which types (and which map keys within them) were modified.
	changed map[*abstractType]map[string]struct{}
	// newItems collects ContentString items integrated during this transaction.
	// Used by squashRuns to merge adjacent same-client runs after observers fire.
	newItems []*Item
	// mergeStructs collects right-halves produced by splitItem during this
	// transaction. tryMergeWithLefts walks the slice at commit and re-merges
	// each entry with its left neighbour when the split turned out to be
	// transient (no item was inserted between them). Mirrors Yjs JS's
	// `_mergeStructs` and powers gap #78 H2.
	mergeStructs []*Item
	// ctx is the context associated with this transaction. Set to
	// context.Background() by Transact and to the caller's ctx by
	// TransactContext. Exposed via the Ctx() method so fn can poll for
	// cancellation.
	ctx context.Context
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

// squashRuns merges adjacent ContentString items that were both created in this
// transaction and form a contiguous clock run from the same client.
//
// Safety: only items with ID.Clock >= beforeState.Clock(client) are eligible,
// ensuring pre-existing items (which snapshot clock boundaries reference) are
// never modified.
//
// squashRuns runs only for LOCAL transactions. For remote updates (bulk decode)
// items arrive already compacted from the sender or are left as individual
// units — the cost of squashing 182k remote items outweighs the benefit, since
// subsequent local edits will squash their own new items incrementally.
//
// Performance: uses a two-pointer (run) approach with strings.Builder so that
// string concatenation is O(total_run_length) rather than O(n²), and tracks
// the expected next-clock without calling left.Content.Len() on the growing
// merged string. Store compaction is a single O(n) filter pass per client.
func squashRuns(txn *Transaction) {
	if !txn.Local || len(txn.newItems) == 0 {
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

			// Walk j forward to find all items that can be squashed into left.
			// expectedClock tracks the clock boundary at the right edge of the
			// current merged item, updated with each absorbed right item's
			// original Len() — avoiding a call to left.Content.Len() (which is
			// O(string length) and would make the loop O(n²)).
			expectedClock := left.ID.Clock + uint64(left.Content.Len())
			var sb strings.Builder
			sb.WriteString(left.Content.(*ContentString).Str)

			j := i + 1
			for j < len(items) {
				right := items[j]
				if right.Deleted || right.ID.Clock < beforeClock {
					break
				}
				if expectedClock != right.ID.Clock {
					break
				}
				if left.Right != right {
					break
				}
				// right is directly adjacent and clock-contiguous: absorb it.
				rightLen := uint64(right.Content.Len()) // O(1) for single-char items
				expectedClock = right.ID.Clock + rightLen

				// Rewire linked list: splice right out.
				left.Right = right.Right
				if right.Right != nil {
					right.Right.Left = left
				}

				// Collect right's string into the builder.
				sb.WriteString(right.Content.(*ContentString).Str)

				// Schedule for store removal (appended in clock order).
				if removedByClient == nil {
					removedByClient = make(map[ClientID][]*Item, 1)
				}
				removedByClient[client] = append(removedByClient[client], right)

				j++
			}

			if j > i+1 {
				// At least one item was absorbed: commit the merged string and
				// invalidate the position cache once for the whole run.
				cs := left.Content.(*ContentString)
				cs.Str = sb.String()
				cs.utf16Len = utf16Len(cs.Str)
				if left.Parent != nil {
					left.Parent.invalidatePosCache()
				}
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

// tryMergeWithLefts walks every right-half produced by splitItem during this
// transaction and re-merges it with its left neighbour when the split turned
// out to be unnecessary (#78 H2). A split is unnecessary when, by the time the
// transaction commits, no item has been inserted between the two halves and
// no other invariant (move arbitration, parent, ParentSub, deleted state,
// origin pointers) blocks reunification.
//
// Re-merging shortens the linked list, which compounds with squashRuns (new
// runs) and gcTxnDeleteSet (deleted runs after auto-GC) to keep documents from
// fragmenting over long edit sessions. Mirrors Yjs JS's `tryToMergeWithLeft`.
//
// Caller must hold doc.mu.
func tryMergeWithLefts(txn *Transaction) {
	if len(txn.mergeStructs) == 0 {
		return
	}
	store := txn.doc.store
	for _, item := range txn.mergeStructs {
		tryMergeWithLeft(item, store)
	}
}

// tryMergeWithLeft attempts to absorb item into item.Left. Returns true when
// the merge succeeded. Conditions, all required:
//   - both items share the same client, parent, ParentSub, MovedBy, and Deleted state
//   - left.Right == item (still directly adjacent in the linked list)
//   - clocks are contiguous: left.ID.Clock + left.Content.Len() == item.ID.Clock
//   - item.Origin points to the last clock of left (Yjs origin invariant)
//   - content types match and support merging (ContentString, ContentAny,
//     ContentJSON, ContentDeleted)
//
// On success: left's content is extended in place, the linked list is spliced
// past item, and item is removed from the store.
func tryMergeWithLeft(item *Item, store *StructStore) bool {
	left := item.Left
	if left == nil {
		return false
	}
	if left.ID.Client != item.ID.Client {
		return false
	}
	if left.Right != item {
		return false
	}
	if left.ID.Clock+uint64(left.Content.Len()) != item.ID.Clock {
		return false
	}
	if left.Deleted != item.Deleted {
		return false
	}
	if !parentSubEqual(left.ParentSub, item.ParentSub) {
		return false
	}
	if left.Parent != item.Parent {
		return false
	}
	if left.MovedBy != item.MovedBy {
		return false
	}
	// item.Origin must reference the last clock of left for the split to be
	// reversible. (splitItem always sets Origin this way; foreign updates may
	// set Origin differently, in which case we leave the items split.)
	if item.Origin == nil {
		return false
	}
	expectedLast := left.ID.Clock + uint64(left.Content.Len()) - 1
	if item.Origin.Client != left.ID.Client || item.Origin.Clock != expectedLast {
		return false
	}

	// Content-type match + in-place extension.
	switch lc := left.Content.(type) {
	case *ContentString:
		rc, ok := item.Content.(*ContentString)
		if !ok {
			return false
		}
		lc.Str += rc.Str
		lc.utf16Len += rc.utf16Len
	case *ContentAny:
		rc, ok := item.Content.(*ContentAny)
		if !ok {
			return false
		}
		lc.Vals = append(lc.Vals, rc.Vals...)
	case *ContentJSON:
		rc, ok := item.Content.(*ContentJSON)
		if !ok {
			return false
		}
		lc.Vals = append(lc.Vals, rc.Vals...)
	case *ContentDeleted:
		rc, ok := item.Content.(*ContentDeleted)
		if !ok {
			return false
		}
		lc.length += rc.length
	default:
		// ContentEmbed, ContentType, ContentFormat, ContentBinary, ContentDoc,
		// and ContentMove are single-value or not splittable — never appear
		// in mergeStructs in mergeable form.
		return false
	}

	// Splice item out of the linked list.
	left.Right = item.Right
	if item.Right != nil {
		item.Right.Left = left
	}
	if left.Parent != nil {
		left.Parent.invalidatePosCache()
	}

	// Remove item from the store's per-client slice.
	storeItems := store.clients[item.ID.Client]
	for i, it := range storeItems {
		if it == item {
			store.clients[item.ID.Client] = append(storeItems[:i], storeItems[i+1:]...)
			break
		}
	}
	return true
}
