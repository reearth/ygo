package mobile

import (
	"encoding/json"
	"errors"
	"sync"

	"github.com/reearth/ygo/awareness"
)

// Awareness is a gomobile-safe wrapper around *awareness.Awareness.
type Awareness struct {
	mu sync.RWMutex
	a  *awareness.Awareness // nil after Close

	// subsMu guards the presence-change-observer registry. It is DISTINCT from
	// mu on purpose: mu.RLock is shared, so mutating subs under it would
	// data-race a concurrent Observe. Never take subsMu while holding mu for
	// writing and vice-versa in a way that could invert lock order.
	subsMu sync.Mutex
	subs   map[*Subscription]struct{}
}

// NewAwareness creates an awareness instance for clientID (in [0, 2^53 - 1],
// the JS safe-integer range).
func NewAwareness(clientID int64) (*Awareness, error) {
	if err := checkClientID(clientID); err != nil {
		return nil, err
	}
	return &Awareness{a: awareness.New(uint64(clientID))}, nil
}

// Each method holds w.mu for the full duration of the underlying call: a shared
// (read) lock for operations, an exclusive lock for Close. This keeps the
// lifecycle linearizable — an operation either completes before Close or, once
// Close has run Destroy and set a = nil, observes it and returns ErrClosed / a
// zero value, so no method ever runs against a destroyed awareness. The
// underlying *awareness.Awareness has its own internal locking; no method calls
// another method, so the read lock is never taken re-entrantly.

// ClientID returns the local client ID, or 0 after Close.
func (w *Awareness) ClientID() int64 {
	w.mu.RLock()
	defer w.mu.RUnlock()
	if w.a == nil {
		return 0
	}
	return int64(w.a.ClientID())
}

// SetLocalState sets the local state from a JSON object. A zero-length stateJSON
// or the JSON literal `null` is rejected (to remove presence, call
// ClearLocalState); `{}` is a valid present-but-empty state. Returns ErrClosed
// after Close.
func (w *Awareness) SetLocalState(stateJSON []byte) error {
	w.mu.RLock()
	defer w.mu.RUnlock()
	if w.a == nil {
		return ErrClosed
	}
	if len(stateJSON) == 0 {
		return errors.New("ygo/mobile: empty stateJSON; use ClearLocalState to remove presence")
	}
	var m map[string]any
	if err := json.Unmarshal(stateJSON, &m); err != nil {
		return err
	}
	if m == nil {
		// JSON `null` unmarshals into a nil map without error; passing it to
		// SetLocalState would silently remove presence. Reject it so removal
		// only ever happens through the explicit ClearLocalState.
		return errors.New("ygo/mobile: stateJSON is null; use ClearLocalState to remove presence")
	}
	w.a.SetLocalState(m)
	return nil
}

// ClearLocalState removes the local client's presence. No-op after Close.
func (w *Awareness) ClearLocalState() {
	w.mu.RLock()
	defer w.mu.RUnlock()
	if w.a == nil {
		return
	}
	w.a.SetLocalState(nil)
}

// LocalStateJSON returns the local state as JSON. It yields JSON `null` when no
// local state is set — either freshly constructed or after ClearLocalState — and
// a JSON object (e.g. `{}` or `{"k":v}`) once a state has been set via
// SetLocalState. So `null` vs `{}` is a meaningful present/absent distinction
// consumers can rely on: `{}` is a present-but-empty state, `null` is no presence
// yet. Returns ErrClosed after Close.
func (w *Awareness) LocalStateJSON() ([]byte, error) {
	w.mu.RLock()
	defer w.mu.RUnlock()
	if w.a == nil {
		return nil, ErrClosed
	}
	return json.Marshal(w.a.GetLocalState())
}

// StatesJSON returns all active client states as a JSON object keyed by stringy
// client ID, each value being that client's raw state object (the internal clock
// is not exposed), e.g. `{"1":{"user":"alice"}}`. An empty set returns `{}`.
// Returns ErrClosed after Close.
func (w *Awareness) StatesJSON() ([]byte, error) {
	w.mu.RLock()
	defer w.mu.RUnlock()
	if w.a == nil {
		return nil, ErrClosed
	}
	return statesToIdiomaticJSON(w.a.GetStates())
}

// EncodeAll encodes the full awareness state for all known clients. nil after Close.
func (w *Awareness) EncodeAll() []byte {
	w.mu.RLock()
	defer w.mu.RUnlock()
	if w.a == nil {
		return nil
	}
	// GetStates returns active clients only, so EncodeAll snapshots live presence
	// (it does not propagate removal tombstones — that's by design for a full snapshot).
	states := w.a.GetStates()
	ids := make([]uint64, 0, len(states))
	for id := range states {
		ids = append(ids, id)
	}
	return w.a.EncodeUpdate(ids)
}

// ApplyUpdate merges an awareness update from a peer. Returns ErrClosed after Close.
func (w *Awareness) ApplyUpdate(update []byte) error {
	w.mu.RLock()
	defer w.mu.RUnlock()
	if w.a == nil {
		return ErrClosed
	}
	return w.a.ApplyUpdate(update, nil)
}

// Close releases the underlying awareness state (stopping any background work).
// Idempotent. After Close, methods return ErrClosed / zero values.
//
// Close destroys the awareness and nils a under the write lock FIRST, then
// detaches every presence-change subscription (unsubscribing the awareness
// bridge and signalling each drain goroutine to stop). Ordering matters: an
// in-flight Observe holds w.mu.RLock for its whole body and registers itself
// only near the end, so taking the write lock first guarantees every such
// Observe has finished registering before we snapshot w.subs (no missed
// subscription → no leaked drain goroutine). Any Observe starting after this
// sees a == nil and returns the closed stub without launching a drain.
//
// Close does not join drain goroutines: one may be inside OnChange re-entering a
// mobile method that needs w.mu, so joining under a lock would deadlock. s.Close
// never takes w.mu, so detaching after releasing the write lock keeps the
// signal-don't-join / no-lock-across-callback property. No callback starts after
// Close — the bridge is unsubscribed and each drain sees stopped.
func (w *Awareness) Close() {
	w.mu.Lock()
	if w.a != nil {
		w.a.Destroy()
		w.a = nil
	}
	w.mu.Unlock()
	w.subsMu.Lock()
	subs := make([]*Subscription, 0, len(w.subs))
	for s := range w.subs {
		subs = append(subs, s)
	}
	w.subsMu.Unlock()
	for _, s := range subs {
		s.Close()
	}
}
