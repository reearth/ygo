package crdt

import (
	"context"
	"maps"
	"reflect"
	"slices"
	"sort"

	"github.com/reearth/ygo/encoding"
)

// cleanupTestHook, when set by a test, runs after every cleanup's deletions.
var cleanupTestHook func()

// needsFormattingCleanup reports whether a committed remote transaction
// changed a text with cleanFormatting set, which is when Yjs's YText observer
// sets _needFormattingCleanup; a root placeholder has no such observer.
func needsFormattingCleanup(txn *Transaction) bool {
	if txn.Local {
		return false
	}
	for t := range txn.changed {
		if _, raw := t.owner.(*rawType); raw {
			continue
		}
		if t.cleanFormatting && (t.item == nil || !t.item.Deleted) {
			return true
		}
	}
	return false
}

// changedText reports whether txn changed a text, which an observer could
// still give its first marker.
func changedText(txn *Transaction) bool {
	for t := range txn.changed {
		switch t.owner.(type) {
		case *YText, *YXmlText:
			return true
		}
	}
	return false
}

// afterRemoteObservers finishes a remote transaction once its observers have
// fired: it runs the format cleanup, GCs the transaction, and fires the
// cleanup's observers. A panicking cleanup still emits what it deleted, then
// re-panics.
func (d *Doc) afterRemoteObservers(remote *Transaction) {
	var phase2 func()
	var panicked any
	func() {
		d.mu.Lock()
		defer d.mu.Unlock()
		defer func() {
			if p := recover(); p != nil {
				panicked = p
			}
		}()
		if remote.formatCleanup {
			phase2, panicked = d.formattingCleanupLocked(remote)
		}
		if d.gc && d.undoManagerCount == 0 {
			gcTxnDeleteSet(d, remote)
		}
	}()
	if phase2 != nil {
		phase2()
	}
	if panicked != nil {
		panic(panicked)
	}
}

// formattingCleanupLocked runs Yjs's cleanupYTextAfterTransaction for the
// remote transaction remote in a new local transaction with a nil origin, as
// Yjs does, and returns that transaction's observer phase and any panic the
// cleanup raised. A cleanup that deleted nothing fires only the
// after-transaction observers, as Yjs emits no update without content. Like
// a panicking Transact, a panicking cleanup still emits the deletions it
// made. It must run under d.mu before remote's deleted content is GC'd.
func (d *Doc) formattingCleanupLocked(remote *Transaction) (phase2 func(), panicked any) {
	txn := &Transaction{
		doc:         d,
		Local:       true,
		deleteSet:   newOrderedDeleteSet(),
		beforeState: d.store.StateVector(),
		changed:     make(map[*abstractType]map[string]struct{}, 1),
		ctx:         context.Background(),
	}
	func() {
		defer func() { panicked = recover() }()
		cleanupYTextAfterTransaction(remote, txn)
		if cleanupTestHook != nil {
			cleanupTestHook()
		}
	}()
	txn.afterState = d.store.StateVector()
	txn.done = true
	if len(txn.changed) == 0 {
		return afterTxnPhase(d, txn), panicked
	}
	func() {
		if panicked != nil {
			defer func() { _ = recover() }()
		}
		phase2 = buildPhase2(d, txn)
	}()
	if d.gc && d.undoManagerCount == 0 {
		gcTxnDeleteSet(d, txn)
	}
	return phase2, panicked
}

// afterTxnPhase fires only d's after-transaction observers for txn.
func afterTxnPhase(d *Doc, txn *Transaction) func() {
	if len(d.onAfterTxn) == 0 {
		return nil
	}
	fns := make([]func(*Transaction), len(d.onAfterTxn))
	for i, s := range d.onAfterTxn {
		fns[i] = s.fn
	}
	return func() {
		for _, fn := range fns {
			fn(txn)
		}
	}
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
				if p == nil || !p.cleanFormatting || needFull[p] {
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
	startAttrs := fmtAttrs{}
	cur := fmtAttrs{}
	for end := t.start; end != nil; end = end.Right {
		if end.Deleted {
			continue
		}
		if cf, ok := end.Content.(*ContentFormat); ok {
			cur.update(cf)
			continue
		}
		cleanupFormattingGap(txn, start, end, startAttrs, cur)
		startAttrs = maps.Clone(cur)
		start = end
	}
}

// fmtAttrs maps each attribute to the marker that set it, so values keep
// the identity Yjs's === sees.
type fmtAttrs map[string]*ContentFormat

func (a fmtAttrs) update(cf *ContentFormat) {
	if cf.Val == nil {
		delete(a, cf.Key)
	} else {
		a[cf.Key] = cf
	}
}

// cleanupFormattingGap deletes the markers between start and the next live
// content item that are overwritten within the gap or restate startAttrs,
// adjusting curAttrs for markers before curr. Mirrors Yjs.
func cleanupFormattingGap(txn *Transaction, start, curr *Item, startAttrs, curAttrs fmtAttrs) {
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
		startCF := startAttrs[cf.Key]
		if endFormats[cf.Key] != cf || jsIdentical(startCF, cf) {
			start.delete(txn)
			if !reachedCurr && jsIdentical(curAttrs[cf.Key], cf) && !jsIdentical(startCF, cf) {
				if startCF == nil {
					delete(curAttrs, cf.Key)
				} else {
					curAttrs[cf.Key] = startCF
				}
			}
		}
		if !reachedCurr && !start.Deleted {
			curAttrs.update(cf)
		}
	}
}

// jsIdentical is JavaScript's === on two markers' values, a nil marker
// standing for an absent attribute (null). Scalars compare by value; a
// composite value is identical only to itself, as each decoded Yjs marker
// holds its own object.
func jsIdentical(a, b *ContentFormat) bool {
	if a == b {
		return true
	}
	var av, bv any
	if a != nil {
		av = a.Val
	}
	if b != nil {
		bv = b.Val
	}
	if av == nil || bv == nil {
		return av == nil && bv == nil
	}
	_, aBig := av.(encoding.BigInt)
	_, bBig := bv.(encoding.BigInt)
	if aBig || bBig {
		return av == bv
	}
	va, vb := reflect.ValueOf(av), reflect.ValueOf(bv)
	if fa, ok := jsNumber(va); ok {
		fb, ok := jsNumber(vb)
		return ok && fa == fb
	}
	switch va.Kind() {
	case reflect.String, reflect.Bool:
		return va.Type() == vb.Type() && av == bv
	}
	return false
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
