package awareness_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/reearth/ygo/awareness"
)

// ---------------------------------------------------------------------------
// Unit tests
// ---------------------------------------------------------------------------

func TestUnit_SetLocalState_IncreasesClock(t *testing.T) {
	a := awareness.New(1)
	a.SetLocalState(map[string]any{"x": 1})
	states := a.GetStates()
	assert.Equal(t, uint64(1), states[1].Clock, "clock should be 1 after first set")

	a.SetLocalState(map[string]any{"x": 2})
	states = a.GetStates()
	assert.Equal(t, uint64(2), states[1].Clock, "clock should be 2 after second set")
}

func TestUnit_GetLocalState_ReturnsSetState(t *testing.T) {
	a := awareness.New(42)
	want := map[string]any{"name": "alice", "cursor": float64(10)}
	a.SetLocalState(want)
	got := a.GetLocalState()
	assert.Equal(t, want, got)
}

func TestUnit_SetLocalState_Nil_RemovesClient(t *testing.T) {
	a := awareness.New(7)
	a.SetLocalState(map[string]any{"presence": true})
	require.Contains(t, a.GetStates(), uint64(7))

	a.SetLocalState(nil)
	assert.NotContains(t, a.GetStates(), uint64(7), "client should be removed after nil state")
	assert.Nil(t, a.GetLocalState())
}

func TestUnit_EncodeUpdate_SingleClient(t *testing.T) {
	a := awareness.New(1)
	a.SetLocalState(map[string]any{"hello": "world"})
	b := a.EncodeUpdate([]uint64{1})
	assert.NotEmpty(t, b, "encoded bytes should not be empty")
}

func TestUnit_EncodeUpdate_NilClientIDs_EncodesAll(t *testing.T) {
	// Peer A has its own state.
	a := awareness.New(10)
	a.SetLocalState(map[string]any{"a": 1})

	// Simulate a second peer by having A apply an update from peer 20.
	b := awareness.New(20)
	b.SetLocalState(map[string]any{"b": 2})
	update := b.EncodeUpdate(nil)
	require.NoError(t, a.ApplyUpdate(update, nil))

	// Now a.GetStates() should have both clients 10 and 20.
	states := a.GetStates()
	require.Len(t, states, 2, "should have two clients")

	// Encode all clients from A.
	encoded := a.EncodeUpdate(nil)

	// Apply to a fresh peer and verify it receives both clients.
	c := awareness.New(99)
	require.NoError(t, c.ApplyUpdate(encoded, nil))
	gotStates := c.GetStates()
	assert.Contains(t, gotStates, uint64(10))
	assert.Contains(t, gotStates, uint64(20))
}

func TestUnit_ApplyUpdate_IgnoresOlderClock(t *testing.T) {
	a := awareness.New(1) // local peer, different ID

	// Build an update with clock=5 for client 99.
	b := awareness.New(99)
	b.SetLocalState(map[string]any{"v": 1}) // clock 1
	// Manually craft higher-clock update by applying multiple times.
	for i := 0; i < 4; i++ {
		b.SetLocalState(map[string]any{"v": i + 2})
	}
	// b's clock is now 5.
	update5 := b.EncodeUpdate(nil)
	require.NoError(t, a.ApplyUpdate(update5, nil))
	assert.Equal(t, uint64(5), a.GetStates()[99].Clock)

	// Now build an update with clock=3 for client 99 from a different source.
	// We craft it directly by creating a fresh peer at clock 3.
	b2 := awareness.New(99)
	b2.SetLocalState(map[string]any{"v": 10}) // clock 1
	b2.SetLocalState(map[string]any{"v": 11}) // clock 2
	b2.SetLocalState(map[string]any{"v": 12}) // clock 3
	update3 := b2.EncodeUpdate(nil)

	require.NoError(t, a.ApplyUpdate(update3, nil))
	// Clock must still be 5 (the older clock=3 must be ignored).
	assert.Equal(t, uint64(5), a.GetStates()[99].Clock)
}

// ---------------------------------------------------------------------------
// Integration tests
// ---------------------------------------------------------------------------

func TestInteg_TwoPeer_StateExchange(t *testing.T) {
	peerA := awareness.New(1)
	peerB := awareness.New(2)

	state := map[string]any{"user": "alice", "color": "#f00"}
	peerA.SetLocalState(state)

	update := peerA.EncodeUpdate(nil)
	require.NoError(t, peerB.ApplyUpdate(update, nil))

	gotStates := peerB.GetStates()
	require.Contains(t, gotStates, uint64(1))
	assert.Equal(t, state, gotStates[1].State)
}

func TestInteg_RemoveExpired_FiresObserver(t *testing.T) {
	a := awareness.New(5)
	a.SetLocalState(map[string]any{"x": 1})

	var fired bool
	var removedIDs []uint64
	a.OnChange(func(evt awareness.ChangeEvent) {
		if len(evt.Removed) > 0 {
			fired = true
			removedIDs = append(removedIDs, evt.Removed...)
		}
	})

	// timeout=0 means everything is expired immediately.
	a.RemoveExpired(0)

	assert.True(t, fired, "observer should have been called")
	assert.Contains(t, removedIDs, uint64(5))
	assert.NotContains(t, a.GetStates(), uint64(5))
}

func TestInteg_OnChange_CalledOnApply(t *testing.T) {
	peerA := awareness.New(1)
	peerB := awareness.New(2)

	var observedAdded []uint64
	peerB.OnChange(func(evt awareness.ChangeEvent) {
		observedAdded = append(observedAdded, evt.Added...)
	})

	peerA.SetLocalState(map[string]any{"cursor": 42})
	update := peerA.EncodeUpdate(nil)
	require.NoError(t, peerB.ApplyUpdate(update, "remote"))

	assert.Contains(t, observedAdded, uint64(1), "observer should report client 1 as added")
}

func TestInteg_RoundTrip_NilState(t *testing.T) {
	peerA := awareness.New(1)
	peerB := awareness.New(2)

	// First, A sets a real state so B knows about A.
	peerA.SetLocalState(map[string]any{"x": 1})
	require.NoError(t, peerB.ApplyUpdate(peerA.EncodeUpdate(nil), nil))
	require.Contains(t, peerB.GetStates(), uint64(1))

	// Now A removes itself (nil state).
	peerA.SetLocalState(nil)
	update := peerA.EncodeUpdate([]uint64{peerA.ClientID()})
	// The local state is nil so encode should produce "null" for client 1.
	// But since client 1 is no longer in a's states, we need to check:
	// EncodeUpdate for a missing ID encodes clock=0, json="null".
	require.NoError(t, peerB.ApplyUpdate(update, nil))

	// B should no longer have client 1.
	assert.NotContains(t, peerB.GetStates(), uint64(1))
}

// ── checkJSONDepth string-context tests (N-C3) ─────────────────────────────

func TestUnit_Awareness_JSONDepth_BracketsInsideString(t *testing.T) {
	// A JSON object with brackets inside a string value must NOT be rejected.
	// Before the N-C3 fix, {"key": "[[[["}  was counted as depth 5.
	a := awareness.New(1)
	peerB := awareness.New(2)

	// Build an update where the state contains brackets in a string value.
	a.SetLocalState(map[string]any{"cursor": "[[[[in a string]]]]"})
	update := a.EncodeUpdate(nil)

	err := peerB.ApplyUpdate(update, nil)
	require.NoError(t, err)

	states := peerB.GetStates()
	require.Contains(t, states, uint64(1))
	assert.Equal(t, "[[[[in a string]]]]", states[1].State["cursor"])
}

func TestUnit_Awareness_JSONDepth_ActuallyDeepPayload(t *testing.T) {
	// A genuinely deeply-nested JSON payload must still be rejected.
	a := awareness.New(1)

	// Build a 25-deep nested array string directly and apply it as raw bytes.
	deep := ""
	for i := 0; i < 25; i++ {
		deep += "{"
	}
	for i := 0; i < 25; i++ {
		deep += "}"
	}
	// Manually encode an awareness update with this deep state JSON.
	enc := func() []byte {
		// 1 client, clientID=99, clock=1, jsonStr=deep
		b := []byte{}
		writeVarUint := func(v uint64) {
			for v >= 0x80 {
				b = append(b, byte(v)|0x80)
				v >>= 7
			}
			b = append(b, byte(v))
		}
		writeStr := func(s string) {
			writeVarUint(uint64(len(s)))
			b = append(b, s...)
		}
		writeVarUint(1)  // numClients
		writeVarUint(99) // clientID
		writeVarUint(1)  // clock
		writeStr(deep)   // state JSON
		return b
	}()
	err := a.ApplyUpdate(enc, nil)
	require.NoError(t, err)
	// The deep state should have been treated as null (removed).
	assert.NotContains(t, a.GetStates(), uint64(99))
}
