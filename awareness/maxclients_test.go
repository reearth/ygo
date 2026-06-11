package awareness

import "testing"

// encodeOne builds a single-client awareness update frame for clientID with the
// given JSON state, at clock 1. Mirrors the wire format ApplyUpdate expects.
func encodeOneEntry(t *testing.T, clientID uint64, stateJSON string) []byte {
	t.Helper()
	a := New(clientID)
	if stateJSON == "null" {
		// represent a removal: set then clear so clock advances to a null entry
		a.SetLocalState(map[string]any{"x": 1})
		a.SetLocalState(nil)
	} else {
		a.SetLocalState(map[string]any{"v": stateJSON})
	}
	return a.EncodeUpdate([]uint64{clientID})
}

func TestApplyUpdate_MaxClients_CapsDistinctEntries(t *testing.T) {
	a := New(0)
	a.SetMaxClients(3) // cap incl. any local entry; local id 0 has no state here

	// Invent 10 distinct client IDs with live state. Only up to the cap are kept.
	for id := uint64(1); id <= 10; id++ {
		_ = a.ApplyUpdate(encodeOneEntry(t, id, "hi"), nil)
	}
	if got := len(a.GetStates()); got > 3 {
		t.Fatalf("distinct entries = %d, want <= 3 (cap)", got)
	}

	// Existing clients can still update past the cap (not a new ID).
	// Pick one accepted id and re-apply a newer clock; must not error/panic.
	for id := range a.GetStates() {
		_ = a.ApplyUpdate(encodeOneEntry(t, id, "again"), nil)
		break
	}
}

func TestApplyUpdate_MaxClients_NullEntriesAlsoCapped(t *testing.T) {
	a := New(0)
	a.SetMaxClients(2)
	// Null-state invented IDs are the DoS vector (they bypass the byte cap).
	for id := uint64(1); id <= 50; id++ {
		_ = a.ApplyUpdate(encodeOneEntry(t, id, "null"), nil)
	}
	if got := len(a.GetStates()); got > 2 {
		t.Fatalf("null-entry distinct count = %d, want <= 2 (cap)", got)
	}
}

func TestApplyUpdate_MaxClients_ZeroIsUnlimited(t *testing.T) {
	a := New(0) // no SetMaxClients → unlimited (backward compatible)
	for id := uint64(1); id <= 20; id++ {
		_ = a.ApplyUpdate(encodeOneEntry(t, id, "hi"), nil)
	}
	if got := len(a.GetStates()); got != 20 {
		t.Fatalf("unlimited: distinct entries = %d, want 20", got)
	}
}
