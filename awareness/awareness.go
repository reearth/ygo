package awareness

import (
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

// checkJSONDepth reports whether the JSON string s has at most maxJSONDepth
// levels of nesting. It scans bytes rather than parsing, so it runs in O(n).
//
// It tracks string context to avoid counting bracket characters inside JSON
// string values. Without this, {"key": "[[[["}  would be counted as depth 5
// instead of the correct depth 1, causing false-positive rejections (N-C3).
func checkJSONDepth(s string) bool {
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
}

// New creates an Awareness instance for the given client.
func New(clientID uint64) *Awareness {
	return &Awareness{
		clientID: clientID,
		states:   make(map[uint64]ClientState),
		meta:     make(map[uint64]time.Time),
	}
}

// ClientID returns the local client ID.
func (a *Awareness) ClientID() uint64 {
	return a.clientID
}

// SetLocalState updates the local client's state and increments the clock.
// Passing nil removes the local client from the awareness set.
func (a *Awareness) SetLocalState(state map[string]any) {
	a.mu.Lock()
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
		jsonStr  string
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
		jsonStr, err := dec.ReadVarString()
		if err != nil {
			return err
		}
		if len(jsonStr) > maxAwarenessStateBytes {
			return ErrStateTooLarge
		}
		entries = append(entries, entry{clientID, clock, jsonStr})
	}

	a.mu.Lock()
	var added, updated, removed []uint64

	for _, e := range entries {
		current, exists := a.states[e.clientID]
		// Only apply if incoming clock is strictly greater.
		if exists && e.clock <= current.Clock {
			continue
		}

		wasActive := exists && current.State != nil

		// Decode JSON state. Reject deeply nested payloads before unmarshalling
		// to prevent quadratic parse cost from crafted inputs like [[[[...]]]].
		isNull := e.jsonStr == "null" || e.jsonStr == ""
		var state map[string]any
		if !isNull {
			if !checkJSONDepth(e.jsonStr) {
				isNull = true
			} else if err := json.Unmarshal([]byte(e.jsonStr), &state); err != nil {
				isNull = true
			}
			if state == nil {
				isNull = true
			}
		}

		if isNull {
			// Store nil state with incoming clock to prevent stale re-application.
			a.states[e.clientID] = ClientState{Clock: e.clock, State: nil}
			delete(a.meta, e.clientID)
			if wasActive {
				removed = append(removed, e.clientID)
			}
		} else {
			a.states[e.clientID] = ClientState{
				Clock: e.clock,
				State: state,
			}
			a.meta[e.clientID] = time.Now()
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

// StartAutoExpiry starts a background goroutine that periodically calls
// RemoveExpired(timeout). The goroutine ticks at timeout/2 so that clients
// are expired within one tick period after their deadline. The returned
// function stops the goroutine; it must be called to avoid a goroutine leak.
// The stop function is also stored internally and will be called by Destroy.
func (a *Awareness) StartAutoExpiry(timeout time.Duration) func() {
	ticker := time.NewTicker(timeout / 2)
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
	stop := func() { close(done) }
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
func (a *Awareness) RemoveExpired(timeout time.Duration) {
	now := time.Now()
	a.mu.Lock()
	var removed []uint64
	for id, t := range a.meta {
		if now.Sub(t) >= timeout {
			// Mark as removed (keep clock for future clock comparisons).
			if cs, ok := a.states[id]; ok {
				a.states[id] = ClientState{Clock: cs.Clock, State: nil}
			}
			delete(a.meta, id)
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
