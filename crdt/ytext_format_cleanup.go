package crdt

import (
	"context"
	"reflect"
	"slices"
	"sort"
)

// needsFormattingCleanup reports whether a committed remote transaction
// changed a text that has formatting, which is when Yjs's YText observer sets
// _needFormattingCleanup.
func needsFormattingCleanup(txn *Transaction) bool {
	if txn.Local {
		return false
	}
	for t := range txn.changed {
		if t.hasFormatting && (t.item == nil || !t.item.Deleted) {
			return true
		}
	}
	return false
}

// formattingCleanupLocked runs Yjs's cleanupYTextAfterTransaction for the
// remote transaction remote in a new local transaction with a nil origin, as
// Yjs does, and returns that transaction's observer phase (nil when it changed
// nothing). It must run under d.mu before remote's deleted content is GC'd.
func (d *Doc) formattingCleanupLocked(remote *Transaction) func() {
	txn := &Transaction{
		doc:         d,
		Local:       true,
		deleteSet:   newOrderedDeleteSet(),
		beforeState: d.store.StateVector(),
		changed:     make(map[*abstractType]map[string]struct{}, 1),
		ctx:         context.Background(),
	}
	cleanupYTextAfterTransaction(remote, txn)
	txn.afterState = d.store.StateVector()
	txn.done = true
	if len(txn.changed) == 0 {
		return nil
	}
	phase2 := buildPhase2(d, txn)
	if d.gc && d.undoManagerCount == 0 {
		gcTxnDeleteSet(d, txn)
	}
	return phase2
}

// cleanupYTextAfterTransaction deletes the format markers remote left
// redundant, in txn. Mirrors Yjs cleanupYTextAfterTransaction: a text that
// gained a live marker gets the full cleanup, any other formatted text the
// contextless cleanup around each deleted item.
func cleanupYTextAfterTransaction(remote, txn *Transaction) {
	store := txn.doc.store
	var full []*abstractType
	needFull := map[*abstractType]bool{}
	addFull := func(t *abstractType) {
		if !needFull[t] {
			needFull[t] = true
			full = append(full, t)
		}
	}
	for _, client := range sortedStateClients(remote.afterState) {
		before, after := remote.beforeState.Clock(client), remote.afterState.Clock(client)
		storeRange(store, client, before, after, func(it *Item) {
			if !it.Deleted && isContentFormat(it) && it.Parent != nil {
				addFull(it.Parent)
			}
		})
	}
	// Visit deletions as Yjs does: clients in first-deletion order, each
	// client's ranges sorted and merged.
	ds := remote.deleteSet
	for _, client := range ds.orderedClients() {
		sorted := DeleteSet{clients: map[ClientID][]DeleteRange{client: slices.Clone(ds.clients[client])}}
		sorted.sortAndCompact(client)
		for _, r := range sorted.clients[client] {
			storeRange(store, client, r.Clock, r.Clock+r.Len, func(it *Item) {
				p := it.Parent
				if p == nil || !p.hasFormatting || needFull[p] {
					return
				}
				if isContentFormat(it) {
					addFull(p)
				} else {
					cleanupContextlessFormattingGap(txn, it)
				}
			})
		}
	}
	for _, t := range full {
		cleanupYTextFormatting(txn, t)
	}
}

func sortedStateClients(sv StateVector) []ClientID {
	out := make([]ClientID, 0, len(sv))
	for c := range sv {
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// storeRange calls fn for every stored item of client overlapping clocks
// [from, to).
func storeRange(store *StructStore, client ClientID, from, to uint64, fn func(*Item)) {
	items := store.clients[client]
	i := sort.Search(len(items), func(i int) bool { return items[i].ID.Clock > from }) - 1
	if i < 0 {
		i = 0
	}
	for ; i < len(items) && items[i].ID.Clock < to; i++ {
		if it := items[i]; it.ID.Clock+uint64(it.Content.Len()) > from {
			fn(it)
		}
	}
}

// cleanupContextlessFormattingGap keeps only the last live marker per key in
// the run of deleted and non-countable items around item. Mirrors Yjs.
func cleanupContextlessFormattingGap(txn *Transaction, item *Item) {
	for item != nil && item.Right != nil && (item.Right.Deleted || !item.Right.Content.IsCountable()) {
		item = item.Right
	}
	seen := map[string]bool{}
	for item != nil && (item.Deleted || !item.Content.IsCountable()) {
		if cf, ok := item.Content.(*ContentFormat); ok && !item.Deleted {
			if seen[cf.Key] {
				item.delete(txn)
			} else {
				seen[cf.Key] = true
			}
		}
		item = item.Left
	}
}

// cleanupYTextFormatting is Yjs's cleanupYTextFormatting: it runs
// cleanupFormattingGap from each live content item, which in Yjs only ever
// cleans the markers before the first one.
func cleanupYTextFormatting(txn *Transaction, t *abstractType) {
	start := t.start
	startAttrs := Attributes{}
	cur := Attributes{}
	for end := t.start; end != nil; end = end.Right {
		if end.Deleted {
			continue
		}
		if cf, ok := end.Content.(*ContentFormat); ok {
			updateAttr(cur, cf)
			continue
		}
		cleanupFormattingGap(txn, start, end, startAttrs, cur)
		startAttrs = cloneAttributes(cur)
		start = end
	}
}

// cleanupFormattingGap deletes the markers between start and the next live
// content item that are overwritten within the gap or restate startAttrs,
// adjusting curAttrs for markers before curr. Mirrors Yjs, including its ===
// comparisons (composite values are equal only to themselves).
func cleanupFormattingGap(txn *Transaction, start, curr *Item, startAttrs, curAttrs Attributes) {
	end := start
	endFormats := map[string]*ContentFormat{}
	for end != nil && (!end.Content.IsCountable() || end.Deleted) {
		if cf, ok := end.Content.(*ContentFormat); ok && !end.Deleted {
			endFormats[cf.Key] = cf
		}
		end = end.Right
	}
	reachedCurr := false
	for ; start != end; start = start.Right {
		if curr == start {
			reachedCurr = true
		}
		cf, ok := start.Content.(*ContentFormat)
		if !ok || start.Deleted {
			continue
		}
		startVal := startAttrs[cf.Key]
		if endFormats[cf.Key] != cf || jsIdentical(startVal, cf.Val) {
			start.delete(txn)
			if !reachedCurr && jsIdentical(curAttrs[cf.Key], cf.Val) && !jsIdentical(startVal, cf.Val) {
				if startVal == nil {
					delete(curAttrs, cf.Key)
				} else {
					curAttrs[cf.Key] = startVal
				}
			}
		}
		if !reachedCurr && !start.Deleted {
			updateAttr(curAttrs, cf)
		}
	}
}

// jsIdentical is JavaScript's === on attribute values: numbers by value,
// maps and slices by identity.
func jsIdentical(a, b any) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	va, vb := reflect.ValueOf(a), reflect.ValueOf(b)
	switch va.Kind() {
	case reflect.Map, reflect.Slice:
		return va.Kind() == vb.Kind() && va.Pointer() == vb.Pointer() && va.Len() == vb.Len()
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64,
		reflect.Float32, reflect.Float64:
		fa, okA := jsNumber(va)
		fb, okB := jsNumber(vb)
		return okA && okB && fa == fb
	}
	return va.Type() == vb.Type() && va.Comparable() && a == b
}

func jsNumber(v reflect.Value) (float64, bool) {
	switch {
	case v.CanInt():
		return float64(v.Int()), true
	case v.CanUint():
		return float64(v.Uint()), true
	case v.CanFloat():
		return v.Float(), true
	}
	return 0, false
}
