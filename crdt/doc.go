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

// WithGUID sets the document's subdocument identifier. When a Doc is embedded
// inside another Doc via ContentDoc, the GUID identifies it across peers.
func WithGUID(guid string) DocOption {
	return func(d *Doc) { d.guid = guid }
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
	guid     string // subdocument identifier; empty for root docs

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

// GUID returns the document's subdocument identifier (empty for root docs).
func (d *Doc) GUID() string {
	return d.guid
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

// buildPhase2 runs under d.mu (called from Transact while the lock is held)
// and returns a closure that fires all observers in the correct order.
// The closure must be invoked OUTSIDE d.mu — observers may re-enter Doc
// methods that acquire d.mu, which would deadlock under the lock.
//
// Returns nil only if there is nothing to fire (no observers of any kind).
func buildPhase2(d *Doc, txn *Transaction) func() {
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

	if len(fireFns) == 0 && len(deepSnap) == 0 && len(onUpdateSnap) == 0 && len(onAfterTxnSnap) == 0 {
		return nil
	}

	return func() {
		for _, fn := range fireFns {
			fn()
		}
		for _, de := range deepSnap {
			for _, fn := range de.fns {
				fn(txn)
			}
		}
		for _, fn := range onUpdateSnap {
			fn(updateBytes, txn.Origin)
		}
		for _, fn := range onAfterTxnSnap {
			fn(txn)
		}
	}
}

// transactInternal is the shared transaction entry point that Transact,
// TransactContext, TransactE, and TransactContextE all delegate to. It
// acquires d.mu, runs fn under the lock with panic-safe cleanup, fires
// Phase 2 observers outside the lock, and re-raises any panic to the
// caller. The ctx parameter is stored on the Transaction struct and
// exposed to fn via Transaction.Ctx().
//
// fn's returned error is captured as the named return value retErr.
// Observers fire regardless of whether fn returned an error; the error
// is propagated to the caller only after the observers have fired.
//
// See docs/superpowers/specs/2026-04-21-transact-panic-safety-design.md
// for the full rationale on the defer/recover structure.
func (d *Doc) transactInternal(ctx context.Context, fn func(*Transaction) error, origin ...any) (retErr error) {
	var orig any
	if len(origin) > 0 {
		orig = origin[0]
	}

	d.mu.Lock()

	txn := &Transaction{
		doc:         d,
		Origin:      orig,
		Local:       true,
		deleteSet:   newDeleteSet(),
		beforeState: d.store.StateVector(),
		changed:     make(map[*abstractType]map[string]struct{}),
		ctx:         ctx,
	}

	defer func() {
		r := recover()

		if r != nil {
			func() {
				defer func() { _ = recover() }()
				if txn.afterState == nil {
					txn.afterState = d.store.StateVector()
				}
			}()
		}

		var phase2 func()
		if r != nil {
			func() {
				defer func() { _ = recover() }()
				phase2 = buildPhase2(d, txn)
			}()
		} else {
			phase2 = buildPhase2(d, txn)
		}

		d.mu.Unlock()

		if phase2 != nil {
			phase2()
		}

		if r != nil {
			panic(r)
		}
	}()

	retErr = fn(txn)

	txn.afterState = d.store.StateVector()

	squashRuns(txn)
	return retErr
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
//
// Panic semantics:
//   - If fn panics (or any Phase 1 work panics), d.mu is released via defer.
//   - Observers fire with whatever partial state was committed before the
//     panic: OnUpdate receives a V1 update describing the mutations that
//     completed (non-empty if fn mutated; minimal but well-formed if fn
//     panicked before mutating); per-type, deep, and OnAfterTransaction
//     observers fire for what was recorded in txn.changed.
//   - The original panic is re-raised to the caller after observers fire.
//   - Rollback is NOT supported. The in-memory doc reflects fn's partial
//     work. Callers who need atomicity must implement it above Transact.
//     This matches the behavior of Yjs JS and the Rust yrs implementation;
//     yrs explicitly directs users to UndoManager for transactional undo.
//   - If fn panics and an observer callback also panics during the partial
//     firing, the observer's panic reaches the caller and the original
//     fn panic value is lost.
func (d *Doc) Transact(fn func(*Transaction), origin ...any) {
	_ = d.transactInternal(context.Background(), func(t *Transaction) error {
		fn(t)
		return nil
	}, origin...)
}

// TransactE is like Transact but allows fn to return an error. The error is
// returned to the caller after all observers have fired. Mutations commit
// regardless of the error (no rollback — matches Yjs JS doc.transact(f) and
// the Rust yrs implementation).
//
//   - If fn returns nil, TransactE returns nil.
//   - If fn returns a non-nil error, that error becomes the return value of
//     TransactE. Mutations that fn completed before returning are committed.
//   - Observers (OnUpdate, OnAfterTransaction, per-type) fire BEFORE TransactE
//     returns the error — matching the Yjs JS "finally-block observer" pattern.
//   - If fn panics, the existing v1.1.1 panic-safety contract applies: partial
//     state commits, observers fire with partial changes, and the panic is
//     re-raised to the caller. TransactE does not swallow panics.
func (d *Doc) TransactE(fn func(*Transaction) error, origin ...any) error {
	return d.transactInternal(context.Background(), fn, origin...)
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

// TransactContext is like Transact but associates a context with the
// transaction so fn can cooperatively observe cancellation.
//
// If ctx is already cancelled when TransactContext is called, fn is not
// invoked and ctx.Err() is returned immediately.
//
// Inside fn, callers can poll txn.Ctx().Err() or <-txn.Ctx().Done() to
// detect cancellation and return early. Any mutations fn completed
// before returning are committed (no rollback, consistent with the
// Transact panic-safety contract — see Transact's godoc).
//
// If ctx cancels during fn and fn does not poll, fn runs to completion —
// Go has no safe mechanism for interrupting arbitrary fn code. ctx.Err()
// is returned after the transaction commits as a "missed cancellation"
// signal to the caller. It is not an error flag for the mutations; those
// are committed either way.
//
// Neither Yjs JS nor the Rust yrs implementation offers mid-fn
// interruption either; cooperative polling is the ecosystem norm.
func (d *Doc) TransactContext(ctx context.Context, fn func(*Transaction), origin ...any) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	_ = d.transactInternal(ctx, func(t *Transaction) error {
		fn(t)
		return nil
	}, origin...)
	return ctx.Err()
}

// TransactContextE is like TransactContext but allows fn to return an error.
// It combines context-cancellation awareness with fn-error propagation.
//
//   - If ctx is already cancelled when TransactContextE is called, fn is not
//     invoked and ctx.Err() is returned immediately.
//   - If fn returns nil and ctx is not cancelled after the transaction commits,
//     TransactContextE returns nil.
//   - If fn returns a non-nil error but ctx is still fine, that error is
//     returned.
//   - If ctx cancels during fn (cooperative only — Go cannot safely interrupt
//     arbitrary fn code), ctx.Err() wins over fn's error. This matches the
//     precedent set by TransactContext and Yjs JS's ecosystem norm of
//     cooperative cancellation polling via txn.Ctx().
//   - Mutations commit regardless of fn's error or ctx cancellation (no
//     rollback — matches Yjs JS doc.transact(f) and yrs semantics).
//   - Observers fire BEFORE TransactContextE returns, even when fn errors.
//   - Panics in fn follow the v1.1.1 panic-safety contract (partial commit,
//     observer fire, re-raise).
func (d *Doc) TransactContextE(ctx context.Context, fn func(*Transaction) error, origin ...any) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	fnErr := d.transactInternal(ctx, fn, origin...)
	if ctxErr := ctx.Err(); ctxErr != nil {
		return ctxErr
	}
	return fnErr
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
