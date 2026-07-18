package mobile

import (
	"sync"

	"github.com/reearth/ygo/crdt"
)

// Doc is a gomobile-safe wrapper around *crdt.Doc.
type Doc struct {
	mu sync.RWMutex
	d  *crdt.Doc // nil after Close

	// subsMu guards the change-observer registry. It is DISTINCT from mu on
	// purpose: mu.RLock is shared, so mutating subs under it would data-race a
	// concurrent Observe. Never take subsMu while holding mu for writing and
	// vice-versa in a way that could invert lock order.
	subsMu sync.Mutex
	subs   map[*Subscription]struct{}
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
// Idempotent. After Close, methods return ErrClosed / zero values.
//
// Close nils d under the write lock FIRST, then detaches every change-observer
// subscription (unsubscribing the crdt bridge and signalling each drain
// goroutine to stop). Ordering matters: an in-flight Observe holds m.mu.RLock
// for its whole body and registers itself only near the end, so taking the
// write lock first guarantees every such Observe has finished registering
// before we snapshot m.subs (no missed subscription → no leaked drain
// goroutine). Any Observe starting after this sees d == nil and returns the
// closed stub without launching a drain.
//
// Close does not join drain goroutines: one may be inside OnChange re-entering
// a mobile method that needs m.mu, so joining under a lock would deadlock.
// s.Close never takes m.mu, so detaching after releasing the write lock keeps
// the signal-don't-join / no-lock-across-callback property. No callback starts
// after Close — the bridge is unsubscribed and each drain sees stopped.
func (m *Doc) Close() {
	m.mu.Lock()
	m.d = nil
	m.mu.Unlock()
	m.subsMu.Lock()
	subs := make([]*Subscription, 0, len(m.subs))
	for s := range m.subs {
		subs = append(subs, s)
	}
	m.subsMu.Unlock()
	for _, s := range subs {
		s.Close()
	}
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

// GetTextJSON returns the named YText root's formatted content as an idiomatic
// Yjs delta: a JSON array of ops shaped `[{"insert":...,"attributes":{...}}]`,
// where each op carries exactly one of insert/retain/delete plus optional
// attributes (a full-content read yields insert ops only). An absent/empty root
// returns `[]` (never `null`), so a JS consumer can iterate it unconditionally.
// Returns ErrClosed after Close.
func (m *Doc) GetTextJSON(name string) ([]byte, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if m.d == nil {
		return nil, ErrClosed
	}
	return deltaToIdiomaticJSON(m.d.GetText(name).ToDelta())
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
