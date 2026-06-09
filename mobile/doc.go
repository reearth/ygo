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
