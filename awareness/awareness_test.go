package awareness_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/reearth/ygo/awareness"
	"github.com/reearth/ygo/encoding"
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

// ---------------------------------------------------------------------------
// Fix 1 — H4 + D2: EncodeUpdate(nil) must include removed clients
// ---------------------------------------------------------------------------

func TestUnit_EncodeUpdate_Nil_IncludesRemovedClient(t *testing.T) {
	a := awareness.New(1)
	a.SetLocalState(map[string]any{"x": 1})

	// Peer b learns about client 1 while it was active.
	b := awareness.New(2)
	require.NoError(t, b.ApplyUpdate(a.EncodeUpdate(nil), nil))
	require.Contains(t, b.GetStates(), uint64(1), "b must know client 1 before removal")

	// Now a removes itself.
	a.SetLocalState(nil)

	// EncodeUpdate(nil) must include client 1 even though its state is now nil.
	enc := a.EncodeUpdate(nil)
	require.NotEmpty(t, enc)

	// Use an observer on b to capture the removal event.
	var removedIDs []uint64
	b.OnChange(func(evt awareness.ChangeEvent) {
		removedIDs = append(removedIDs, evt.Removed...)
	})

	err := b.ApplyUpdate(enc, nil)
	require.NoError(t, err)

	// b must have been notified that client 1 was removed.
	assert.Contains(t, removedIDs, uint64(1), "b must receive removal notification for client 1")
	// Client 1 must no longer appear in active states on b.
	assert.NotContains(t, b.GetStates(), uint64(1), "client 1 must not appear in active states after removal")
}

// ---------------------------------------------------------------------------
// Fix 2 — M3: checkJSONDepth must reject unterminated strings
// ---------------------------------------------------------------------------

func TestUnit_JSONDepth_UnterminatedString_Rejected(t *testing.T) {
	// Unterminated strings should cause ApplyUpdate to treat the state as null.
	// We verify indirectly: craft a raw update whose JSON state is unterminated,
	// then confirm the client does not appear in GetStates().
	buildUpdate := func(jsonState string) []byte {
		enc := encoding.NewEncoder()
		enc.WriteVarUint(1)  // numClients
		enc.WriteVarUint(99) // clientID
		enc.WriteVarUint(1)  // clock
		enc.WriteVarString(jsonState)
		return enc.Bytes()
	}

	a := awareness.New(1)

	// Unterminated string: {"key": "unterminated
	err := a.ApplyUpdate(buildUpdate(`{"key": "unterminated`), nil)
	require.NoError(t, err)
	assert.NotContains(t, a.GetStates(), uint64(99), "unterminated string state must be treated as null")

	// Unterminated bare string: "no closing quote
	// Reset by applying a higher clock with a valid null state first so client
	// 99 is unknown again from clock perspective — use a fresh instance.
	a2 := awareness.New(1)
	err = a2.ApplyUpdate(buildUpdate(`"no closing quote`), nil)
	require.NoError(t, err)
	assert.NotContains(t, a2.GetStates(), uint64(99), "bare unterminated string must be treated as null")

	// Valid JSON must still be accepted.
	a3 := awareness.New(1)
	err = a3.ApplyUpdate(buildUpdate(`{"key": "value"}`), nil)
	require.NoError(t, err)
	assert.Contains(t, a3.GetStates(), uint64(99), "valid JSON must be accepted")

	// Brackets inside a string value must not be miscounted.
	a4 := awareness.New(1)
	err = a4.ApplyUpdate(buildUpdate(`{"key": "with [brackets] inside"}`), nil)
	require.NoError(t, err)
	assert.Contains(t, a4.GetStates(), uint64(99), "brackets inside string must not cause false rejection")
}

// ---------------------------------------------------------------------------
// Fix 3 — M4: Destroy() stops the auto-expiry goroutine
// ---------------------------------------------------------------------------

func TestUnit_Awareness_Destroy_StopsExpiry(t *testing.T) {
	a := awareness.New(1)
	a.SetLocalState(map[string]any{"x": 1})
	stop := a.StartAutoExpiry(50 * time.Millisecond)
	_ = stop // intentionally not calling stop; Destroy() should clean up

	// Destroy should stop the goroutine without panic or hang.
	done := make(chan struct{})
	go func() {
		a.Destroy()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Destroy() hung — goroutine not stopped")
	}

	// Calling Destroy() again must be a no-op.
	a.Destroy()
}

// ---------------------------------------------------------------------------
// Fix 4 — T3: ErrTooManyClients and ErrStateTooLarge
// ---------------------------------------------------------------------------

func TestUnit_Awareness_ApplyUpdate_TooManyClients_Errors(t *testing.T) {
	// Build an encoded update claiming maxAwarenessClients+1 clients (100_001).
	// The check fires on the count field alone, before reading client data.
	enc := encoding.NewEncoder()
	enc.WriteVarUint(uint64(100_000) + 1) // numClients field exceeds the limit

	a := awareness.New(1)
	err := a.ApplyUpdate(enc.Bytes(), nil)
	require.Error(t, err)
	assert.ErrorIs(t, err, awareness.ErrTooManyClients)
}

func TestUnit_Awareness_ApplyUpdate_StateTooLarge_Errors(t *testing.T) {
	// Build an update with 1 client whose state JSON exceeds maxAwarenessStateBytes (1 MiB).
	const maxAwarenessStateBytes = 1 << 20 // mirrors the constant in awareness.go
	huge := make([]byte, maxAwarenessStateBytes+1)
	for i := range huge {
		huge[i] = 'x'
	}
	enc := encoding.NewEncoder()
	enc.WriteVarUint(1)              // numClients
	enc.WriteVarUint(999)            // clientID
	enc.WriteVarUint(1)              // clock
	enc.WriteVarString(string(huge)) // state (oversized)

	a := awareness.New(1)
	err := a.ApplyUpdate(enc.Bytes(), nil)
	require.Error(t, err)
	assert.ErrorIs(t, err, awareness.ErrStateTooLarge)
}
