package mobile

import (
	"sync"

	"github.com/reearth/ygo/crdt"
)

// Doc is a gomobile-safe wrapper around *crdt.Doc.
type Doc struct {
	mu sync.Mutex
	d  *crdt.Doc // nil after Close
}

// NewDoc creates a document with a random client ID.
func NewDoc() *Doc { return &Doc{d: crdt.New()} }

// NewDocWithClientID creates a document with the given client ID. id must be in
// [0, 2^53] (JS safe-integer range) for cross-language interop.
func NewDocWithClientID(id int64) (*Doc, error) {
	if err := checkClientID(id); err != nil {
		return nil, err
	}
	return &Doc{d: crdt.New(crdt.WithClientID(crdt.ClientID(uint64(id))))}, nil
}

// inner returns the underlying doc, or nil if closed, reading under the guard so
// it cannot race Close. The returned *crdt.Doc has its own internal locking.
func (m *Doc) inner() *crdt.Doc {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.d
}

// ClientID returns the document's client ID, or 0 after Close.
func (m *Doc) ClientID() int64 {
	d := m.inner()
	if d == nil {
		return 0
	}
	return int64(d.ClientID())
}

// Close releases the underlying document for prompt Go-side collection.
// Idempotent. After Close, methods return ErrClosed / zero values.
func (m *Doc) Close() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.d = nil
}

// ApplyUpdate merges a V1 update from a peer. Returns ErrClosed after Close.
func (m *Doc) ApplyUpdate(update []byte) error {
	d := m.inner()
	if d == nil {
		return ErrClosed
	}
	return crdt.ApplyUpdateV1(d, update, nil)
}

// EncodeStateAsUpdate returns the full document state as a V1 update.
// Returns nil after Close.
func (m *Doc) EncodeStateAsUpdate() []byte {
	d := m.inner()
	if d == nil {
		return nil
	}
	return crdt.EncodeStateAsUpdateV1(d, nil)
}

// EncodeStateVector returns this document's encoded state vector.
// Returns nil after Close.
func (m *Doc) EncodeStateVector() []byte {
	d := m.inner()
	if d == nil {
		return nil
	}
	return crdt.EncodeStateVectorV1(d)
}

// GetText returns the plain-text content of the named YText root ("" after Close
// or for an absent/empty root).
func (m *Doc) GetText(name string) string {
	d := m.inner()
	if d == nil {
		return ""
	}
	return d.GetText(name).ToString()
}

// GetTextJSON returns the named YText root as a Yjs delta JSON document.
func (m *Doc) GetTextJSON(name string) ([]byte, error) {
	d := m.inner()
	if d == nil {
		return nil, ErrClosed
	}
	return d.GetText(name).ToJSON()
}

// GetMapJSON returns the named YMap root as JSON.
func (m *Doc) GetMapJSON(name string) ([]byte, error) {
	d := m.inner()
	if d == nil {
		return nil, ErrClosed
	}
	return d.GetMap(name).ToJSON()
}

// GetArrayJSON returns the named YArray root as JSON.
func (m *Doc) GetArrayJSON(name string) ([]byte, error) {
	d := m.inner()
	if d == nil {
		return nil, ErrClosed
	}
	return d.GetArray(name).ToJSON()
}

// EncodeDiff returns the updates this document has that the remote (described by
// its encoded state vector) is missing. Returns ErrClosed after Close.
func (m *Doc) EncodeDiff(remoteStateVector []byte) ([]byte, error) {
	d := m.inner()
	if d == nil {
		return nil, ErrClosed
	}
	sv, err := crdt.DecodeStateVectorV1(remoteStateVector)
	if err != nil {
		return nil, err
	}
	return crdt.EncodeStateAsUpdateV1(d, sv), nil
}
