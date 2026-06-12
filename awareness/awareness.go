package awareness

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"sync"
	"time"

	"github.com/reearth/ygo/encoding"
)

const DefaultTimeout = 30 * time.Second

// maxJSONDepth is the maximum nesting depth accepted for a client state JSON
// string. Go's encoding/json has no depth limit, so a payload of deeply nested
// arrays/maps triggers quadratic parsing. States exceeding this depth are
// treated as null (removed).
const maxJSONDepth = 20

// checkJSONDepth reports whether the JSON byte slice s has at most maxJSONDepth
// levels of nesting. It scans bytes rather than parsing, so it runs in O(n).
//
// It tracks string context to avoid counting bracket characters inside JSON
// string values. Without this, {"key": "[[[["}  would be counted as depth 5
// instead of the correct depth 1, causing false-positive rejections (N-C3).
//
// Takes []byte (not string) so callers can pass a Decoder sub-slice without
// the []byte→string conversion copy.
func checkJSONDepth(s []byte) bool {
	depth := 0
	inString := false
	i := 0
	for i < len(s) {
		c := s[i]
		if inString {
			if c == '\\' {
				i += 2 // skip escaped character (handles \" correctly)
				continue
			}
			if c == '"' {
				inString = false
			}
		} else {
			switch c {
			case '"':
				inString = true
			case '{', '[':
				depth++
				if depth > maxJSONDepth {
					return false
				}
			case '}', ']':
				depth--
			}
		}
		i++
	}
	return !inString // unterminated string is malformed → reject
}

// maxAwarenessClients is the maximum number of client entries accepted in a
// single ApplyUpdate call. Prevents OOM from a crafted message that claims a
// huge client count, causing make([]entry, 0, n) to allocate exabytes.
const maxAwarenessClients = 100_000

// maxAwarenessStateBytes is the maximum size (bytes) of a single client's
// JSON state string. Prevents OOM from a peer broadcasting a multi-GB state.
const maxAwarenessStateBytes = 1 << 20 // 1 MiB

// maxStateKeys is the maximum number of top-level keys accepted in a client's
// decoded state object. The existing 1 MiB byte cap allows a single payload
// like {"k1":1,"k2":1,...,"k65535":1} (~10 bytes per key * 65k keys = ~650 KiB)
// that would materialise into a multi-MB map. States exceeding this key count
// are dropped (treated as null). See issue #48 vector A.
const maxStateKeys = 1000

// ErrTooManyClients is returned when an update claims more clients than maxAwarenessClients.
var ErrTooManyClients = errors.New("awareness: update exceeds maximum client count")

// ErrStateTooLarge is returned when a single client state exceeds maxAwarenessStateBytes.
var ErrStateTooLarge = errors.New("awareness: client state exceeds maximum size")

// ClientState holds the clock and decoded state for one peer.
type ClientState struct {
	Clock uint64
	State map[string]any // nil means the client was removed
}

// ChangeEvent is delivered to observers when states change.
type ChangeEvent struct {
	Added   []uint64 // client IDs newly seen
	Updated []uint64 // client IDs whose state changed
	Removed []uint64 // client IDs whose state was set to null
	Origin  any
}

// observer wraps a callback with an active flag so it can be unsubscribed
// without shifting the slice.
type observer struct {
	fn     func(ChangeEvent)
	active bool
}

// Awareness tracks ephemeral peer state.
type Awareness struct {
	clientID uint64
	mu       sync.RWMutex
	// states stores all known clients including those with nil State (removed).
	// Clients with nil State have been removed but their clock is retained so
	// removal messages can be properly encoded with an up-to-date clock.
	states     map[uint64]ClientState
	meta       map[uint64]time.Time // last update time, for expiry (only active clients)
	clock      uint64               // local client's clock
	observers  []*observer
	stopExpiry func() // set by StartAutoExpiry; stopped by Destroy
	// wireBytes tracks the JSON byte length of the last ApplyUpdate-accepted
	// state per client. Used to enforce maxBytes (issue #48 vector B). Only
	// entries that arrived via ApplyUpdate are counted; SetLocalState is
	// excluded because the local client's state is set by trusted embedder
	// code, not adversarial wire input.
	wireBytes   map[uint64]int
	activeBytes int64 // sum of wireBytes values
	maxBytes    int64 // 0 = unlimited (default; backward compatible)
	maxClients  int   // 0 = unlimited (default; backward compatible)
}

// New creates an Awareness instance for the given client.
func New(clientID uint64) *Awareness {
	return &Awareness{
		clientID:  clientID,
		states:    make(map[uint64]ClientState),
		meta:      make(map[uint64]time.Time),
		wireBytes: make(map[uint64]int),
	}
}

// SetMaxBytes caps the cumulative byte size of awareness state across all
// remote clients merged via ApplyUpdate. Incoming entries that would push the
// total past the cap are silently dropped (treated as null) rather than
// rejected with an error — matches the existing pattern for oversized-state
// handling so a single misbehaving peer cannot cause an entire room's
// awareness merge to fail.
//
// A value of 0 (the default) disables the cap. Local state set via
// SetLocalState is not counted; the cap is intended to constrain untrusted
// wire input only. See issue #48 vector B.
//
// Set this once at construction time; changes while concurrent updates are
// being processed are safe (the field is read under a.mu).
func (a *Awareness) SetMaxBytes(n int64) {
	a.mu.Lock()
	a.maxBytes = n
	a.mu.Unlock()
}

// SetMaxClients caps the number of distinct client entries this Awareness will
// track — live presence plus retained removal tombstones. Once the cap is
// reached, ApplyUpdate refuses previously-unseen client IDs; clients already
// tracked can still update and be removed. This bounds memory against a peer
// that invents unbounded client IDs to exhaust the room — including null-state
// entries, which are NOT subject to the byte cap (see SetMaxBytes) and so would
// otherwise grow the states map without limit.
//
// A value of 0 (the default) disables the cap. Local state set via
// SetLocalState is exempt — the local client is always allowed. Suggested
// production value: a few thousand to tens of thousands per room.
//
// Set this once at construction time; changes while concurrent updates are
// being processed are safe (the field is read under a.mu).
func (a *Awareness) SetMaxClients(n int) {
	a.mu.Lock()
	a.maxClients = n
	a.mu.Unlock()
}

// ClientID returns the local client ID.
func (a *Awareness) ClientID() uint64 {
	return a.clientID
}

// SetLocalState updates the local client's state and increments the clock.
// Passing nil removes the local client from the awareness set.
func (a *Awareness) SetLocalState(state map[string]any) {
	a.mu.Lock()
	// Reconcile a.clock with any remote echo of our clientID that bumped
	// states[a.clientID].Clock past a.clock — e.g. another tab also acting
	// as this clientID. Without this, a.clock++ could produce a value lower
	// than what peers have already seen, and the update would be ignored
	// downstream (#73 vector C3). yrs derives the local clock directly from
	// meta.get(clientID).clock + 1; we do the equivalent via a max() step.
	if cs, ok := a.states[a.clientID]; ok && cs.Clock > a.clock {
		a.clock = cs.Clock
	}
	// Saturate at MaxUint64 rather than wrapping: a wrap-around would make new
	// states appear older than existing ones, breaking monotonicity.
	if a.clock < math.MaxUint64 {
		a.clock++
	}
	var added, updated, removed []uint64

	prev, exists := a.states[a.clientID]
	// "exists and active" means prev.State != nil
	wasActive := exists && prev.State != nil

	if state == nil {
		// Store nil state with incremented clock so it can be encoded correctly.
		a.states[a.clientID] = ClientState{Clock: a.clock, State: nil}
		delete(a.meta, a.clientID)
		if wasActive {
			removed = []uint64{a.clientID}
		}
	} else {
		a.states[a.clientID] = ClientState{Clock: a.clock, State: state}
		a.meta[a.clientID] = time.Now()
		if wasActive {
			updated = []uint64{a.clientID}
		} else {
			added = []uint64{a.clientID}
		}
	}

	obs := a.copyObservers()
	a.mu.Unlock()

	if len(added) > 0 || len(updated) > 0 || len(removed) > 0 {
		evt := ChangeEvent{Added: added, Updated: updated, Removed: removed}
		fireObservers(obs, evt)
	}
}

// Heartbeat re-emits the local client's current state at an incremented
// clock so peers learn that we're still alive even when the state hasn't
// changed. No-op when no local state is set or when the local client has
// been removed (state == nil).
//
// Typical use is to schedule periodic calls alongside StartAutoExpiry on
// peers — they expire clients that go quiet, this advertises that we
// haven't. Matches Yjs JS's constructor interval which re-emits local
// state every outdatedTimeout/2.
//
// Observers are NOT fired — the state itself didn't change, only the
// clock advanced. Peers will pick up the new clock via EncodeUpdate.
//
// Added in v1.11.0 (#73 vector C5).
func (a *Awareness) Heartbeat() {
	a.mu.Lock()
	cs, ok := a.states[a.clientID]
	if !ok || cs.State == nil {
		a.mu.Unlock()
		return
	}
	if a.clock < cs.Clock {
		a.clock = cs.Clock
	}
	if a.clock < math.MaxUint64 {
		a.clock++
	}
	a.states[a.clientID] = ClientState{Clock: a.clock, State: cs.State}
	a.meta[a.clientID] = time.Now()
	a.mu.Unlock()
}

// SetLocalStateContext is the context-aware variant of SetLocalState.
// If ctx is already cancelled, the state is NOT updated and ctx.Err()
// is returned. Otherwise the state is updated and nil is returned.
//
// Like TransactContext (see crdt.Doc.TransactContext), mid-call ctx
// cancellation is not interrupted — the operation itself is short and
// uninterruptible. This method provides cooperative entry-point
// cancellation only.
func (a *Awareness) SetLocalStateContext(ctx context.Context, state map[string]any) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	a.SetLocalState(state)
	return nil
}

// GetLocalState returns the local client's current state (nil if not set or removed).
func (a *Awareness) GetLocalState() map[string]any {
	a.mu.RLock()
	defer a.mu.RUnlock()
	cs, ok := a.states[a.clientID]
	if !ok {
		return nil
	}
	return cs.State
}

// GetStates returns a snapshot of all known active client states.
// Removed clients (State == nil) are excluded.
func (a *Awareness) GetStates() map[uint64]ClientState {
	a.mu.RLock()
	defer a.mu.RUnlock()
	out := make(map[uint64]ClientState, len(a.states))
	for k, v := range a.states {
		if v.State != nil {
			out[k] = v
		}
	}
	return out
}

// OnChange registers a callback invoked whenever any state changes.
// Returns an unsubscribe function.
func (a *Awareness) OnChange(fn func(ChangeEvent)) func() {
	if fn == nil {
		return func() {}
	}
	obs := &observer{fn: fn, active: true}
	a.mu.Lock()
	a.observers = append(a.observers, obs)
	a.mu.Unlock()

	return func() {
		a.mu.Lock()
		obs.active = false
		a.mu.Unlock()
	}
}

// copyObservers returns a snapshot of active observer functions.
// Must be called while holding a.mu (read or write).
func (a *Awareness) copyObservers() []func(ChangeEvent) {
	fns := make([]func(ChangeEvent), 0, len(a.observers))
	for _, o := range a.observers {
		if o.active {
			fns = append(fns, o.fn)
		}
	}
	return fns
}

// fireObservers calls each observer function in turn.
func fireObservers(fns []func(ChangeEvent), evt ChangeEvent) {
	for _, fn := range fns {
		fn(evt)
	}
}

// EncodeUpdate encodes the current state of the given client IDs into a
// binary awareness update message. Pass nil to encode all known clients,
// including those with a nil (removed) State, which encodes as JSON null
// and signals removal to peers.
func (a *Awareness) EncodeUpdate(clientIDs []uint64) []byte {
	a.mu.RLock()
	defer a.mu.RUnlock()

	var ids []uint64
	if clientIDs == nil {
		ids = make([]uint64, 0, len(a.states))
		for id := range a.states {
			ids = append(ids, id)
		}
	} else {
		ids = clientIDs
	}

	enc := encoding.NewEncoder()
	enc.WriteVarUint(uint64(len(ids)))
	for _, id := range ids {
		cs, ok := a.states[id]
		enc.WriteVarUint(id)
		if !ok {
			enc.WriteVarUint(0)
			enc.WriteVarString("null")
			continue
		}
		enc.WriteVarUint(cs.Clock)
		if cs.State == nil {
			enc.WriteVarString("null")
		} else {
			b, err := json.Marshal(cs.State)
			if err != nil {
				enc.WriteVarString("null")
			} else {
				enc.WriteVarString(string(b))
			}
		}
	}
	return enc.Bytes()
}

// ApplyUpdate decodes an incoming awareness update and merges it.
// Only updates with a higher clock than the current one are applied.
// Returns ErrTooManyClients if the update claims more than maxAwarenessClients
// entries, or ErrStateTooLarge if any single state JSON exceeds maxAwarenessStateBytes.
func (a *Awareness) ApplyUpdate(update []byte, origin any) error {
	dec := encoding.NewDecoder(update)

	numClients, err := dec.ReadVarUint()
	if err != nil {
		return err
	}
	if numClients > maxAwarenessClients {
		return ErrTooManyClients
	}

	type entry struct {
		clientID uint64
		clock    uint64
		// jsonBytes aliases the decoder's underlying buffer (zero-copy).
		// Stable for the duration of ApplyUpdate; callers must not mutate
		// the input slice while ApplyUpdate is running. Replacing string
		// with []byte here eliminates two copies per entry: one in
		// ReadVarString's []byte→string conversion and one in
		// json.Unmarshal's string→[]byte conversion downstream.
		jsonBytes []byte
	}
	entries := make([]entry, 0, numClients)
	for i := uint64(0); i < numClients; i++ {
		clientID, err := dec.ReadVarUint()
		if err != nil {
			return err
		}
		clock, err := dec.ReadVarUint()
		if err != nil {
			return err
		}
		jsonBytes, err := dec.ReadVarBytes()
		if err != nil {
			return err
		}
		if len(jsonBytes) > maxAwarenessStateBytes {
			return ErrStateTooLarge
		}
		entries = append(entries, entry{clientID, clock, jsonBytes})
	}

	a.mu.Lock()
	var added, updated, removed []uint64

	for _, e := range entries {
		current, exists := a.states[e.clientID]
		// string([]byte) == "constant" is optimised by the Go compiler to
		// length+content comparison without allocating, since Go 1.5.
		isNullEntry := string(e.jsonBytes) == "null" || len(e.jsonBytes) == 0

		// y-protocols clock gate (#73 vector C2):
		//   - strictly-older clock → drop
		//   - equal clock + null + currently-active → accept (offline signal)
		//   - equal clock + non-null OR equal clock + already-removed → drop (no new info)
		//   - strictly-newer clock → accept
		if exists {
			if e.clock < current.Clock {
				continue
			}
			if e.clock == current.Clock {
				validRemoval := isNullEntry && current.State != nil
				if !validRemoval {
					continue
				}
			}
		}

		// Self-state protection (#73 vector C1): if a remote sends a null
		// update for OUR clientID, don't honor it — bump our local clock past
		// the incoming and re-emit local state so peers learn the new value.
		// Matches Yjs JS / yrs which both detect this and override the remote.
		if e.clientID == a.clientID && isNullEntry {
			if e.clock >= a.clock {
				a.clock = e.clock + 1
			}
			// If we currently have an active local state, re-emit it at the
			// bumped clock. Wire-bytes accounting is left alone — local state
			// is excluded from the wireBytes cap (see SetMaxBytes godoc).
			if exists && current.State != nil {
				a.states[a.clientID] = ClientState{Clock: a.clock, State: current.State}
				updated = append(updated, a.clientID)
			}
			continue
		}

		// Distinct-entry cap (#48 / S-1 DoS guard): once the room is at
		// capacity, refuse previously-unseen client IDs. Already-tracked clients
		// (exists) still update and can be removed. This bounds the states map
		// against a peer inventing unbounded client IDs — crucially including
		// null-state entries, which bypass the byte cap above. The local client
		// is exempt — it is set by trusted embedder code, not adversarial wire
		// input — so it does not count toward the cap and is never rejected here
		// (the self-state branch above already handles a null for our own ID).
		if !exists && a.maxClients > 0 {
			remote := len(a.states)
			if _, hasLocal := a.states[a.clientID]; hasLocal {
				remote-- // exclude the exempt local entry from the cap
			}
			if remote >= a.maxClients {
				continue
			}
		}

		wasActive := exists && current.State != nil

		// Decode JSON state. Reject deeply nested payloads before unmarshalling
		// to prevent quadratic parse cost from crafted inputs like [[[[...]]]].
		// isNull starts from isNullEntry (computed at the gate above) and can
		// be further set true by the depth check, unmarshal failure, or one
		// of the resource caps below.
		isNull := isNullEntry
		var state map[string]any
		if !isNull {
			if !checkJSONDepth(e.jsonBytes) {
				isNull = true
			} else if err := json.Unmarshal(e.jsonBytes, &state); err != nil {
				isNull = true
			}
			if state == nil {
				isNull = true
			}
		}

		// Issue #48 vector A: cap the number of keys in a decoded state to
		// prevent a small (under 1 MiB) JSON payload from materialising into
		// a huge map.
		if !isNull && len(state) > maxStateKeys {
			isNull = true
			state = nil
		}

		// Issue #48 vector B: enforce per-room cumulative byte cap. Compute
		// the byte delta this entry would introduce (new size minus the
		// previously-tracked size for this client) and drop the entry if
		// applying it would exceed maxBytes.
		newSize := len(e.jsonBytes)
		oldSize := a.wireBytes[e.clientID]
		if !isNull && a.maxBytes > 0 {
			delta := int64(newSize - oldSize)
			if a.activeBytes+delta > a.maxBytes {
				isNull = true
				state = nil
			}
		}

		if isNull {
			// Store nil state with incoming clock to prevent stale re-application.
			a.states[e.clientID] = ClientState{Clock: e.clock, State: nil}
			delete(a.meta, e.clientID)
			// Release any wire-bytes the previous active state held for this client.
			if oldSize > 0 {
				a.activeBytes -= int64(oldSize)
				delete(a.wireBytes, e.clientID)
			}
			if wasActive {
				removed = append(removed, e.clientID)
			}
		} else {
			a.states[e.clientID] = ClientState{
				Clock: e.clock,
				State: state,
			}
			a.meta[e.clientID] = time.Now()
			// Update byte accounting: replace the old size with the new size.
			a.activeBytes += int64(newSize - oldSize)
			a.wireBytes[e.clientID] = newSize
			if wasActive {
				updated = append(updated, e.clientID)
			} else {
				added = append(added, e.clientID)
			}
		}
	}

	obs := a.copyObservers()
	a.mu.Unlock()

	if len(added) > 0 || len(updated) > 0 || len(removed) > 0 {
		evt := ChangeEvent{Added: added, Updated: updated, Removed: removed, Origin: origin}
		fireObservers(obs, evt)
	}

	return nil
}

// ApplyUpdateContext is the context-aware variant of ApplyUpdate.
// If ctx is already cancelled, the update is NOT applied and ctx.Err()
// is returned. Otherwise the update is applied and the underlying
// ApplyUpdate's error (or nil) is returned.
func (a *Awareness) ApplyUpdateContext(ctx context.Context, update []byte, origin any) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return a.ApplyUpdate(update, origin)
}

// StartAutoExpiry starts a background goroutine that periodically calls
// RemoveExpired(timeout). The goroutine ticks at timeout/2 so that clients
// are expired within one tick period after their deadline. The returned
// function stops the goroutine; it must be called to avoid a goroutine leak.
// The stop function is also stored internally and will be called by Destroy.
func (a *Awareness) StartAutoExpiry(timeout time.Duration) func() {
	// Stop any previously-started goroutine to prevent leaking it (#34).
	a.mu.Lock()
	prev := a.stopExpiry
	a.stopExpiry = nil
	a.mu.Unlock()
	if prev != nil {
		prev()
	}

	// Tick at half the timeout so an entry is checked at least twice within its
	// window. Clamp to a positive minimum: time.NewTicker panics on a zero or
	// negative duration, and timeout is caller- (and CLI-) configurable, so a
	// sub-2ns value would make timeout/2 round to 0 and crash the room.
	interval := timeout / 2
	if interval <= 0 {
		interval = 1
	}
	ticker := time.NewTicker(interval)
	done := make(chan struct{})
	go func() {
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				a.RemoveExpired(timeout)
			case <-done:
				return
			}
		}
	}()
	var stopOnce sync.Once
	stop := func() {
		stopOnce.Do(func() { close(done) })
	}
	a.mu.Lock()
	a.stopExpiry = stop
	a.mu.Unlock()
	return stop
}

// Destroy stops the auto-expiry goroutine (if started) and releases
// associated resources. Safe to call more than once.
func (a *Awareness) Destroy() {
	a.mu.Lock()
	stop := a.stopExpiry
	a.stopExpiry = nil
	a.mu.Unlock()
	if stop != nil {
		stop()
	}
}

// RemoveExpired removes clients whose last update is older than timeout.
// Only active clients (with non-nil State) are tracked in meta and can expire.
//
// The local client is exempt: peers can't reliably tell whether we've gone
// silent, so it's our job to broadcast presence via SetLocalState or
// Heartbeat. Self-expiry would create false offline signals on quiet rooms.
// Matches y-protocols / yrs behavior (#73 vector C4).
func (a *Awareness) RemoveExpired(timeout time.Duration) {
	now := time.Now()
	a.mu.Lock()
	var removed []uint64
	for id, t := range a.meta {
		if id == a.clientID {
			continue // never self-expire
		}
		if now.Sub(t) >= timeout {
			// Mark as removed (keep clock for future clock comparisons).
			if cs, ok := a.states[id]; ok {
				a.states[id] = ClientState{Clock: cs.Clock, State: nil}
			}
			delete(a.meta, id)
			// Release any wire-bytes the expired client held (#48 vector B).
			if size, ok := a.wireBytes[id]; ok {
				a.activeBytes -= int64(size)
				delete(a.wireBytes, id)
			}
			removed = append(removed, id)
		}
	}
	obs := a.copyObservers()
	a.mu.Unlock()

	if len(removed) > 0 {
		evt := ChangeEvent{Removed: removed}
		fireObservers(obs, evt)
	}
}
