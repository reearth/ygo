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

// rawType is a placeholder sharedType created during update decoding when the
// concrete type (YArray, YMap, YText, …) is not yet known. It is upgraded
// transparently the first time the user calls GetArray/GetMap/GetText/etc.
type rawType struct {
	abstractType
}

func (r *rawType) baseType() *abstractType                    { return &r.abstractType }
func (r *rawType) fire(_ *Transaction, _ map[string]struct{}) {}

// getOrCreateType returns the abstractType for a named root type, creating a
// rawType placeholder if none exists yet. Must be called with d.mu already held
// (i.e., from within a Transact callback or another locked helper).
func (d *Doc) getOrCreateType(name string) *abstractType {
	if t, ok := d.share[name]; ok {
		return t.baseType()
	}
	r := &rawType{}
	r.doc = d
	r.itemMap = make(map[string]*Item)
	r.owner = r
	r.name = name
	d.share[name] = r
	return &r.abstractType
}

// upgradeRawType copies a rawType's abstractType into dst, rewires all item
// Parent pointers to dst, and stores dst in d.share[name].
// Must be called with d.mu held.
func upgradeRawType(raw *rawType, dst sharedType, name string, share map[string]sharedType) {
	at := dst.baseType()
	*at = raw.abstractType // copy all fields (doc, start, itemMap, length, item, name)
	at.owner = dst
	// Rewire every item's Parent pointer.
	for item := at.start; item != nil; item = item.Right {
		item.Parent = at
	}
	share[name] = dst
}

// GetArray returns the named root YArray, creating it if it does not exist.
func (d *Doc) GetArray(name string) *YArray {
	d.mu.Lock()
	defer d.mu.Unlock()
	if t, ok := d.share[name]; ok {
		if arr, ok := t.(*YArray); ok {
			return arr
		}
		if raw, ok := t.(*rawType); ok {
			arr := &YArray{}
			upgradeRawType(raw, arr, name, d.share)
			return arr
		}
	}
	arr := &YArray{}
	arr.doc = d
	arr.itemMap = make(map[string]*Item)
	arr.owner = arr
	arr.name = name
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
		if raw, ok := t.(*rawType); ok {
			m := &YMap{}
			upgradeRawType(raw, m, name, d.share)
			return m
		}
	}
	m := &YMap{}
	m.doc = d
	m.itemMap = make(map[string]*Item)
	m.owner = m
	m.name = name
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
		if raw, ok := t.(*rawType); ok {
			txt := &YText{}
			upgradeRawType(raw, txt, name, d.share)
			return txt
		}
	}
	txt := &YText{}
	txt.doc = d
	txt.itemMap = make(map[string]*Item)
	txt.owner = txt
	txt.name = name
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

	// Fire deep observers, propagating each change up the type tree.
	// Each modified type and all its ancestors receive deep observer callbacks.
	firedDeep := make(map[*abstractType]struct{})
	for t := range txn.changed {
		current := t
		for current != nil {
			if _, already := firedDeep[current]; already {
				break
			}
			firedDeep[current] = struct{}{}
			for _, fn := range current.deepObservers {
				fn(txn)
			}
			if current.item != nil {
				current = current.item.Parent
			} else {
				break
			}
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

// GetXmlFragment returns the named root YXmlFragment, creating it if it does
// not exist.
func (d *Doc) GetXmlFragment(name string) *YXmlFragment {
	d.mu.Lock()
	defer d.mu.Unlock()
	if t, ok := d.share[name]; ok {
		if f, ok := t.(*YXmlFragment); ok {
			return f
		}
		if raw, ok := t.(*rawType); ok {
			f := &YXmlFragment{}
			upgradeRawType(raw, f, name, d.share)
			return f
		}
	}
	f := &YXmlFragment{}
	f.doc = d
	f.itemMap = make(map[string]*Item)
	f.owner = f
	f.name = name
	d.share[name] = f
	return f
}

// StateVector returns the current state vector of the document.
func (d *Doc) StateVector() StateVector {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.store.StateVector()
}

// EncodeStateAsUpdate encodes the full document state as a V1 binary update.
func (d *Doc) EncodeStateAsUpdate() []byte {
	return EncodeStateAsUpdateV1(d, nil)
}

// ApplyUpdate decodes and integrates a V1 binary update into the document.
func (d *Doc) ApplyUpdate(update []byte) error {
	return ApplyUpdateV1(d, update, nil)
}

// Destroy detaches all observers and clears internal state, releasing
// references held by the document. After Destroy the document must not be used.
func (d *Doc) Destroy() {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.onUpdate = nil
	d.share = make(map[string]sharedType)
	d.store = newStructStore()
}
