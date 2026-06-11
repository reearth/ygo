package mobile

import (
	"encoding/json"
	"sync"

	"github.com/reearth/ygo/crdt"
)

// Doc is a gomobile-safe wrapper around *crdt.Doc.
type Doc struct {
	mu sync.RWMutex
	d  *crdt.Doc // nil after Close
}

// NewDoc creates a document with a random client ID.
func NewDoc() *Doc { return &Doc{d: crdt.New()} }

// NewDocWithClientID creates a document with the given client ID. id must be in
// [0, 2^53 - 1] (the JS safe-integer range) for cross-language interop.
func NewDocWithClientID(id int64) (*Doc, error) {
	if err := checkClientID(id); err != nil {
		return nil, err
	}
	return &Doc{d: crdt.New(crdt.WithClientID(crdt.ClientID(uint64(id))))}, nil
}

// Each method holds m.mu for the full duration of the underlying call: a shared
// (read) lock for operations, an exclusive lock for Close. This keeps the
// lifecycle linearizable — an operation either completes before Close or, once
// Close has set d = nil, observes it and returns ErrClosed / a zero value. The
// underlying *crdt.Doc has its own internal locking, so concurrent operations
// (all holding the read lock) still serialize their mutations there; only Close
// is exclusive. No method calls another method, so the read lock is never taken
// re-entrantly.

// ClientID returns the document's client ID, or 0 after Close.
func (m *Doc) ClientID() int64 {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if m.d == nil {
		return 0
	}
	return int64(m.d.ClientID())
}

// Close releases the underlying document for prompt Go-side collection.
// Idempotent. After Close, methods return ErrClosed / zero values. Close blocks
// until any in-flight operation completes, so it never tears down mid-call.
func (m *Doc) Close() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.d = nil
}

// ApplyUpdate merges a V1 update from a peer. Returns ErrClosed after Close.
func (m *Doc) ApplyUpdate(update []byte) error {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if m.d == nil {
		return ErrClosed
	}
	return crdt.ApplyUpdateV1(m.d, update, nil)
}

// EncodeStateAsUpdate returns the full document state as a V1 update.
// Returns nil after Close.
func (m *Doc) EncodeStateAsUpdate() []byte {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if m.d == nil {
		return nil
	}
	return crdt.EncodeStateAsUpdateV1(m.d, nil)
}

// EncodeStateVector returns this document's encoded state vector.
// Returns nil after Close.
func (m *Doc) EncodeStateVector() []byte {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if m.d == nil {
		return nil
	}
	return crdt.EncodeStateVectorV1(m.d)
}

// GetText returns the plain-text content of the named YText root ("" after Close
// or for an absent/empty root).
func (m *Doc) GetText(name string) string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if m.d == nil {
		return ""
	}
	return m.d.GetText(name).ToString()
}

// GetTextJSON returns the named YText root's formatted content as JSON.
//
// NOTE: the current shape is ygo's internal crdt.Delta struct marshaled
// directly — an array of ops with capitalized Go field names, e.g.
// [{"Op":0,"Insert":"hi","Attributes":{"bold":true}}] — NOT the idiomatic Yjs
// delta shape ([{"insert":"hi","attributes":{...}}]). Emitting the idiomatic
// shape is tracked in https://github.com/reearth/ygo/issues/109; consumers
// should not hard-code against this shape long-term. Returns ErrClosed after Close.
func (m *Doc) GetTextJSON(name string) ([]byte, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if m.d == nil {
		return nil, ErrClosed
	}
	delta := m.d.GetText(name).ToDelta()
	if delta == nil {
		// ToDelta returns a nil slice for an absent/empty text root, which
		// json.Marshal emits as `null` — a JS consumer doing delta.forEach(...)
		// on null would crash. Emit `[]` instead.
		delta = []crdt.Delta{}
	}
	return json.Marshal(delta)
}

// GetMapJSON returns the named YMap root as JSON.
func (m *Doc) GetMapJSON(name string) ([]byte, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if m.d == nil {
		return nil, ErrClosed
	}
	return m.d.GetMap(name).ToJSON()
}

// GetArrayJSON returns the named YArray root as JSON.
func (m *Doc) GetArrayJSON(name string) ([]byte, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if m.d == nil {
		return nil, ErrClosed
	}
	return m.d.GetArray(name).ToJSON()
}

// EncodeDiff returns the updates this document has that the remote (described by
// its encoded state vector) is missing. Returns ErrClosed after Close.
func (m *Doc) EncodeDiff(remoteStateVector []byte) ([]byte, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if m.d == nil {
		return nil, ErrClosed
	}
	sv, err := crdt.DecodeStateVectorV1(remoteStateVector)
	if err != nil {
		return nil, err
	}
	return crdt.EncodeStateAsUpdateV1(m.d, sv), nil
}
