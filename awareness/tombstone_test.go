package awareness

import (
	"testing"
	"time"

	"github.com/reearth/ygo/encoding"
)

// encodeAt builds a single-client awareness frame at an explicit clock, mirroring
// EncodeUpdate's wire layout: varuint(count) then varuint(id) varuint(clock)
// varstring(json). Pass "null" for a removal frame. Lets a test drive clocks the
// encodeOneEntry helper (fixed clock 1/2) can't reach — e.g. reactivating a
// tombstone that sits at clock 2.
func encodeAt(id, clock uint64, stateJSON string) []byte {
	enc := encoding.NewEncoder()
	enc.WriteVarUint(1)
	enc.WriteVarUint(id)
	enc.WriteVarUint(clock)
	enc.WriteVarString(stateJSON)
	return enc.Bytes()
}

// makeTombstone creates a remote tombstone for id and backdates its removal by
// age, so PurgeTombstones sees it as older than a grace shorter than age.
func makeTombstone(t *testing.T, a *Awareness, id uint64, age time.Duration) {
	t.Helper()
	applyEntry(t, a, id, "null")
	if _, ok := a.removedAt[id]; !ok {
		t.Fatalf("tombstone for id %d did not record removedAt", id)
	}
	a.removedAt[id] = time.Now().Add(-age)
}

func TestPurgeTombstones_ReclaimsAgedTombstones(t *testing.T) {
	a := New(0)
	makeTombstone(t, a, 1, time.Hour)
	makeTombstone(t, a, 2, time.Hour)
	applyEntry(t, a, 3, "null") // fresh tombstone, removedAt == now
	a.removedAt[3] = time.Now() // ensure fresh

	purged := a.PurgeTombstones(time.Minute)

	if purged != 2 {
		t.Fatalf("purged = %d, want 2", purged)
	}
	if _, ok := a.states[1]; ok {
		t.Error("aged tombstone 1 should be purged")
	}
	if _, ok := a.states[2]; ok {
		t.Error("aged tombstone 2 should be purged")
	}
	if _, ok := a.states[3]; !ok {
		t.Error("fresh tombstone 3 should be retained")
	}
	if _, ok := a.removedAt[1]; ok {
		t.Error("removedAt entry for purged id 1 should be deleted")
	}
}

func TestPurgeTombstones_PreservesLiveAndLocalClients(t *testing.T) {
	a := New(0)
	a.SetLocalState(map[string]any{"me": true})
	a.SetLocalState(nil)                        // local becomes a tombstone (id 0) — must never be purged
	a.removedAt[0] = time.Now().Add(-time.Hour) // even if aged (it never gets one, but be adversarial)

	applyEntry(t, a, 1, "hi")         // live remote
	makeTombstone(t, a, 2, time.Hour) // aged remote tombstone

	purged := a.PurgeTombstones(time.Minute)

	if purged != 1 {
		t.Fatalf("purged = %d, want 1 (only remote tombstone 2)", purged)
	}
	if _, ok := a.states[0]; !ok {
		t.Error("local client (id 0) must never be purged")
	}
	if _, ok := a.states[1]; !ok {
		t.Error("live remote client 1 must be retained")
	}
	if _, ok := a.states[2]; ok {
		t.Error("aged remote tombstone 2 should be purged")
	}
}

func TestPurgeTombstones_FreesCapForNewClients(t *testing.T) {
	a := New(0)
	a.SetMaxClients(2)
	makeTombstone(t, a, 1, time.Hour)
	makeTombstone(t, a, 2, time.Hour)

	// Cap is full of tombstones — a previously-unseen client is refused.
	applyEntry(t, a, 3, "hi")
	if _, ok := a.states[3]; ok {
		t.Fatal("precondition: new client 3 should be refused while cap is full")
	}

	if purged := a.PurgeTombstones(time.Minute); purged != 2 {
		t.Fatalf("purged = %d, want 2", purged)
	}

	// Cap freed — the same client is now accepted.
	applyEntry(t, a, 3, "hi")
	cs, ok := a.states[3]
	if !ok || cs.State == nil {
		t.Error("after reclamation, new client 3 should be accepted with live state")
	}
}

func TestPurgeTombstones_ClearsRemovedAtOnReactivation(t *testing.T) {
	a := New(0)
	makeTombstone(t, a, 1, time.Hour) // tombstone at clock 2, aged

	// Reactivate at a higher clock than the tombstone (clock 2).
	if err := a.ApplyUpdate(encodeAt(1, 3, `{"v":"back"}`), nil); err != nil {
		t.Fatalf("reactivation ApplyUpdate: %v", err)
	}
	if _, ok := a.removedAt[1]; ok {
		t.Fatal("removedAt must be cleared when a tombstone reactivates")
	}

	// A now-live client must survive a purge even though it was once aged.
	if purged := a.PurgeTombstones(time.Minute); purged != 0 {
		t.Fatalf("purged = %d, want 0 (client is live again)", purged)
	}
	if cs, ok := a.states[1]; !ok || cs.State == nil {
		t.Error("reactivated client 1 must remain live")
	}
}

func TestPurgeTombstones_FiresNoObserverEvent(t *testing.T) {
	a := New(0)
	var events int
	a.OnChange(func(ChangeEvent) { events++ })

	applyEntry(t, a, 1, "hi")   // Added event
	applyEntry(t, a, 1, "null") // Removed event (was active)
	a.removedAt[1] = time.Now().Add(-time.Hour)
	before := events

	if purged := a.PurgeTombstones(time.Minute); purged != 1 {
		t.Fatalf("purged = %d, want 1", purged)
	}
	if events != before {
		t.Errorf("PurgeTombstones fired %d observer event(s); want 0", events-before)
	}
}

func TestPurgeTombstones_EncodeUpdateNilDropsPurged(t *testing.T) {
	a := New(0)
	makeTombstone(t, a, 1, time.Hour) // aged tombstone
	applyEntry(t, a, 2, "hi")         // live client

	a.PurgeTombstones(time.Minute)

	// A full-state broadcast must no longer carry the purged id.
	peer := New(99)
	if err := peer.ApplyUpdate(a.EncodeUpdate(nil), nil); err != nil {
		t.Fatalf("peer ApplyUpdate: %v", err)
	}
	if _, ok := peer.states[1]; ok {
		t.Error("EncodeUpdate(nil) should not emit the purged tombstone (id 1)")
	}
	if _, ok := peer.states[2]; !ok {
		t.Error("EncodeUpdate(nil) must still emit the live client (id 2)")
	}
}

func TestStartAutoExpiry_ReclaimsTombstones(t *testing.T) {
	a := New(0)
	applyEntry(t, a, 1, "hi") // live remote client; never heartbeats again

	stop := a.StartAutoExpiry(20 * time.Millisecond)
	defer stop()

	exists := func(id uint64) bool {
		a.mu.RLock()
		defer a.mu.RUnlock()
		_, ok := a.states[id]
		return ok
	}

	// The client expires into a tombstone (~20ms) and is then purged by the
	// second stage once its removal is older than 2*timeout (~40ms). The raw
	// states entry must eventually disappear entirely, not linger as a tombstone.
	deadline := time.Now().Add(2 * time.Second)
	for exists(1) {
		if time.Now().After(deadline) {
			t.Fatal("StartAutoExpiry did not reclaim the tombstone for id 1 within 2s")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestPurgeTombstones_NonPositiveGraceIsNoOp(t *testing.T) {
	a := New(0)
	makeTombstone(t, a, 1, time.Hour)

	for _, grace := range []time.Duration{0, -time.Second} {
		if purged := a.PurgeTombstones(grace); purged != 0 {
			t.Errorf("PurgeTombstones(%v) = %d, want 0 (no-op)", grace, purged)
		}
		if _, ok := a.states[1]; !ok {
			t.Fatalf("tombstone must survive PurgeTombstones(%v)", grace)
		}
	}
}
