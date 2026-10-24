package awareness

import "testing"

// applyForeignEntry applies a single-client frame through ApplyUpdateForeign
// and fails the test on error.
func applyForeignEntry(t *testing.T, a *Awareness, clientID uint64, stateJSON string) {
	t.Helper()
	if err := a.ApplyUpdateForeign(encodeOneEntry(t, clientID, stateJSON), nil); err != nil {
		t.Fatalf("ApplyUpdateForeign(client %d, %q): %v", clientID, stateJSON, err)
	}
}

func TestApplyUpdateForeign_SubCapRefusesNewIDs(t *testing.T) {
	a := New(0)
	a.SetMaxClients(10)
	a.SetMaxForeignClients(3)

	for id := uint64(1); id <= 10; id++ {
		applyForeignEntry(t, a, id, "hi")
	}
	if got := len(a.GetStates()); got != 3 {
		t.Fatalf("live entries = %d, want 3 (foreign sub-cap)", got)
	}
}

func TestApplyUpdateForeign_LocalApplyStillFillsFullCap(t *testing.T) {
	a := New(0)
	a.SetMaxClients(10)
	a.SetMaxForeignClients(3)

	for id := uint64(1); id <= 10; id++ {
		applyForeignEntry(t, a, id, "hi")
	}
	for id := uint64(11); id <= 20; id++ {
		applyEntry(t, a, id, "hi")
	}
	if got := len(a.GetStates()); got != 10 {
		t.Fatalf("live entries = %d, want 10 (SetMaxClients)", got)
	}
}

func TestApplyUpdateForeign_TrackedForeignClientKeepsUpdating(t *testing.T) {
	a := New(0)
	a.SetMaxForeignClients(2)

	applyForeignEntry(t, a, 1, "hi")
	applyForeignEntry(t, a, 2, "hi")
	applyForeignEntry(t, a, 3, "hi") // refused: sub-cap full

	// Higher clock for an already-admitted ID.
	b := New(1)
	b.SetLocalState(map[string]any{"v": "hi"})
	b.SetLocalState(map[string]any{"v": "again"})
	if err := a.ApplyUpdateForeign(b.EncodeUpdate([]uint64{1}), nil); err != nil {
		t.Fatalf("ApplyUpdateForeign: %v", err)
	}

	states := a.GetStates()
	if got := states[1].State["v"]; got != "again" {
		t.Fatalf("client 1 state = %v, want %q", got, "again")
	}
	if _, ok := states[3]; ok {
		t.Fatal("client 3 admitted past a full sub-cap")
	}
}

func TestApplyUpdateForeign_TombstonesDoNotSaturate(t *testing.T) {
	a := New(0)
	a.SetMaxForeignClients(3)

	// Churn: admit then remove, with no expiry sweep reclaiming tombstones.
	for id := uint64(1); id <= 10; id++ {
		applyForeignEntry(t, a, id, "hi")
		applyForeignEntry(t, a, id, "null")
	}
	if got := len(a.GetStates()); got != 0 {
		t.Fatalf("live entries = %d after churn, want 0", got)
	}

	applyForeignEntry(t, a, 100, "hi")
	if _, ok := a.GetStates()[100]; !ok {
		t.Fatal("new foreign client refused after tombstone churn")
	}
}

func TestApplyUpdateForeign_TombstoneRevivalRespectsSubCap(t *testing.T) {
	a := New(0)
	a.SetMaxForeignClients(3)

	// Accumulate tombstones in batches the live-only gate allows.
	for batch := uint64(0); batch < 3; batch++ {
		for id := batch*3 + 1; id <= batch*3+3; id++ {
			applyForeignEntry(t, a, id, "hi")
		}
		for id := batch*3 + 1; id <= batch*3+3; id++ {
			applyForeignEntry(t, a, id, "null")
		}
	}
	if got := len(a.GetStates()); got != 0 {
		t.Fatalf("live entries = %d after nulling, want 0", got)
	}

	// Revive more tombstoned IDs than the cap, each at a higher clock.
	for id := uint64(1); id <= 9; id++ {
		b := New(id)
		b.SetLocalState(map[string]any{"v": "a"})
		b.SetLocalState(nil)
		b.SetLocalState(map[string]any{"v": "back"})
		if err := a.ApplyUpdateForeign(b.EncodeUpdate([]uint64{id}), nil); err != nil {
			t.Fatalf("ApplyUpdateForeign(client %d): %v", id, err)
		}
	}
	if got := len(a.GetStates()); got != 3 {
		t.Fatalf("live entries = %d after revival, want 3 (foreign sub-cap)", got)
	}
}

func TestApplyUpdateForeign_ZeroDisablesSubCap(t *testing.T) {
	a := New(0)
	a.SetMaxClients(10)

	for id := uint64(1); id <= 10; id++ {
		applyForeignEntry(t, a, id, "hi")
	}
	if got := len(a.GetStates()); got != 10 {
		t.Fatalf("live entries = %d, want 10", got)
	}
}

func TestApplyUpdateForeign_LocalClientIDExempt(t *testing.T) {
	a := New(7)
	a.SetMaxForeignClients(1)
	a.SetLocalState(map[string]any{"v": "mine"})

	applyForeignEntry(t, a, 1, "hi") // fills the sub-cap

	// A foreign frame carrying our own ID at a higher clock is not refused.
	b := New(7)
	b.SetLocalState(map[string]any{"v": "a"})
	b.SetLocalState(map[string]any{"v": "b"})
	b.SetLocalState(map[string]any{"v": "c"})
	if err := a.ApplyUpdateForeign(b.EncodeUpdate([]uint64{7}), nil); err != nil {
		t.Fatalf("ApplyUpdateForeign: %v", err)
	}
	if got := a.GetStates()[7].State["v"]; got != "c" {
		t.Fatalf("local client state = %v, want %q", got, "c")
	}
}

func TestApplyUpdateForeign_LocalTombstoneExempt(t *testing.T) {
	a := New(7)
	a.SetMaxForeignClients(1)
	a.SetLocalState(map[string]any{"v": "mine"})
	a.SetLocalState(nil) // local tombstone, clock 2

	applyForeignEntry(t, a, 1, "hi") // fills the sub-cap

	// Foreign frame for our own ID at a higher clock must bypass the sub-cap.
	b := New(7)
	for _, v := range []string{"a", "b", "c", "d"} {
		b.SetLocalState(map[string]any{"v": v})
	}
	if err := a.ApplyUpdateForeign(b.EncodeUpdate([]uint64{7}), nil); err != nil {
		t.Fatalf("ApplyUpdateForeign: %v", err)
	}
	if got := a.GetStates()[7].State["v"]; got != "d" {
		t.Fatalf("local client state = %v, want %q", got, "d")
	}
}
