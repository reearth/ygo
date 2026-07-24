package crdt

import (
	"fmt"
	"math/rand"
	"testing"
)

// coldLeftNeighbour is the reference (marker-free) linear walk used as the oracle.
func coldLeftNeighbour(t *abstractType, index int) (*Item, int) {
	if index <= 0 {
		return nil, 0
	}
	counted := 0
	for item := t.start; item != nil; item = item.Right {
		if item.Deleted || !item.Content.IsCountable() {
			continue
		}
		n := item.Content.Len()
		if counted+n >= index {
			return item, counted
		}
		counted += n
	}
	return nil, counted
}

func TestSearchMarker_ROMatchesCold_Text(t *testing.T) {
	d := New(WithClientID(1))
	txt := d.GetText("t")
	d.Transact(func(tr *Transaction) { txt.Insert(tr, 0, "hello world foo bar baz", nil) })
	at := txt.baseType()
	for i := 0; i <= txt.Len(); i++ {
		gi, gidx := at.findMarkerRO(i)
		ci, cidx := coldLeftNeighbour(at, i)
		if gi != ci || gidx != cidx {
			t.Fatalf("index %d: findMarkerRO=(%p,%d) cold=(%p,%d)", i, gi, gidx, ci, cidx)
		}
	}
}

// TestSearchMarker_ROMatchesCold_LargeDocRandom builds a ~5k-char document
// via many separate inserts (so the linked list has many distinct items,
// some possibly deleted), hand-seeds a handful of search markers with
// correct (index, item) pairs taken from the cold oracle, and asserts that
// findMarkerRO — using those markers as its nearest-marker starting points —
// still agrees with the marker-free oracle for 500 random indices. This
// exercises the nearest-marker-then-walk-right/left code paths in
// findMarkerRO, not just the empty-markers fallback covered by the test
// above.
func TestSearchMarker_ROMatchesCold_LargeDocRandom(t *testing.T) {
	d := New(WithClientID(1))
	txt := d.GetText("t")

	rng := rand.New(rand.NewSource(42))

	// Build a large, fragmented document: many small inserts at random
	// positions, plus occasional deletes, so the linked list contains a
	// realistic mix of live and tombstoned items.
	d.Transact(func(tr *Transaction) {
		for txt.Len() < 5000 {
			chunk := fmt.Sprintf("chunk%d ", rng.Intn(100000))
			pos := rng.Intn(txt.Len() + 1)
			txt.Insert(tr, pos, chunk, nil)
			if txt.Len() > 20 && rng.Intn(5) == 0 {
				delPos := rng.Intn(txt.Len() - 10)
				txt.Delete(tr, delPos, 3+rng.Intn(5))
			}
		}
	})

	at := txt.baseType()

	// Hand-seed markers at a handful of positions using the cold oracle as
	// the source of truth for (item, index) pairs, mimicking what a later
	// write-path would install — but findMarkerRO itself must never write
	// t.markers.
	seedPositions := []int{1, 100, 777, 1234, 2500, 3333, 4321, 4999, txt.Len()}
	markers := make([]searchMarker, 0, len(seedPositions))
	for _, p := range seedPositions {
		item, idx := coldLeftNeighbour(at, p)
		if item == nil {
			continue
		}
		markers = append(markers, searchMarker{item: item, index: idx})
	}
	at.markers = markers

	for i := 0; i < 500; i++ {
		idx := rng.Intn(txt.Len() + 1)
		gi, gidx := at.findMarkerRO(idx)
		ci, cidx := coldLeftNeighbour(at, idx)
		if gi != ci || gidx != cidx {
			t.Fatalf("index %d: findMarkerRO=(%p,%d) cold=(%p,%d)", idx, gi, gidx, ci, cidx)
		}
	}

	// Sanity: the marker slice itself must be untouched by findMarkerRO —
	// it must still have exactly the entries we seeded (read-only lookup
	// never appends/truncates/reassigns t.markers).
	if len(at.markers) != len(markers) {
		t.Fatalf("t.markers length changed by findMarkerRO: got %d, want %d", len(at.markers), len(markers))
	}
}

// TestSearchMarker_InsertMatchesCold drives the mutating write-path lookup
// (leftNeighbourAt → findMarkerMut) with several insert orderings and asserts
// the resulting document is identical to a force-cold run (disableMarkers),
// i.e. the search-marker maintenance never changes the position a local
// insert resolves to.
func TestSearchMarker_InsertMatchesCold(t *testing.T) {
	build := func(cold bool, order []int) string {
		d := New(WithClientID(1))
		txt := d.GetText("t")
		txt.baseType().disableMarkers = cold
		d.Transact(func(tr *Transaction) {
			for _, pos := range order {
				txt.Insert(tr, pos, "x", nil)
			}
		})
		return txt.ToString()
	}
	seq := []int{0, 1, 2, 3, 4, 5, 6, 7, 8, 9}
	rev := []int{0, 0, 0, 0, 0, 0, 0, 0, 0, 0}
	rnd := []int{0, 1, 0, 2, 1, 3, 0, 4, 2, 1}
	for _, order := range [][]int{seq, rev, rnd} {
		if got, want := build(false, order), build(true, order); got != want {
			t.Fatalf("markers=%q cold=%q order=%v", got, want, order)
		}
	}
}

// TestSearchMarker_InsertMatchesCold_LargeRandom is a heavier version of the
// above: many random inserts and deletes through the marker write path must
// still yield exactly the force-cold document. This exercises marker refresh,
// LRU eviction, and the shim-driven invalidation across a long transaction.
func TestSearchMarker_InsertMatchesCold_LargeRandom(t *testing.T) {
	build := func(cold bool) string {
		d := New(WithClientID(1))
		txt := d.GetText("t")
		txt.baseType().disableMarkers = cold
		rng := rand.New(rand.NewSource(7))
		d.Transact(func(tr *Transaction) {
			for i := 0; i < 3000; i++ {
				pos := rng.Intn(txt.Len() + 1)
				txt.Insert(tr, pos, fmt.Sprintf("%d", rng.Intn(10)), nil)
				if txt.Len() > 30 && rng.Intn(6) == 0 {
					txt.Delete(tr, rng.Intn(txt.Len()-5), 1+rng.Intn(4))
				}
			}
		})
		return txt.ToString()
	}
	if got, want := build(false), build(true); got != want {
		t.Fatalf("marker/cold divergence:\n marker len=%d\n cold   len=%d", len(got), len(want))
	}
}

// TestSearchMarker_RemoteDeleteOnly_NoStale is the v1.31.6 stale-cache class
// guard (Task 3, #181). A remote update that ONLY tombstones an item is applied
// via ApplyUpdateV1 (which runs with txn.Local hardcoded true, so item.delete's
// own !txn.Local invalidation is dead), then a positioned LOCAL insert follows.
// If the remote delete-apply path fails to invalidate the search markers, the
// still-live markers after the tombstone carry indices that are now one-too-high
// and the insert resolves the wrong neighbour. The marker-run result must equal
// the force-cold run.
func TestSearchMarker_RemoteDeleteOnly_NoStale(t *testing.T) {
	// The v1.31.6 stale-cache class: a remote update that ONLY tombstones an
	// item applies with txn.Local hardcoded true (so item.delete's own
	// !txn.Local invalidation is dead) and must still invalidate the search
	// markers. A warmed cache holding markers for live items AFTER the deleted
	// one would otherwise carry indices that are now too high, and the next
	// positioned insert resolves the wrong neighbour.
	//
	// The cache is hand-seeded (as the read-only oracle tests do) because the
	// write path snaps markers to run-starts, which would hide the exact tail
	// marker this class needs; seeding models precisely the warmed state that
	// v1.31.6 corrupted.
	a := New(WithClientID(1))
	arrA := a.GetArray("arr")
	a.Transact(func(tr *Transaction) {
		for _, v := range []any{"A", "B", "C", "D", "E"} { // 5 distinct (unmerged) items
			arrA.Push(tr, []any{v})
		}
	})
	at := arrA.baseType()

	// Seed a correct, pre-delete warmed cache: markers for the live tail items.
	seed := func() {
		at.markers = at.markers[:0]
		for _, p := range []int{4, 5} { // -> (D,3), (E,4)
			it, idx := coldLeftNeighbour(at, p)
			if it != nil {
				at.markers = append(at.markers, searchMarker{item: it, index: idx})
			}
		}
	}
	seed()
	// Sanity: with the correct warmed cache, findMarkerRO agrees with cold.
	for i := 0; i <= arrA.Len(); i++ {
		if gi, gx := at.findMarkerRO(i); func() bool { ci, cx := coldLeftNeighbour(at, i); return gi != ci || gx != cx }() {
			t.Fatalf("pre-delete seed already stale at %d: got (%p,%d)", i, gi, gx)
		}
	}

	// A remote peer tombstones "B" and ships ONLY that delete back.
	b := New(WithClientID(2))
	if err := ApplyUpdateV1(b, EncodeStateAsUpdateV1(a, nil), nil); err != nil {
		t.Fatal(err)
	}
	arrB := b.GetArray("arr")
	seed() // re-warm right before the delete apply (encoding above may have touched markers)
	b.Transact(func(tr *Transaction) { arrB.Delete(tr, 1, 1) })
	upd := EncodeStateAsUpdateV1(b, a.StateVector())
	if err := ApplyUpdateV1(a, upd, nil); err != nil { // a live: [A C D E]; D,E shift left by 1
		t.Fatal(err)
	}

	// The invariant the remote-delete apply must uphold: every surviving marker
	// still resolves like the cold oracle. A stale (D,3)/(E,4) that survived the
	// tombstone would make findMarkerRO return the wrong item near the tail.
	for i := 0; i <= arrA.Len(); i++ {
		gi, gx := at.findMarkerRO(i)
		ci, cx := coldLeftNeighbour(at, i)
		if gi != ci || gx != cx {
			t.Fatalf("stale marker after remote delete-only apply at index %d: findMarkerRO=(%p,%d) cold=(%p,%d)", i, gi, gx, ci, cx)
		}
	}

	// And the end-to-end shape: a positioned insert after the remote delete must
	// land where the cold walk would put it.
	a.Transact(func(tr *Transaction) { arrA.Insert(tr, 4, []any{"Y"}) }) // append at end -> [A C D E Y]
	j, err := arrA.ToJSON()
	if err != nil {
		t.Fatal(err)
	}
	if got, want := string(j), `["A","C","D","E","Y"]`; got != want {
		t.Fatalf("positioned insert after remote delete-only: got %s want %s", got, want)
	}
}

// TestUpdateMarkerChanges_ShiftSemantics directly exercises the index-shift
// primitive that Task 3 wires into the structural mutation sites. The subtle
// invariant it must uphold: after the call, every surviving marker's recorded
// index still equals its item's true rendered start. The exact-boundary case
// (a marker whose index equals the edit position) is the one that made the
// naive strict-less-than condition wrong for our re-walk-trusting findMarkerRO
// (see the "index <= m.index" reasoning in search_marker.go).
func TestUpdateMarkerChanges_ShiftSemantics(t *testing.T) {
	live := &Item{Content: NewContentString("x")}         // countable, not deleted
	dead := &Item{Content: NewContentString("y"), Deleted: true}

	t.Run("insert shifts markers at or after the edit index", func(t *testing.T) {
		at := &abstractType{markers: []searchMarker{
			{item: live, index: 2}, // strictly before edit: must NOT move
			{item: live, index: 5}, // exactly at edit: MUST move (boundary case)
			{item: live, index: 9}, // after edit: must move
		}}
		at.updateMarkerChanges(5, +3) // insert 3 units at index 5
		want := []int{2, 8, 12}
		for i, w := range want {
			if at.markers[i].index != w {
				t.Fatalf("insert marker[%d].index=%d want %d", i, at.markers[i].index, w)
			}
		}
	})

	t.Run("delete shifts and clamps markers at or after the edit index", func(t *testing.T) {
		at := &abstractType{markers: []searchMarker{
			{item: live, index: 2},  // before delete: unchanged
			{item: live, index: 10}, // after delete: shift left by 3
		}}
		at.updateMarkerChanges(4, -3) // delete 3 units at index 4
		want := []int{2, 7}
		for i, w := range want {
			if at.markers[i].index != w {
				t.Fatalf("delete marker[%d].index=%d want %d", i, at.markers[i].index, w)
			}
		}
	})

	t.Run("markers on deleted items are dropped", func(t *testing.T) {
		at := &abstractType{markers: []searchMarker{
			{item: live, index: 1},
			{item: dead, index: 4}, // item became a tombstone: drop
			{item: live, index: 8},
		}}
		at.updateMarkerChanges(4, -2)
		if len(at.markers) != 2 {
			t.Fatalf("expected deleted-item marker dropped, got %d markers", len(at.markers))
		}
		for _, m := range at.markers {
			if m.item == dead {
				t.Fatalf("deleted-item marker survived")
			}
		}
	})
}

// TestSearchMarker_RO_IndexBeyondLen (carried over from Task 1 review): a
// read-only lookup for index > Len() must match the cold oracle (both walk
// off the end and return (nil, totalCounted)).
func TestSearchMarker_RO_IndexBeyondLen(t *testing.T) {
	d := New(WithClientID(1))
	txt := d.GetText("t")
	d.Transact(func(tr *Transaction) { txt.Insert(tr, 0, "abcde", nil) })
	at := txt.baseType()
	for _, idx := range []int{txt.Len() + 1, txt.Len() + 5, txt.Len() + 100} {
		gi, gidx := at.findMarkerRO(idx)
		ci, cidx := coldLeftNeighbour(at, idx)
		if gi != ci || gidx != cidx {
			t.Fatalf("index %d: findMarkerRO=(%p,%d) cold=(%p,%d)", idx, gi, gidx, ci, cidx)
		}
	}
}

// TestSearchMarker_RO_DisableMarkersIgnoresBadMarkers (carried over from Task
// 1 review): with disableMarkers set, findMarkerRO must ignore t.markers
// entirely and fall back to the cold walk — even when t.markers is populated
// with deliberately wrong (item, index) pairs.
func TestSearchMarker_RO_DisableMarkersIgnoresBadMarkers(t *testing.T) {
	d := New(WithClientID(1))
	txt := d.GetText("t")
	d.Transact(func(tr *Transaction) { txt.Insert(tr, 0, "hello world foo bar", nil) })
	at := txt.baseType()

	// Populate markers with garbage: point every marker at the first item but
	// claim wildly wrong indices. If findMarkerRO consulted these it would
	// return wrong answers.
	at.markers = []searchMarker{
		{item: at.start, index: 9999},
		{item: at.start, index: -50},
		{item: at.start, index: 3},
	}
	at.disableMarkers = true

	for i := 0; i <= txt.Len()+2; i++ {
		gi, gidx := at.findMarkerRO(i)
		ci, cidx := coldLeftNeighbour(at, i)
		if gi != ci || gidx != cidx {
			t.Fatalf("index %d: findMarkerRO=(%p,%d) cold=(%p,%d)", i, gi, gidx, ci, cidx)
		}
	}
}
