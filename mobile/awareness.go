package mobile

import (
	"encoding/json"
	"errors"
	"sync"

	"github.com/reearth/ygo/awareness"
)

// Awareness is a gomobile-safe wrapper around *awareness.Awareness.
type Awareness struct {
	mu sync.Mutex
	a  *awareness.Awareness // nil after Close
}

// NewAwareness creates an awareness instance for clientID (in [0, 2^53]).
func NewAwareness(clientID int64) (*Awareness, error) {
	if err := checkClientID(clientID); err != nil {
		return nil, err
	}
	return &Awareness{a: awareness.New(uint64(clientID))}, nil
}

// inner returns the underlying awareness, or nil if closed, reading under the
// guard so it cannot race Close. The returned *awareness.Awareness has its own
// internal locking.
func (w *Awareness) inner() *awareness.Awareness {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.a
}

// ClientID returns the local client ID, or 0 after Close.
func (w *Awareness) ClientID() int64 {
	a := w.inner()
	if a == nil {
		return 0
	}
	return int64(a.ClientID())
}

// SetLocalState sets the local state from a JSON object. A zero-length stateJSON
// is rejected (to remove presence, call ClearLocalState); `{}` is a valid
// present-but-empty state. Returns ErrClosed after Close.
func (w *Awareness) SetLocalState(stateJSON []byte) error {
	a := w.inner()
	if a == nil {
		return ErrClosed
	}
	if len(stateJSON) == 0 {
		return errors.New("ygo/mobile: empty stateJSON; use ClearLocalState to remove presence")
	}
	var m map[string]any
	if err := json.Unmarshal(stateJSON, &m); err != nil {
		return err
	}
	a.SetLocalState(m)
	return nil
}

// ClearLocalState removes the local client's presence. No-op after Close.
func (w *Awareness) ClearLocalState() {
	a := w.inner()
	if a == nil {
		return
	}
	a.SetLocalState(nil)
}

// LocalStateJSON returns the local state as JSON. Returns ErrClosed after Close.
func (w *Awareness) LocalStateJSON() ([]byte, error) {
	a := w.inner()
	if a == nil {
		return nil, ErrClosed
	}
	return json.Marshal(a.GetLocalState())
}

// StatesJSON returns all known client states as a JSON object keyed by client ID.
// Returns ErrClosed after Close.
func (w *Awareness) StatesJSON() ([]byte, error) {
	a := w.inner()
	if a == nil {
		return nil, ErrClosed
	}
	return json.Marshal(a.GetStates())
}

// EncodeAll encodes the full awareness state for all known clients. nil after Close.
func (w *Awareness) EncodeAll() []byte {
	a := w.inner()
	if a == nil {
		return nil
	}
	// GetStates returns active clients only, so EncodeAll snapshots live presence
	// (it does not propagate removal tombstones — that's by design for a full snapshot).
	states := a.GetStates()
	ids := make([]uint64, 0, len(states))
	for id := range states {
		ids = append(ids, id)
	}
	return a.EncodeUpdate(ids)
}

// ApplyUpdate merges an awareness update from a peer. Returns ErrClosed after Close.
func (w *Awareness) ApplyUpdate(update []byte) error {
	a := w.inner()
	if a == nil {
		return ErrClosed
	}
	return a.ApplyUpdate(update, nil)
}

// Close releases the underlying awareness state (stopping any background work).
// Idempotent.
func (w *Awareness) Close() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.a != nil {
		w.a.Destroy()
		w.a = nil
	}
}
