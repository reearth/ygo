package crdt

import (
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
