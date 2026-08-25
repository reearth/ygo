package crdt

import (
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// gcPlaceholdersInStore counts the GC placeholders the decoder actually put in
// the store: parentless, deleted, ContentDeleted. Those are produced by exactly
// one thing, a GC struct on the wire, so the count is DERIVED FROM THE BYTES
// rather than read out of the fixture's own metadata.
func gcPlaceholdersInStore(d *Doc) int {
	n := 0
	for _, items := range d.store.clients {
		for _, it := range items {
			if it.Parent != nil || !it.Deleted {
				continue
			}
			if _, ok := it.Content.(*ContentDeleted); ok {
				n++
			}
		}
	}
	return n
}

// TestGCFixtures_GCStructsAreActuallyOnTheWire checks the property the GC
// conformance suite depends on, against the fixture BYTES.
//
// The metadata-only version of this check was circular: it asserted that the
// committed gcStructs integer was positive, which a hand edit stripping the GC
// struct out of the V2 payload would leave untouched. This applies the real
// decoder and looks for the placeholders a GC struct produces, so removing the
// struct from the wire fails the test no matter what the integer says.
func TestGCFixtures_GCStructsAreActuallyOnTheWire(t *testing.T) {
	type row struct {
		Name      string `json:"name"`
		GCStructs int    `json:"gcStructs"`
		V2        string `json:"v2"`
	}
	raw, err := os.ReadFile(filepath.Join("testdata", "gc_yjs_fixtures.json"))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	var rows []row
	if err := json.Unmarshal(raw, &rows); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(rows) == 0 {
		t.Fatal("gc_yjs_fixtures.json: no fixtures")
	}
	for _, r := range rows {
		t.Run(r.Name, func(t *testing.T) {
			b, err := hex.DecodeString(r.V2)
			if err != nil {
				t.Fatalf("bad hex: %v", err)
			}
			d := New()
			if err := ApplyUpdateV2(d, b, nil); err != nil {
				t.Fatalf("apply: %v", err)
			}
			onWire := gcPlaceholdersInStore(d)
			if onWire == 0 {
				t.Errorf("the V2 payload contains no GC struct; this fixture no longer covers the GC decode path (metadata claims %d)", r.GCStructs)
			}
			// Keep the committed metadata honest about the bytes. Not an
			// equality check: Append merges adjacent compatible structs, so the
			// store count can legitimately be lower than the wire count.
			if (r.GCStructs > 0) != (onWire > 0) {
				t.Errorf("metadata says gcStructs=%d but the wire yields %d GC placeholders", r.GCStructs, onWire)
			}
		})
	}
}

// TestTryIntegrate_GCPlaceholderPartialOverlapIsTrimmed covers the retry path
// for a GC placeholder that was parked for a same-client clock gap and whose
// range has since become partially integrated.
//
// tryIntegrate used to append such an item untrimmed, leaving two structs
// covering the same clocks in a per-client list that Append and the state
// vector both assume is contiguous and ordered. The decode loops always
// trimmed; only the retry did not.
func TestTryIntegrate_GCPlaceholderPartialOverlapIsTrimmed(t *testing.T) {
	d := New()
	m := d.GetMap("m")
	d.Transact(func(txn *Transaction) {
		m.Set(txn, "a", 1)
		m.Set(txn, "b", 2)
		m.Set(txn, "c", 3)
	})
	client := d.ClientID()
	end := d.store.NextClock(client)
	if end < 2 {
		t.Fatalf("setup: expected at least 2 clocks for the local client, got %d", end)
	}

	// Starts one clock inside the integrated range and extends past it, which is
	// exactly the partial overlap the retry has to trim.
	item := &Item{
		ID:      ID{Client: client, Clock: end - 1},
		Content: NewContentDeleted(5),
		Deleted: true,
	}
	d.Transact(func(txn *Transaction) {
		if !tryIntegrate(txn, item) {
			t.Fatal("tryIntegrate refused a GC placeholder whose predecessor is present")
		}
	})

	// The per-client list must stay contiguous and non-overlapping.
	prevEnd := uint64(0)
	for i, it := range d.store.clients[client] {
		if it.ID.Clock < prevEnd {
			t.Fatalf("struct %d starts at clock %d, overlapping the previous struct which ends at %d",
				i, it.ID.Clock, prevEnd)
		}
		prevEnd = it.ID.Clock + uint64(it.Content.Len())
	}
	if got := d.store.NextClock(client); got != end+4 {
		t.Errorf("NextClock = %d, want %d (the placeholder's 4 un-integrated clocks appended)", got, end+4)
	}
}
