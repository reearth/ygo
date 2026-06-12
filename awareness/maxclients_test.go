package awareness

import "testing"

// encodeOneEntry builds a single-client awareness update frame for clientID with
// the given JSON state. For a live state the frame is at clock 1; for the "null"
// removal case it sets then clears local state, so the frame is at clock 2.
// Mirrors the wire format ApplyUpdate expects.
func encodeOneEntry(t *testing.T, clientID uint64, stateJSON string) []byte {
	t.Helper()
	a := New(clientID)
	if stateJSON == "null" {
		// Represent a removal: set then clear, so the encoded entry is a null
		// (removed) state carrying clock 2.
		a.SetLocalState(map[string]any{"x": 1})
		a.SetLocalState(nil)
	} else {
		a.SetLocalState(map[string]any{"v": stateJSON})
	}
	return a.EncodeUpdate([]uint64{clientID})
}

// applyEntry applies a single-client frame and fails the test on error — these
// helper frames are always valid, so an error means a real regression.
func applyEntry(t *testing.T, a *Awareness, clientID uint64, stateJSON string) {
	t.Helper()
	if err := a.ApplyUpdate(encodeOneEntry(t, clientID, stateJSON), nil); err != nil {
		t.Fatalf("ApplyUpdate(client %d, %q): %v", clientID, stateJSON, err)
	}
}

func TestApplyUpdate_MaxClients_CapsDistinctEntries(t *testing.T) {
	a := New(0)
	a.SetMaxClients(3) // caps REMOTE entries; the local client (id 0) is exempt

	// Invent 10 distinct remote client IDs with live state; only the cap is kept.
	for id := uint64(1); id <= 10; id++ {
		applyEntry(t, a, id, "hi")
	}
	if got := len(a.states); got > 3 {
		t.Fatalf("distinct entries = %d, want <= 3 (cap)", got)
	}

	// An already-tracked client can still update past the cap (not a new ID).
	for id := range a.states {
		applyEntry(t, a, id, "again")
		break
	}
}

func TestApplyUpdate_MaxClients_NullEntriesAlsoCapped(t *testing.T) {
	a := New(0)
	a.SetMaxClients(2)
	// Null-state invented IDs are the DoS vector — they bypass the byte cap and
	// are stored as tombstones, so assert on the internal states map (tombstones
	// included), not GetStates (which returns only active clients).
	for id := uint64(1); id <= 50; id++ {
		applyEntry(t, a, id, "null")
	}
	if got := len(a.states); got > 2 {
		t.Fatalf("null-entry distinct count = %d, want <= 2 (cap)", got)
	}
}

func TestApplyUpdate_MaxClients_ZeroIsUnlimited(t *testing.T) {
	a := New(0) // no SetMaxClients → unlimited (backward compatible)
	for id := uint64(1); id <= 20; id++ {
		applyEntry(t, a, id, "hi")
	}
	if got := len(a.states); got != 20 {
		t.Fatalf("unlimited: distinct entries = %d, want 20", got)
	}
}

func TestApplyUpdate_MaxClients_LocalClientExempt(t *testing.T) {
	// The local client must not consume a remote cap slot. Set local state FIRST
	// so states = {7}, then apply a new remote client with cap=1: it must be
	// accepted, because the cap counts remote entries only. (Without excluding
	// the local entry, len(states)=1 >= 1 would wrongly reject the remote.)
	a := New(7)
	a.SetMaxClients(1)
	a.SetLocalState(map[string]any{"me": true}) // states = {7}; local is exempt
	applyEntry(t, a, 100, "hi")                 // new remote; must be accepted despite cap=1
	if _, ok := a.states[7]; !ok {
		t.Fatal("local client (7) missing")
	}
	if v, ok := a.GetStates()[100]; !ok || v.State == nil {
		t.Fatal("remote client 100 was rejected; the local client must not consume a cap slot")
	}
}
