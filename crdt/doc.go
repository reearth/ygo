// Package crdt implements the Yjs CRDT algorithm in pure Go.
//
// The central concept is the Item: a node in a per-type doubly-linked list
// that carries content and origin pointers enabling conflict-free merging (YATA).
//
// Start with Doc, which is the root of a collaborative document:
//
//	doc := crdt.New()
//	doc.Transact(func(txn *crdt.Transaction) {
//	    doc.GetText("content").Insert(txn, 0, "Hello", nil)
//	})
//	update := doc.EncodeStateAsUpdate()
//
// Reference algorithm: https://github.com/yjs/yjs/blob/main/INTERNALS.md
package crdt

import (
	"math/rand"
	"sync"
)

// DocOption configures a Doc at creation time.
type DocOption func(*Doc)

// WithClientID sets a fixed ClientID instead of generating a random one.
// Useful in tests and server-side scenarios where the ID must be deterministic.
func WithClientID(id ClientID) DocOption {
	return func(d *Doc) { d.ClientID = id }
}

// WithGC controls whether deleted item content is freed at the end of each
// transaction. Default is true. Set to false to preserve history for snapshots.
func WithGC(gc bool) DocOption {
	return func(d *Doc) { d.GC = gc }
}

// Doc is the root of a Yjs collaborative document.
// All shared types (YArray, YMap, YText, …) live inside a Doc.
type Doc struct {
	ClientID ClientID
	GC       bool

	store *StructStore
	share map[string]sharedType // named root types

	mu sync.Mutex

	// update observers — called after each committed transaction.
	onUpdate []func(origin any)
}

// New creates a new Doc with a randomly generated ClientID.
func New(opts ...DocOption) *Doc {
	d := &Doc{
		ClientID: ClientID(rand.Uint64()),
		GC:       true,
		store:    newStructStore(),
		share:    make(map[string]sharedType),
	}
	for _, opt := range opts {
		opt(d)
	}
	return d
}

// GetArray returns the named root YArray, creating it if it does not exist.
func (d *Doc) GetArray(name string) *YArray {
	d.mu.Lock()
	defer d.mu.Unlock()
	if t, ok := d.share[name]; ok {
		if arr, ok := t.(*YArray); ok {
			return arr
		}
	}
	arr := &YArray{}
	arr.abstractType.doc = d
	arr.abstractType.itemMap = make(map[string]*Item)
	arr.abstractType.owner = arr
	d.share[name] = arr
	return arr
}

// GetMap returns the named root YMap, creating it if it does not exist.
func (d *Doc) GetMap(name string) *YMap {
	d.mu.Lock()
	defer d.mu.Unlock()
	if t, ok := d.share[name]; ok {
		if m, ok := t.(*YMap); ok {
			return m
		}
	}
	m := &YMap{}
	m.abstractType.doc = d
	m.abstractType.itemMap = make(map[string]*Item)
	m.abstractType.owner = m
	d.share[name] = m
	return m
}

// GetText returns the named root YText, creating it if it does not exist.
func (d *Doc) GetText(name string) *YText {
	d.mu.Lock()
	defer d.mu.Unlock()
	if t, ok := d.share[name]; ok {
		if txt, ok := t.(*YText); ok {
			return txt
		}
	}
	txt := &YText{}
	txt.abstractType.doc = d
	txt.abstractType.itemMap = make(map[string]*Item)
	txt.abstractType.owner = txt
	d.share[name] = txt
	return txt
}


// Transact executes fn inside a transaction. All insertions and deletions made
// during fn are batched; observers fire once after fn returns.
func (d *Doc) Transact(fn func(*Transaction), origin ...any) {
	d.mu.Lock()
	defer d.mu.Unlock()

	var orig any
	if len(origin) > 0 {
		orig = origin[0]
	}

	txn := &Transaction{
		doc:         d,
		Origin:      orig,
		Local:       true,
		deleteSet:   newDeleteSet(),
		beforeState: d.store.StateVector(),
		changed:     make(map[*abstractType]map[string]struct{}),
	}

	fn(txn)

	txn.afterState = d.store.StateVector()

	// Fire per-type observers for each modified type.
	for t, keys := range txn.changed {
		if t.owner != nil {
			t.owner.fire(txn, keys)
		}
	}

	for _, fn := range d.onUpdate {
		fn(orig)
	}
}

// OnUpdate registers a callback that fires after every committed transaction.
// Returns an unsubscribe function.
func (d *Doc) OnUpdate(fn func(origin any)) func() {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.onUpdate = append(d.onUpdate, fn)
	idx := len(d.onUpdate) - 1
	return func() {
		d.mu.Lock()
		defer d.mu.Unlock()
		d.onUpdate = append(d.onUpdate[:idx], d.onUpdate[idx+1:]...)
	}
}

// StateVector returns the current state vector of the document.
func (d *Doc) StateVector() StateVector {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.store.StateVector()
}
