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
