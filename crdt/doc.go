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
	"context"
	"math/rand"
	"sync"
)

// DocOption configures a Doc at creation time.
type DocOption func(*Doc)

// WithClientID sets a fixed ClientID instead of generating a random one.
// Useful in tests and server-side scenarios where the ID must be deterministic.
func WithClientID(id ClientID) DocOption {
	return func(d *Doc) { d.clientID = id }
}

// WithGC controls whether deleted item content is freed at the end of each
// transaction. Default is true. Set to false to preserve history for snapshots.
func WithGC(gc bool) DocOption {
	return func(d *Doc) { d.gc = gc }
}

// updateSub pairs a unique subscription ID with its callback so that
// unsubscribe closures can find and remove the right entry even when
// callbacks are removed out-of-order.
type updateSub struct {
	id uint64
	fn func([]byte, any)
}

// transactionSub pairs a unique subscription ID with a post-transaction callback.
type transactionSub struct {
	id uint64
	fn func(*Transaction)
}

// Doc is the root of a Yjs collaborative document.
// All shared types (YArray, YMap, YText, …) live inside a Doc.
type Doc struct {
	clientID ClientID
	gc       bool

	store *StructStore
	share map[string]sharedType // named root types

	// mu guards all document state. Transact and observer registration hold the
	// write lock; read-only methods (Get, ToSlice, Keys, etc.) hold the read
	// lock. Read methods must NOT be called from inside a Transact callback —
	// Transact holds the write lock and a nested RLock would deadlock.
	mu sync.RWMutex

	// subIDGen is a monotonically increasing counter used to issue unique IDs
	// to each observer subscription, enabling correct out-of-order unsubscribe.
	subIDGen uint64

	// onUpdate observers fire after each committed transaction with the encoded
	// incremental V1 update bytes and the transaction origin.
	onUpdate []updateSub

	// onAfterTxn observers fire after each committed transaction with the full
	// Transaction, which carries beforeState, afterState, deleteSet and Local.
	// Used by UndoManager; also available to application code that needs
	// richer change metadata than the binary update alone provides.
	onAfterTxn []transactionSub
}

// ClientID returns the document's client identifier (read-only after creation).
func (d *Doc) ClientID() ClientID {
	return d.clientID
}

// New creates a new Doc with a randomly generated ClientID.
func New(opts ...DocOption) *Doc {
	d := &Doc{
		clientID: ClientID(rand.Uint32()), // uint32 keeps IDs within JS Number.MAX_SAFE_INTEGER
		gc:       true,
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

func (r *rawType) baseType() *abstractType { return &r.abstractType }
func (r *rawType) prepareFire(_ *Transaction, _ map[string]struct{}) func() {
	return nil // rawType has no observers
}

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
//
// Observers are intentionally fired OUTSIDE the document lock. This means:
//   - Observer callbacks may safely call back into any Doc method (Transact,
//     GetArray, ApplyUpdate, etc.) without deadlocking.
//   - The document may be modified by another goroutine between the time fn
//     returns and the time observers fire; observers should treat txn as a
//     snapshot of what changed, not a live view of the current state.
func (d *Doc) Transact(fn func(*Transaction), origin ...any) {
	var orig any
	if len(origin) > 0 {
		orig = origin[0]
	}

	// ── Phase 1: run the transaction body under the lock ─────────────────────
	d.mu.Lock()

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

	// Squash adjacent same-client ContentString runs before encoding so that
	// the incremental update sent to peers is already compact.
	// Note: squashing happens before per-type observers fire. Observers therefore
	// see merged runs rather than individual character items. This is intentional:
	// the YTextEvent API does not expose raw Items, and firing after squash
	// removes the need for a second lock cycle.
	squashRuns(txn)

	// Encode the incremental update and snapshot observer slices while still
	// holding the lock so we get a consistent view.
	var updateBytes []byte
	if len(d.onUpdate) > 0 {
		updateBytes = encodeV1Locked(d, txn.beforeState)
	}

	// Snapshot per-type observer closures while the write lock is held.
	// prepareFire copies each type's observer slice and builds the event struct,
	// so concurrent Observe/Unobserve calls (which also hold the write lock)
	// cannot race with the fire loop below (N-C1).
	fireFns := make([]func(), 0, len(txn.changed))
	for t, keys := range txn.changed {
		if t.owner != nil {
			if fn := t.owner.prepareFire(txn, keys); fn != nil {
				fireFns = append(fireFns, fn)
			}
		}
	}

	// Snapshot deep-observer chains.
	type deepEntry struct {
		fns []func(*Transaction)
	}
	firedDeep := make(map[*abstractType]struct{})
	var deepSnap []deepEntry
	for t := range txn.changed {
		current := t
		for current != nil {
			if _, already := firedDeep[current]; already {
				break
			}
			firedDeep[current] = struct{}{}
			if len(current.deepObservers) > 0 {
				fns := make([]func(*Transaction), len(current.deepObservers))
				for i, s := range current.deepObservers {
					fns[i] = s.fn
				}
				deepSnap = append(deepSnap, deepEntry{fns})
			}
			if current.item != nil {
				current = current.item.Parent
			} else {
				break
			}
		}
	}

	// Snapshot OnUpdate callbacks.
	onUpdateSnap := make([]func([]byte, any), len(d.onUpdate))
	for i, s := range d.onUpdate {
		onUpdateSnap[i] = s.fn
	}
	onAfterTxnSnap := make([]func(*Transaction), len(d.onAfterTxn))
	for i, s := range d.onAfterTxn {
		onAfterTxnSnap[i] = s.fn
	}

	d.mu.Unlock()
	// ── Phase 2: fire all observers OUTSIDE the lock ──────────────────────────

	for _, fn := range fireFns {
		fn()
	}

	for _, de := range deepSnap {
		for _, fn := range de.fns {
			fn(txn)
		}
	}

	for _, fn := range onUpdateSnap {
		fn(updateBytes, orig)
	}

	for _, fn := range onAfterTxnSnap {
		fn(txn)
	}
}

// OnUpdate registers a callback that fires after every committed transaction.
// The callback receives the incremental V1 update bytes for that transaction
// and the origin value passed to Transact. Returns an unsubscribe function.
//
// The unsubscribe function is safe to call concurrently and handles
// out-of-order unsubscription correctly (no index-capture bug).
func (d *Doc) OnUpdate(fn func(update []byte, origin any)) func() {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.subIDGen++
	id := d.subIDGen
	d.onUpdate = append(d.onUpdate, updateSub{id: id, fn: fn})
	return func() {
		d.mu.Lock()
		defer d.mu.Unlock()
		for i, s := range d.onUpdate {
			if s.id == id {
				d.onUpdate = append(d.onUpdate[:i], d.onUpdate[i+1:]...)
				return
			}
		}
	}
}

// OnAfterTransaction registers a callback that fires after every committed
// transaction, receiving the full Transaction object. This provides richer
// change metadata than OnUpdate (beforeState, afterState, deleteSet, Local
// flag) and is the hook used by UndoManager. Returns an unsubscribe function.
func (d *Doc) OnAfterTransaction(fn func(*Transaction)) func() {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.subIDGen++
	id := d.subIDGen
	d.onAfterTxn = append(d.onAfterTxn, transactionSub{id: id, fn: fn})
	return func() {
		d.mu.Lock()
		defer d.mu.Unlock()
		for i, s := range d.onAfterTxn {
			if s.id == id {
				d.onAfterTxn = append(d.onAfterTxn[:i], d.onAfterTxn[i+1:]...)
				return
			}
		}
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

// TransactContext is like Transact but returns immediately with ctx.Err() if
// the context is already cancelled before the transaction starts.
// This is useful when the caller needs a cancellation path (e.g. server
// shutdown) without changing call sites that use the bare Transact form.
func (d *Doc) TransactContext(ctx context.Context, fn func(*Transaction), origin ...any) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
	}
	d.Transact(fn, origin...)
	return ctx.Err() // nil if not cancelled, error if cancelled during txn
}

// Destroy detaches all observers and clears internal state, releasing
// references held by the document. After Destroy the document must not be used.
func (d *Doc) Destroy() {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.onUpdate = nil
	d.onAfterTxn = nil
	d.share = make(map[string]sharedType)
	d.store = newStructStore()
}
