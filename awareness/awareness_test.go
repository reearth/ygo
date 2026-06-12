package awareness_test

import (
	"context"
	"runtime"
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
	// Local client (5) is never expired by RemoveExpired per #73 vector C4;
	// to exercise the observer, seed a remote client (99) and let it expire.
	a := awareness.New(5)
	a.SetLocalState(map[string]any{"x": 1})
	require.NoError(t, a.ApplyUpdate(buildAwarenessPayload(t, 99, 1, `{"y":2}`), nil))

	var fired bool
	var removedIDs []uint64
	a.OnChange(func(evt awareness.ChangeEvent) {
		if len(evt.Removed) > 0 {
			fired = true
			removedIDs = append(removedIDs, evt.Removed...)
		}
	})

	// timeout=0: remote 99 expires; local 5 is exempt (C4).
	a.RemoveExpired(0)

	assert.True(t, fired, "observer should have been called for remote eviction")
	assert.Contains(t, removedIDs, uint64(99), "remote client must be expired")
	assert.NotContains(t, removedIDs, uint64(5), "local client must NOT be expired")
	assert.NotContains(t, a.GetStates(), uint64(99), "remote gone")
	assert.Contains(t, a.GetStates(), uint64(5), "local stays")
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

func TestAwareness_StartAutoExpiry_TinyTimeoutDoesNotPanic(t *testing.T) {
	// A sub-2ns timeout makes timeout/2 round to 0; time.NewTicker(0) panics.
	// Since the timeout is caller- (and CLI-) configurable, StartAutoExpiry must
	// clamp the tick interval rather than crash.
	a := awareness.New(0)
	stop := a.StartAutoExpiry(1 * time.Nanosecond) // would panic without the clamp
	stop()
}

func TestAwareness_StartAutoExpiry_NoLeakOnDoubleCall(t *testing.T) {
	// Regression for #34: calling StartAutoExpiry twice must not leak
	// the first goroutine.
	gotBefore := runtime.NumGoroutine()

	a := awareness.New(0)
	stop1 := a.StartAutoExpiry(50 * time.Millisecond)
	stop2 := a.StartAutoExpiry(100 * time.Millisecond)

	// stop2 should kill G2; the fix ensures G1 was already stopped
	// internally by the second StartAutoExpiry call.
	stop2()
	_ = stop1 // legacy reference; should be a no-op double-close-safe

	// Allow time for any leftover goroutines to exit.
	time.Sleep(200 * time.Millisecond)

	gotAfter := runtime.NumGoroutine()
	const slack = 2
	assert.LessOrEqual(t, gotAfter-gotBefore, slack,
		"StartAutoExpiry leaked goroutine: %d before, %d after",
		gotBefore, gotAfter)
}

// ---------------------------------------------------------------------------
// Context-aware methods (#27)
// ---------------------------------------------------------------------------

func TestAwareness_SetLocalStateContext_PreCancelledReturnsCtxErr(t *testing.T) {
	a := awareness.New(0)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := a.SetLocalStateContext(ctx, map[string]any{"name": "x"})
	require.ErrorIs(t, err, context.Canceled)
	assert.Empty(t, a.GetLocalState(), "state must not be updated when ctx pre-cancelled")
}

func TestAwareness_SetLocalStateContext_OkReturnsNil(t *testing.T) {
	a := awareness.New(0)
	err := a.SetLocalStateContext(context.Background(), map[string]any{"name": "x"})
	require.NoError(t, err)
	assert.Equal(t, "x", a.GetLocalState()["name"])
}

func TestAwareness_ApplyUpdateContext_PreCancelledReturnsCtxErr(t *testing.T) {
	a := awareness.New(0)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := a.ApplyUpdateContext(ctx, []byte{0, 0}, nil)
	require.ErrorIs(t, err, context.Canceled)
}

// ---------------------------------------------------------------------------
// DoS caps for issue #48
// ---------------------------------------------------------------------------

// Vector A: a single state JSON object with thousands of keys passes the
// existing 1 MiB byte cap (a small int value per key fits comfortably in
// 1 MiB at ~10 bytes/key) but materialises into a huge map[string]any.
// The cap on key count must drop such states.
func TestUnit_Awareness_ApplyUpdate_StateKeyCountExceeded_DropsState(t *testing.T) {
	// Build a JSON object with maxStateKeys + 100 keys. Total byte size will
	// be well under the 1 MiB ErrStateTooLarge threshold for trivial values.
	var b []byte
	b = append(b, '{')
	const numKeys = 1100 // > maxStateKeys (1000)
	for i := 0; i < numKeys; i++ {
		if i > 0 {
			b = append(b, ',')
		}
		b = append(b, []byte(`"k`)...)
		b = append(b, []byte(itoa(i))...)
		b = append(b, []byte(`":1`)...)
	}
	b = append(b, '}')

	enc := encoding.NewEncoder()
	enc.WriteVarUint(1)           // numClients
	enc.WriteVarUint(999)         // clientID
	enc.WriteVarUint(1)           // clock
	enc.WriteVarString(string(b)) // state with too many keys

	a := awareness.New(1)
	require.NoError(t, a.ApplyUpdate(enc.Bytes(), nil),
		"oversized-key-count states are dropped silently, not errored")
	assert.NotContains(t, a.GetStates(), uint64(999),
		"client with > maxStateKeys keys must be dropped (treated as null)")
}

func TestUnit_Awareness_ApplyUpdate_StateAtKeyLimit_Accepted(t *testing.T) {
	// Sanity: a state at exactly the limit (1000 keys) is accepted.
	var b []byte
	b = append(b, '{')
	const numKeys = 1000 // == maxStateKeys
	for i := 0; i < numKeys; i++ {
		if i > 0 {
			b = append(b, ',')
		}
		b = append(b, []byte(`"k`)...)
		b = append(b, []byte(itoa(i))...)
		b = append(b, []byte(`":1`)...)
	}
	b = append(b, '}')

	enc := encoding.NewEncoder()
	enc.WriteVarUint(1)
	enc.WriteVarUint(7)
	enc.WriteVarUint(1)
	enc.WriteVarString(string(b))

	a := awareness.New(1)
	require.NoError(t, a.ApplyUpdate(enc.Bytes(), nil))
	assert.Contains(t, a.GetStates(), uint64(7),
		"state with exactly maxStateKeys keys must be accepted")
}

// Vector B: cumulative awareness state across all clients in one room must be
// capped. Configured via SetMaxBytes; entries that would push total past the
// cap are dropped.
func TestUnit_Awareness_ApplyUpdate_PerRoomByteCap_DropsExcess(t *testing.T) {
	a := awareness.New(1)
	const cap = 4 * 1024 // 4 KiB cap for this test
	a.SetMaxBytes(cap)

	// First client: ~2 KiB of legitimate state. Fits.
	payload1 := buildAwarenessPayload(t, 100, 1, makeRoughlySized("x", 2*1024))
	require.NoError(t, a.ApplyUpdate(payload1, nil))
	require.Contains(t, a.GetStates(), uint64(100), "first client must fit")

	// Second client: ~2 KiB more. Total ~4 KiB — still fits.
	payload2 := buildAwarenessPayload(t, 200, 1, makeRoughlySized("y", 2*1024))
	require.NoError(t, a.ApplyUpdate(payload2, nil))
	require.Contains(t, a.GetStates(), uint64(200), "second client must fit at cap boundary")

	// Third client: ~2 KiB more. Total would be ~6 KiB, exceeding 4 KiB cap.
	// Must be dropped.
	payload3 := buildAwarenessPayload(t, 300, 1, makeRoughlySized("z", 2*1024))
	require.NoError(t, a.ApplyUpdate(payload3, nil),
		"per-room byte cap should drop the entry silently, not error")
	assert.NotContains(t, a.GetStates(), uint64(300),
		"client that would exceed per-room byte cap must be dropped")
}

func TestUnit_Awareness_ApplyUpdate_PerRoomByteCap_RemovedClientFreesBytes(t *testing.T) {
	// When a client is removed (null state), its bytes must be released from
	// the room total so subsequent clients can fit.
	a := awareness.New(1)
	const cap = 4 * 1024
	a.SetMaxBytes(cap)

	require.NoError(t, a.ApplyUpdate(buildAwarenessPayload(t, 100, 1, makeRoughlySized("x", 3*1024)), nil))
	require.Contains(t, a.GetStates(), uint64(100))

	// Remove client 100 (null state with higher clock).
	require.NoError(t, a.ApplyUpdate(buildAwarenessPayload(t, 100, 2, "null"), nil))
	require.NotContains(t, a.GetStates(), uint64(100), "client 100 should be removed")

	// Now a fresh 3 KiB client should fit (the slot was reclaimed).
	require.NoError(t, a.ApplyUpdate(buildAwarenessPayload(t, 200, 1, makeRoughlySized("y", 3*1024)), nil))
	assert.Contains(t, a.GetStates(), uint64(200),
		"after removing 100, room has free bytes for 200")
}

func TestUnit_Awareness_ApplyUpdate_PerRoomByteCap_Unlimited_WhenZero(t *testing.T) {
	// SetMaxBytes(0) means unlimited — backward compatible default.
	a := awareness.New(1)
	a.SetMaxBytes(0)
	payload := buildAwarenessPayload(t, 100, 1, makeRoughlySized("x", 100*1024)) // 100 KiB
	require.NoError(t, a.ApplyUpdate(payload, nil))
	assert.Contains(t, a.GetStates(), uint64(100),
		"with cap=0 (unlimited), even large states must be accepted")
}

// itoa is a tiny base-10 integer-to-string helper to keep the test self-
// contained without pulling in strconv (which would balloon imports for one use).
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := false
	if n < 0 {
		neg = true
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}

// buildAwarenessPayload encodes a single-client awareness update.
func buildAwarenessPayload(t *testing.T, clientID uint64, clock uint64, jsonState string) []byte {
	t.Helper()
	enc := encoding.NewEncoder()
	enc.WriteVarUint(1) // numClients
	enc.WriteVarUint(clientID)
	enc.WriteVarUint(clock)
	enc.WriteVarString(jsonState)
	return enc.Bytes()
}

// makeRoughlySized returns a JSON object with one key and a string value
// sized so the total JSON length is approximately targetBytes.
func makeRoughlySized(key string, targetBytes int) string {
	// JSON overhead: {"key":"…"} = 8 + len(key) bytes (assumes no escaping in value).
	overhead := 8 + len(key)
	valLen := targetBytes - overhead
	if valLen < 0 {
		valLen = 0
	}
	val := make([]byte, valLen)
	for i := range val {
		val[i] = 'a'
	}
	return `{"` + key + `":"` + string(val) + `"}`
}

// ---------------------------------------------------------------------------
// #73: awareness protocol correctness (y-protocols parity)
// ---------------------------------------------------------------------------

// C1 (HIGH) — a remote null update targeting our own clientID must NOT wipe
// our local state. Yjs JS and yrs both detect this and bump the local clock
// so a re-emit will overrule the remote. ygo previously accepted the wipe.
func TestUnit_Awareness_C1_RemoteCannotWipeLocalState(t *testing.T) {
	a := awareness.New(1)
	a.SetLocalState(map[string]any{"name": "alice"})
	initialClock := a.GetStates()[1].Clock

	// Malicious / buggy update: clientID=1 (us), state=null, clock far ahead.
	require.NoError(t, a.ApplyUpdate(buildAwarenessPayload(t, 1, 999, "null"), nil))

	// Local state must remain; local clock must be > 999 so the re-emit wins.
	require.Contains(t, a.GetStates(), uint64(1), "local state must survive the remote wipe attempt")
	assert.Equal(t, "alice", a.GetStates()[1].State["name"])
	assert.Greater(t, a.GetStates()[1].Clock, uint64(999),
		"local clock must be bumped past the remote attempt so peers learn the new value")
	assert.Greater(t, a.GetStates()[1].Clock, initialClock,
		"local clock must monotonically advance")
}

// C2 (HIGH) — an equal-clock null entry for a currently-active client must
// be accepted as the canonical "client X went offline at the clock you
// already know" message. ygo previously dropped it because of the strict
// `<= current.Clock` check.
func TestUnit_Awareness_C2_EqualClockNullRemovesActiveClient(t *testing.T) {
	a := awareness.New(1)
	// Seed client 99 with an active state at clock 5.
	require.NoError(t, a.ApplyUpdate(buildAwarenessPayload(t, 99, 5, `{"x":1}`), nil))
	require.Contains(t, a.GetStates(), uint64(99))

	// Same clock 5, but now null — the offline signal.
	require.NoError(t, a.ApplyUpdate(buildAwarenessPayload(t, 99, 5, "null"), nil))
	assert.NotContains(t, a.GetStates(), uint64(99),
		"equal-clock null must remove an active client (offline signal)")
}

// C2 cont. — a strictly-less-than-current clock must still be dropped. The
// fix to accept equal-clock null mustn't accidentally also accept stale ones.
func TestUnit_Awareness_C2_OlderClockStillRejected(t *testing.T) {
	a := awareness.New(1)
	require.NoError(t, a.ApplyUpdate(buildAwarenessPayload(t, 99, 5, `{"x":1}`), nil))
	// Older null — must NOT remove.
	require.NoError(t, a.ApplyUpdate(buildAwarenessPayload(t, 99, 3, "null"), nil))
	assert.Contains(t, a.GetStates(), uint64(99),
		"strictly-older null must be dropped, not honored")
}

// C2 cont. — equal-clock non-null is still a no-op (no new information).
func TestUnit_Awareness_C2_EqualClockSameStateNoOp(t *testing.T) {
	a := awareness.New(1)
	require.NoError(t, a.ApplyUpdate(buildAwarenessPayload(t, 99, 5, `{"x":1}`), nil))
	// Same clock 5, non-null — no new info, must not double-fire observer.
	var fireCount int
	a.OnChange(func(awareness.ChangeEvent) { fireCount++ })
	require.NoError(t, a.ApplyUpdate(buildAwarenessPayload(t, 99, 5, `{"x":2}`), nil))
	assert.Equal(t, 0, fireCount,
		"equal-clock non-null update must be dropped (no observer fire)")
}

// C3 (MEDIUM) — local clock must follow any remote echo of our own
// clientID. If a peer reports our clientID at a higher clock (e.g. another
// browser tab is also us), SetLocalState must produce a clock above that,
// not a stale a.clock++.
func TestUnit_Awareness_C3_LocalClockFollowsRemoteEcho(t *testing.T) {
	a := awareness.New(1)
	// Remote echoes clientID=1 at clock 100 (e.g. another tab).
	require.NoError(t, a.ApplyUpdate(buildAwarenessPayload(t, 1, 100, `{"name":"alice"}`), nil))
	require.Equal(t, uint64(100), a.GetStates()[1].Clock)

	// Now SetLocalState — its clock must be > 100, not 1.
	a.SetLocalState(map[string]any{"name": "alice-v2"})
	assert.Greater(t, a.GetStates()[1].Clock, uint64(100),
		"SetLocalState after remote echo must produce a clock above the echo")
}

// C4 (MEDIUM) — RemoveExpired must skip the local client. Yjs JS and yrs
// both explicitly exclude self from the expiry sweep so the local peer
// can't self-remove in a quiet room.
func TestUnit_Awareness_C4_RemoveExpiredSkipsLocal(t *testing.T) {
	a := awareness.New(1)
	a.SetLocalState(map[string]any{"x": 1})

	// Aggressive expiry: 0 timeout = everything expires immediately.
	a.RemoveExpired(0)

	assert.Contains(t, a.GetStates(), uint64(1),
		"local client must never be expired by RemoveExpired")
}

// C4 cont. — remote clients must STILL be evicted normally.
func TestUnit_Awareness_C4_RemoveExpiredStillEvictsRemote(t *testing.T) {
	a := awareness.New(1)
	a.SetLocalState(map[string]any{"x": 1})
	require.NoError(t, a.ApplyUpdate(buildAwarenessPayload(t, 99, 5, `{"y":2}`), nil))

	a.RemoveExpired(0)

	assert.Contains(t, a.GetStates(), uint64(1), "local stays")
	assert.NotContains(t, a.GetStates(), uint64(99), "remote evicted")
}

// C5 (new API) — Heartbeat re-emits the local state with an incremented
// clock so peers learn that we're still alive even when our state hasn't
// changed. No-op when no local state is set.
func TestUnit_Awareness_C5_Heartbeat_BumpsClock(t *testing.T) {
	a := awareness.New(1)
	a.SetLocalState(map[string]any{"x": 1})
	before := a.GetStates()[1].Clock

	a.Heartbeat()
	after := a.GetStates()[1].Clock
	assert.Greater(t, after, before, "Heartbeat must bump the local clock")
	assert.Equal(t, map[string]any{"x": 1}, a.GetStates()[1].State,
		"Heartbeat must preserve local state")
}

func TestUnit_Awareness_C5_Heartbeat_NoOpWhenNoLocalState(t *testing.T) {
	a := awareness.New(1)
	// No SetLocalState before Heartbeat.
	assert.NotPanics(t, func() { a.Heartbeat() })
	assert.NotContains(t, a.GetStates(), uint64(1),
		"Heartbeat without local state must not create one")
}

// --- #73 edge-case coverage ---

// C1 cont.: the contract isn't just "keep local state" — it's also "advertise
// the new clock to peers so they learn the value." Verify that the next
// EncodeUpdate after a wipe attempt carries the bumped clock, by applying it
// to a fresh peer and checking what they see.
func TestUnit_Awareness_C1_RemoteWipe_FollowedByEncode_AdvertisesNewClock(t *testing.T) {
	a := awareness.New(1)
	a.SetLocalState(map[string]any{"name": "alice"})

	// Adversarial null update at a high clock.
	require.NoError(t, a.ApplyUpdate(buildAwarenessPayload(t, 1, 999, "null"), nil))
	bumpedClock := a.GetStates()[1].Clock
	require.Greater(t, bumpedClock, uint64(999))

	// Now encode and apply to a fresh peer. The peer must see our clientID
	// at the bumped clock, not at the pre-attack one — otherwise peers that
	// joined after the attack would never learn the new value.
	peer := awareness.New(2)
	require.NoError(t, peer.ApplyUpdate(a.EncodeUpdate([]uint64{1}), nil))
	require.Contains(t, peer.GetStates(), uint64(1))
	assert.Equal(t, bumpedClock, peer.GetStates()[1].Clock,
		"EncodeUpdate after wipe attempt must carry the bumped clock so peers learn it")
	assert.Equal(t, "alice", peer.GetStates()[1].State["name"])
}

// C1 cont.: a misbehaving peer might retry the null attack. Local state must
// survive repeated attempts, and the local clock must monotonically advance
// past each one so peers always see the latest value.
func TestUnit_Awareness_C1_RemoteWipe_RepeatAttempts_AlwaysSafe(t *testing.T) {
	a := awareness.New(1)
	a.SetLocalState(map[string]any{"name": "alice"})

	var lastClock uint64
	for i, clk := range []uint64{100, 200, 300} {
		require.NoError(t, a.ApplyUpdate(buildAwarenessPayload(t, 1, clk, "null"), nil))
		got := a.GetStates()[1].Clock
		assert.Greater(t, got, clk, "iter %d: clock must be bumped past %d", i, clk)
		assert.Greater(t, got, lastClock, "iter %d: clock must strictly advance", i)
		require.Contains(t, a.GetStates(), uint64(1), "iter %d: local must survive", i)
		assert.Equal(t, "alice", a.GetStates()[1].State["name"])
		lastClock = got
	}
}

// C2 cont.: equal-clock null when the client is ALREADY removed (state==nil
// at the current clock) — must be dropped. There's no active state to mark
// offline, so the entry conveys no new information.
func TestUnit_Awareness_C2_EqualClockNull_OnAlreadyRemovedClient_Dropped(t *testing.T) {
	a := awareness.New(1)
	// Set up: client 99 active at clock 5.
	require.NoError(t, a.ApplyUpdate(buildAwarenessPayload(t, 99, 5, `{"x":1}`), nil))
	// Remove at the same clock 5 (the C2-supported case).
	require.NoError(t, a.ApplyUpdate(buildAwarenessPayload(t, 99, 5, "null"), nil))
	require.NotContains(t, a.GetStates(), uint64(99))

	// Now another equal-clock null — already-removed client, no observer should fire.
	var fireCount int
	a.OnChange(func(awareness.ChangeEvent) { fireCount++ })
	require.NoError(t, a.ApplyUpdate(buildAwarenessPayload(t, 99, 5, "null"), nil))
	assert.Equal(t, 0, fireCount,
		"equal-clock null on already-removed client conveys no new info — observers must not fire")
}

// C3 cont.: a chain of remote echoes at increasing clocks must each correctly
// raise the local baseline. One echo working is one thing; verifying that the
// reconciliation is monotonic across a sequence catches bugs where the local
// clock latches to the first echo and ignores subsequent ones.
func TestUnit_Awareness_C3_MultipleRemoteEchoes_AllAdvanceLocalClock(t *testing.T) {
	a := awareness.New(1)
	for _, echoClock := range []uint64{50, 150, 300} {
		require.NoError(t, a.ApplyUpdate(buildAwarenessPayload(t, 1, echoClock, `{"v":1}`), nil))
		// Each SetLocalState must emit a clock above the just-applied echo.
		a.SetLocalState(map[string]any{"v": echoClock})
		assert.Greater(t, a.GetStates()[1].Clock, echoClock,
			"after echo at %d, SetLocalState must emit > %d", echoClock, echoClock)
	}
}

// C5 cont.: Heartbeat must not fire observers — the state itself didn't
// change, only the clock advanced. Firing would cause downstream UI churn
// on every heartbeat tick. Yjs JS has the same observer-silent contract.
func TestUnit_Awareness_C5_Heartbeat_DoesNotFireObservers(t *testing.T) {
	a := awareness.New(1)
	a.SetLocalState(map[string]any{"x": 1})

	var fireCount int
	a.OnChange(func(awareness.ChangeEvent) { fireCount++ })

	a.Heartbeat()
	a.Heartbeat()
	a.Heartbeat()

	assert.Equal(t, 0, fireCount,
		"Heartbeat must be observer-silent (state unchanged, clock advanced only)")
}

// C5 cont.: Heartbeat called after SetLocalState(nil) — local state was
// removed — must be a no-op. Heartbeating a removed client would resurrect
// it from peers' point of view, which is the opposite of intent.
func TestUnit_Awareness_C5_Heartbeat_AfterRemoval_NoOp(t *testing.T) {
	a := awareness.New(1)
	a.SetLocalState(map[string]any{"x": 1})
	clockBeforeRemoval := a.GetStates()[1].Clock
	a.SetLocalState(nil)
	require.NotContains(t, a.GetStates(), uint64(1), "removed after nil")

	// Snapshot the underlying record (states map keeps the entry with nil state).
	preHeartbeat := a.EncodeUpdate([]uint64{1})

	a.Heartbeat()

	// State still removed, EncodeUpdate output unchanged (no clock advance).
	assert.NotContains(t, a.GetStates(), uint64(1),
		"Heartbeat must not resurrect a removed client")
	assert.Equal(t, preHeartbeat, a.EncodeUpdate([]uint64{1}),
		"Heartbeat on removed client must be a no-op (encoded bytes identical)")
	_ = clockBeforeRemoval // referenced for clarity; not directly asserted
}

// C5 cont.: clocks must remain strictly monotonic across mixed Heartbeat and
// SetLocalState calls. This catches off-by-one bugs in the
// max(a.clock, cs.Clock) reconciliation logic that powers both methods.
func TestUnit_Awareness_C5_Heartbeat_PreservesMonotonicity_AcrossMixedUpdates(t *testing.T) {
	a := awareness.New(1)
	a.SetLocalState(map[string]any{"v": 1})

	var clocks []uint64
	record := func() { clocks = append(clocks, a.GetStates()[1].Clock) }
	record()

	a.Heartbeat()
	record()
	a.SetLocalState(map[string]any{"v": 2})
	record()
	a.Heartbeat()
	record()
	a.Heartbeat()
	record()
	a.SetLocalState(map[string]any{"v": 3})
	record()

	for i := 1; i < len(clocks); i++ {
		assert.Greater(t, clocks[i], clocks[i-1],
			"clock must strictly advance at step %d (got %d after %d)",
			i, clocks[i], clocks[i-1])
	}
}
