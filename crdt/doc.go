// Package crdt implements the Yjs CRDT algorithm in pure Go.
//
// The central concept is the Item: a node in a per-type doubly-linked list
// that carries content and origin pointers enabling conflict-free merging (YATA).
//
// Start with Doc, which is the root of a collaborative document. Inside a
// transaction, resolve root types through the Transaction (the Doc-level
// accessors take the document lock Transact already holds and would
// deadlock; see issue #138):
//
//	doc := crdt.New()
//	doc.Transact(func(txn *crdt.Transaction) {
//	    txn.GetText("content").Insert(txn, 0, "Hello", nil)
//	})
//	update := doc.EncodeStateAsUpdate()
//
// Reference algorithm: https://github.com/yjs/yjs/blob/main/INTERNALS.md
//
// # Quick start
//
//	doc := crdt.New()
//	text := doc.GetText("body")
//	doc.Transact(func(txn *crdt.Transaction) {
//	    text.Insert(txn, 0, "hello", nil)
//	})
//	update := crdt.EncodeStateAsUpdateV1(doc, nil)
//
// See the Example* functions for canonical usage patterns.
//
// # Stability
//
// ygo follows semantic versioning. The v1.x public API is considered
// stable: new functionality lands as minor releases; bug fixes as patch
// releases; breaking changes are deferred to v2.
//
// # Origin Tags
//
// The Origin field on Transaction, ChangeEvent (awareness), and
// UndoManager.trackedOrigins uses type any for user-defined tags.
// Callers must type-assert on read. Origin is used for filtering
// in observers and undo-manager scope.
package crdt

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"maps"
	"sort"
	"sync"
	"sync/atomic"
)

// ErrSubdocAlreadyIntegrated is raised when a Doc already embedded as a
// subdocument is embedded again. Create a second Doc with the same guid instead.
var ErrSubdocAlreadyIntegrated = errors.New("ygo/crdt: subdocument already integrated; create a second instance with the same guid")

// ErrRootTypeMismatch is raised when a named root is accessed as a different
// shared type than the one already stored in the document.
var ErrRootTypeMismatch = errors.New("crdt: root type mismatch")

// DocOption configures a Doc at creation time.
type DocOption func(*Doc)

// WithClientID sets a fixed ClientID instead of generating a random one.
// Useful in tests and server-side scenarios where the ID must be deterministic.
// The document still takes a new ID when a remote update moves this client's
// clock (see ClientID).
// id must be at most 2^55-1, the largest client a V2 update can carry; a larger
// one panics here, since the document could not encode its own writes in V2
// and every decoder refuses them. Yjs client IDs fit in 53 bits.
func WithClientID(id ClientID) DocOption {
	if id > maxClient {
		// A programmer error: random and remote client IDs are always within it.
		panic(fmt.Sprintf("crdt: WithClientID(%d): a client ID must be at most 2^55-1", id))
	}
	return func(d *Doc) { d.setClientID(id) }
}

// WithGC controls whether deleted item content is freed at the end of each
// transaction. Default is true. Set to false to preserve history for snapshots.
func WithGC(gc bool) DocOption {
	return func(d *Doc) { d.gc = gc }
}

// WithGUID sets the document's subdocument identifier. When a Doc is embedded
// inside another Doc via ContentDoc, the GUID identifies it across peers.
func WithGUID(guid string) DocOption {
	return func(d *Doc) {
		checkUTF8("WithGUID", "guid", guid)
		d.guid = guid
	}
}

// WithAutoLoad marks a subdocument to be auto-loaded by remote peers. Default false.
func WithAutoLoad(v bool) DocOption { return func(d *Doc) { d.autoLoad = v } }

// WithShouldLoad controls whether a provider should sync this doc now. Default
// true; a decoded subdocument starts with its autoLoad value, as in Yjs.
func WithShouldLoad(v bool) DocOption { return func(d *Doc) { d.shouldLoad = v } }

// WithCollectionID sets an optional subdocument collection identifier.
func WithCollectionID(id string) DocOption {
	return func(d *Doc) {
		checkUTF8("WithCollectionID", "id", id)
		d.collectionID = id
	}
}

// defaultMaxPendingItems is the default cap on parked items and, separately,
// parked delete ranges (see WithMaxPendingItems).
const defaultMaxPendingItems = 100_000

// WithMaxPendingItems caps parked items and, separately, parked delete ranges
// (default 100,000 when n <= 0), refusing with ErrInvalidUpdate, before any
// change, an update that would park past either, so a peer cannot grow them
// without bound. A struct sent again merges into the queue as Yjs merges its
// pending update, and the cap counts what the park adds after the merge.
func WithMaxPendingItems(n int) DocOption {
	return func(d *Doc) { d.maxPendingItems = n }
}

// SetMaxPendingItems sets the WithMaxPendingItems cap after construction, so
// stored state that exceeds it can load uncapped first.
func (d *Doc) SetMaxPendingItems(n int) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.maxPendingItems = n
}

// WithIntegrateScanBudget bounds the items the conflict scans of one remote
// apply (ApplyUpdate and its V1 and V2 forms) may pass, by origin, so a server
// can bound a connected client's updates and not those it relays from another
// node or loads from storage: budget gets the apply's origin before the apply
// takes the document's lock, so applies on several goroutines may call it at
// once, and zero or negative sets no bound.
//
// An apply that would pass the bound aborts partway with
// ErrIntegrateScanBudget, and the drain of the parked queue that ends an
// apply, past what the apply left of the bound, parks the rest without failing
// the apply, for a later apply, from any sender, to try again when its drain
// has more of its bound left than the scan that tripped had, or no bound; each
// keeps what it integrated, or nothing on a document with
// WithWholeOrNothingApplies.
func WithIntegrateScanBudget(budget func(origin any) int) DocOption {
	return func(d *Doc) { d.integrateScanBudget = budget }
}

// WithWholeOrNothingApplies, when on, rolls back before it commits a remote
// apply that fails, by tripping the scan bound or for any other reason, so the
// document is left as it was, no observer fires and ApplyUpdateV1Landed
// reports nothing, while a drain of the parked queue that trips what the apply
// left of the bound is rolled back alone and the apply stands. Such a document
// refuses every remote update that carries a move or a subdocument, and each
// remote apply on it allocates at least 288 bytes to record what it changes.
func WithWholeOrNothingApplies(on bool) DocOption {
	return func(d *Doc) { d.wholeOrNothing = on }
}

// ApplyBudget is what one remote apply may decode and add. A field of zero or
// below is no limit in that measure, as WithIntegrateScanBudget reads its
// bound, so the zero value bounds nothing. The smallest limit is 1, and a
// budget cannot refuse every value, struct, byte or split: a caller that
// wants an origin's updates refused whole refuses them before the apply.
type ApplyBudget struct {
	// Decoded is the values the apply may read: each struct, each delete
	// range, each lib0 Any value at any depth, each entry of an Any map and
	// each JSON value and member. It is counted as the update is read,
	// before the apply allocates for a length the update declares, so it
	// bounds the memory one update can take while it is decoded.
	Decoded int
	// Structs is the structs of the update the apply may add to the
	// document, a struct that parks behind a clock the document lacks
	// included. The structs a delete makes by splitting items the document
	// holds are not among them, so Structs never refuses a delete.
	Structs int
	// Bytes is the bytes of content those structs may carry: text, binary,
	// decoded values, a format's key and value, an XML element's or hook's
	// name, and the map key a struct is set under; and the name of each root
	// the update creates. A deleted range carries none, whatever its span.
	Bytes int
	// Split is the structs the apply may make by splitting items the
	// document holds, which Structs does not count: a delete of a range
	// inside a run cuts it into three. The splits the drain of the parked
	// queue makes count against the apply that fills the gap, and a drain
	// that would pass it stops and parks the rest without failing the apply.
	// Split: 1 allows one split and refuses the second.
	Split int
}

// WithApplyBudget bounds each remote apply (ApplyUpdate and its V1 and V2
// forms) by the ApplyBudget that budget returns for the apply's origin. budget
// runs once per apply, under the document's lock, so it must not call the
// document; a nil budget bounds nothing, as a document without the option.
//
// An apply that would pass its budget stops at the value or the struct that
// passes it, before that struct enters the document, and returns
// ErrApplyBudget in a RefusedUpdateError. As for the scan bound
// (WithIntegrateScanBudget), the document keeps what the apply integrated
// before it stopped, which ApplyUpdateV1Landed reports, and keeps nothing on
// a document that also has WithWholeOrNothingApplies. A struct of the update
// that parks behind a clock the document lacks counts when it parks, and a
// struct the drain of the parked queue integrates does not count again
// against the apply that fills its gap. Local transactions are never
// bounded. A budget that limits nothing changes nothing an apply does, and
// every failed remote apply on the document returns a RefusedUpdateError.
func WithApplyBudget(budget func(origin any) ApplyBudget) DocOption {
	return func(d *Doc) { d.applyBudget = budget }
}

// SetApplyBudget sets the budget of WithApplyBudget on a document already
// made, or with nil removes it, for an owner that loads trusted state first.
// It takes the document's lock.
func (d *Doc) SetApplyBudget(budget func(origin any) ApplyBudget) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.applyBudget = budget
}

// WithAllowedRoots names the root types a remote apply may write under: roots
// runs once per apply with the apply's origin, under the document's lock, so
// it must not call the document. A nil roots, or an answer of no names,
// allows every root, as a document without the option.
//
// An apply that would create a root the answer does not name, or add to or
// delete under one, at any depth of nested types, or add under no root of
// the document, stops before that struct enters the document or that item is
// deleted, and returns ErrRootNotAllowed in a RefusedUpdateError whose Root
// is the root's name. The document keeps what the apply integrated before it
// stopped, and keeps nothing on a document that also has
// WithWholeOrNothingApplies. An update that names a root the document holds
// and writes nothing under it, as a peer handing back the whole state does,
// is applied. A struct that parks behind a clock the document lacks is checked
// when it parks, and one whose parent is not known yet keeps the answer of
// the apply that parked it. A parked struct that the drain integrates under a
// root either answer does not name is dropped. The apply that filled its gap
// does not fail for it, and PendingStats.RootsDropped counts the drop for the
// struct's client, unless that apply has the origin of the one that parked
// the struct, which then returns ErrRootNotAllowed. Every failed remote apply
// on the document returns a RefusedUpdateError.
func WithAllowedRoots(roots func(origin any) []string) DocOption {
	return func(d *Doc) { d.allowedRoots = roots }
}

// SetAllowedRoots sets the list of WithAllowedRoots on a document already
// made, or with nil removes it. It takes the document's lock.
func (d *Doc) SetAllowedRoots(roots func(origin any) []string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.allowedRoots = roots
}

// WithRefusedContent names the kinds of content a remote apply may not
// bring into the document: kinds runs once per apply with the apply's origin,
// under the document's lock, so it must not call the document. A nil kinds,
// or an answer of no kinds, takes every kind, as a document without the
// option.
//
// It is for an owner whose readers cannot show every kind of content: one
// embed is one struct of a few bytes, inside any budget, and once a document
// holds it every reader is shown content its schema has no node for.
//
// A struct of a kind the answer names refuses the whole update, wherever in
// the update it sits, with ErrContentNotAllowed in a RefusedUpdateError.
// The check reads each struct's content tag as the apply decodes the update,
// and every struct of an update is decoded before one integrates, so a
// refused update leaves the document as it was and tells no observer, with
// or without WithWholeOrNothingApplies. A struct that parks was checked by
// the apply that decoded it, so the drain that later integrates it does not
// check it again. Naming ContentKindDoc or ContentKindMove refuses remote
// subdocuments or moves; the option refuses neither by itself. An update
// that carries none of the named kinds applies as it would without the
// option, and every failed remote apply on the document returns a
// RefusedUpdateError.
//
// The cost is the call of kinds per apply and, for each struct the update
// carries, a scan of its answer, besides the per-apply count that every
// RefusedUpdateError reports.
func WithRefusedContent(kinds func(origin any) []ContentKind) DocOption {
	return func(d *Doc) { d.refusedContent = kinds }
}

// SetRefusedContent sets the kinds of WithRefusedContent on a document
// already made, or with nil removes them. It takes the document's lock.
func (d *Doc) SetRefusedContent(kinds func(origin any) []ContentKind) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.refusedContent = kinds
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

// SubdocsEvent reports subdocument lifecycle changes for one transaction. The
// slices are freshly allocated per fire; do not retain or mutate them.
type SubdocsEvent struct {
	Added   []*Doc
	Removed []*Doc
	Loaded  []*Doc
}

// subdocsSub pairs a unique subscription ID with an OnSubdocs callback.
type subdocsSub struct {
	id uint64
	fn func(SubdocsEvent)
}

// Doc is the root of a Yjs collaborative document.
// All shared types (YArray, YMap, YText, …) live inside a Doc.
type Doc struct {
	clientID       ClientID      // read and written under mu, or before the doc is shared
	sharedClientID atomic.Uint64 // clientID's copy for ClientID, read without mu
	gc             bool
	guid           string // guid identifies this document (for subdocument embedding). Defaults to a random uuidv4 (see New) unless set with WithGUID.
	// linkMu guards shouldLoad, collectionID, item, parent and detached. It is a leaf lock:
	// no other lock is taken while it is held, so a parent's transaction can
	// take it, also for a document embedded in itself.
	linkMu          sync.Mutex
	shouldLoad      bool
	autoLoad        bool
	wholeOrNothing  bool // see WithWholeOrNothingApplies
	collectionID    string
	meta            any   // subdocument meta from Yjs opts, written back unchanged
	item            *Item // set on integrate when this Doc is embedded as a subdocument (#63)
	parent          *Doc  // document that owns item
	detached        bool  // true once an embedded Doc is removed or destroyed
	subdocs         map[string]*Doc
	onSubdocs       []subdocsSub
	maxPendingItems int // 0 = use defaultMaxPendingItems; see WithMaxPendingItems and #46
	// integrateScanBudget is the bound of WithIntegrateScanBudget, or nil.
	integrateScanBudget func(origin any) int
	// applying is the journal of the remote apply in progress on a document
	// with whole-or-nothing applies, or nil.
	applying *applyJournal
	// applyBudget is the budget of WithApplyBudget, allowedRoots the list of
	// WithAllowedRoots and refusedContent the kinds of WithRefusedContent,
	// each nil for none; counting is the tally of the remote apply in
	// progress on a document with any of them, or nil.
	applyBudget    func(origin any) ApplyBudget
	allowedRoots   func(origin any) []string
	refusedContent func(origin any) []ContentKind
	counting       *applyTally
	// parkedBy is what the document keeps of the apply that parked each
	// parked struct, on a document with a budget or a roots list, and nil
	// when nothing is parked; rootsDropped counts, by client, the parked
	// structs a drain dropped under a root another apply's list leaves out.
	parkedBy     map[*Item]parkedFrom
	rootsDropped map[ClientID]int

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

	// undoManagerCount tracks how many UndoManagers are currently attached to
	// this doc. While count > 0, transaction-commit auto-GC (#78 H1) is
	// suppressed because UndoManager.applyStackItem must be able to flip
	// Deleted=false on items it captured — which requires their original
	// Content to still be present. Mutated under d.mu.
	undoManagerCount int
}

// ClientID returns the document's client identifier. A remote transaction
// that changes this document's own client's clock gives it a new random one
// when it commits, as Yjs does.
func (d *Doc) ClientID() ClientID {
	return ClientID(d.sharedClientID.Load())
}

// setClientID sets the document's client identifier; called under mu once the
// doc is shared.
func (d *Doc) setClientID(id ClientID) {
	d.clientID = id
	d.sharedClientID.Store(uint64(id))
}

// GUID returns the document's identifier. Every Doc has one — a random uuidv4 by default (Yjs parity), or the value passed to WithGUID.
func (d *Doc) GUID() string {
	return d.guid
}

// AutoLoad returns whether this subdocument should be auto-loaded by remote peers.
func (d *Doc) AutoLoad() bool {
	return d.autoLoad
}

// ShouldLoad returns whether a provider should sync this doc now.
func (d *Doc) ShouldLoad() bool {
	d.linkMu.Lock()
	defer d.linkMu.Unlock()
	return d.shouldLoad
}

// CollectionID returns the optional subdocument collection identifier. A
// subdocument without one takes its parent's when it is added, as in Yjs.
func (d *Doc) CollectionID() string {
	d.linkMu.Lock()
	defer d.linkMu.Unlock()
	return d.collectionID
}

// inheritCollectionID gives d the collection ID id when d has none, as Yjs
// cleanupTransactions does for each added subdocument.
func (d *Doc) inheritCollectionID(id string) {
	d.linkMu.Lock()
	defer d.linkMu.Unlock()
	if d.collectionID == "" {
		d.collectionID = id
	}
}

// Load signals that this subdocument's data should be synced. On a subdocument
// it sets shouldLoad=true and emits a subdocs "loaded" event on the parent; on a
// root doc it just sets shouldLoad=true. Safe to call multiple times.
//
// On an embedded subdocument, Load opens a transaction on the PARENT document,
// so it MUST NOT be called from inside the parent's Transact closure
// (re-entrant lock — same footgun as GetText-in-Transact).
func (d *Doc) Load() {
	d.linkMu.Lock()
	if d.shouldLoad {
		d.linkMu.Unlock()
		return
	}
	d.shouldLoad = true
	item, parent := d.item, d.parent
	d.linkMu.Unlock()
	if item == nil {
		return
	}
	parent.Transact(func(txn *Transaction) {
		// item can be deleted after Load copied it under linkMu and before this
		// parent transaction starts. Do not report a loaded event for that
		// removed item. A detached subdocument clears d.item and returns above.
		if item.Deleted {
			return
		}
		txn.addSubdocLoaded(d)
	})
}

// attachSubdoc records the item and parent together under linkMu and returns
// the load request that integration must report.
func (d *Doc) attachSubdoc(item *Item, parent *Doc) bool {
	d.linkMu.Lock()
	defer d.linkMu.Unlock()
	if d.item != nil || d.detached {
		panic(ErrSubdocAlreadyIntegrated)
	}
	d.item, d.parent = item, parent
	return d.shouldLoad
}

// detachSubdoc drops links that would otherwise retain the former parent.
func (d *Doc) detachSubdoc(item *Item) {
	d.linkMu.Lock()
	defer d.linkMu.Unlock()
	if d.item == item {
		d.item, d.parent, d.detached = nil, nil, true
	}
}

// reattachSubdoc gives d back the item and parent detachSubdoc dropped, for
// item, alive again after the rollback of the apply that deleted it.
func (d *Doc) reattachSubdoc(item *Item, parent *Doc) {
	d.linkMu.Lock()
	defer d.linkMu.Unlock()
	if d.item == nil && d.detached {
		d.item, d.parent, d.detached = item, parent, false
	}
}

// maxPendingItemsLimit returns the effective cap on the pending queue depth.
func (d *Doc) maxPendingItemsLimit() int {
	if d.maxPendingItems <= 0 {
		return defaultMaxPendingItems
	}
	return d.maxPendingItems
}

// scanBudget is the bound WithIntegrateScanBudget sets for a remote apply with
// origin, or 0 for none.
func (d *Doc) scanBudget(origin any) int {
	if d.integrateScanBudget == nil {
		return 0
	}
	return d.integrateScanBudget(origin)
}

// callApplyBudget calls the WithApplyBudget callback, tagging a panic it
// raises with the option's name; the caller holds d.mu.
func (d *Doc) callApplyBudget(origin any) ApplyBudget {
	defer func() {
		if r := recover(); r != nil {
			panic(budgetCallbackPanic{"WithApplyBudget", r})
		}
	}()
	return d.applyBudget(origin)
}

// callAllowedRoots calls the WithAllowedRoots callback, tagging a panic it
// raises with the option's name; the caller holds d.mu.
func (d *Doc) callAllowedRoots(origin any) []string {
	defer func() {
		if r := recover(); r != nil {
			panic(budgetCallbackPanic{"WithAllowedRoots", r})
		}
	}()
	return d.allowedRoots(origin)
}

// callRefusedContent calls the WithRefusedContent callback, tagging a panic
// it raises with the option's name; the caller holds d.mu.
func (d *Doc) callRefusedContent(origin any) []ContentKind {
	defer func() {
		if r := recover(); r != nil {
			panic(budgetCallbackPanic{"WithRefusedContent", r})
		}
	}()
	return d.refusedContent(origin)
}

// uuidV4 returns a random RFC-4122 v4 UUID string. Used as the default document
// guid so every Doc has a stable unique id for subdocument embedding (Yjs
// parity: Yjs defaults guid to uuidv4()). No external dependency.
func uuidV4() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(fmt.Errorf("crypto/rand failed: %w", err))
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// NewClientID generates a fresh ClientID via crypto/rand.
//
// The 32-bit space matches Yjs JS upstream (which uses uint32 to stay within
// JavaScript's Number.MAX_SAFE_INTEGER). With uniform random distribution, the
// birthday-bound for collisions is around 65k peers — beyond that, collisions
// become probable. For multi-tenant deployments, callers may want to coordinate
// IDs externally; the random generation here is a collision-avoidance
// heuristic, not an authentication primitive. See SECURITY.md.
func NewClientID() ClientID {
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand.Read returning error is unrecoverable — process is broken.
		panic(fmt.Errorf("crypto/rand failed: %w", err))
	}
	return ClientID(binary.BigEndian.Uint32(b[:]))
}

// New creates a new Doc with a randomly generated ClientID.
func New(opts ...DocOption) *Doc {
	d := &Doc{
		gc:         true,
		guid:       uuidV4(),
		shouldLoad: true,
		store:      newStructStore(),
		share:      make(map[string]sharedType),
		subdocs:    make(map[string]*Doc),
	}
	d.setClientID(NewClientID())
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
	if d.applying != nil {
		d.applying.named = append(d.applying.named, name)
	}
	return &r.abstractType
}

// upgradeRawType moves raw's state into dst, stores dst in d.share[name], and
// repoints every struct parented to raw: integrated ones all sit on its list,
// and parked ones would otherwise integrate into the discarded placeholder.
// Must be called with d.mu held.
func (d *Doc) upgradeRawType(raw *rawType, dst sharedType, name string) {
	at := dst.baseType()
	*at = raw.abstractType // copy all fields (doc, start, itemMap, length, item, name)
	at.owner = dst
	at.cleanFormatting = false
	for item := at.start; item != nil; item = item.Right {
		item.Parent = at
	}
	if d.store.pending != nil {
		for _, item := range d.store.pending.items {
			if item != nil && item.Parent == &raw.abstractType {
				item.Parent = at
			}
		}
	}
	d.share[name] = dst
}

// panicRootTypeMismatch keeps a named root's concrete shared type stable. A
// root accessor with a different type must not replace the root and discard
// its contents.
func panicRootTypeMismatch(name string, existing sharedType, requested string) {
	panic(fmt.Errorf("%w: root %q has type %T; cannot access it as %s", ErrRootTypeMismatch, name, existing, requested))
}

// getArrayLocked is the lock-free body of GetArray. Callers must hold d.mu —
// either Doc.GetArray (takes it) or Transaction.GetArray (Transact holds it).
func (d *Doc) getArrayLocked(name string) *YArray {
	if t, ok := d.share[name]; ok {
		if arr, ok := t.(*YArray); ok {
			return arr
		}
		if raw, ok := t.(*rawType); ok {
			arr := &YArray{}
			d.upgradeRawType(raw, arr, name)
			return arr
		}
		panicRootTypeMismatch(name, t, "*crdt.YArray")
	}
	arr := &YArray{}
	arr.doc = d
	arr.itemMap = make(map[string]*Item)
	arr.owner = arr
	arr.name = name
	d.share[name] = arr
	return arr
}

// GetArray returns the named root YArray, creating it if it does not exist.
// It panics if that name already belongs to a different root type.
// It takes the document lock — inside a Transact callback use txn.GetArray
// instead (see Transact and GetText for the locking contract; issue #138).
func (d *Doc) GetArray(name string) *YArray {
	checkUTF8("Doc.GetArray", "name", name)
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.getArrayLocked(name)
}

// getMapLocked is the lock-free body of GetMap. Callers must hold d.mu —
// either Doc.GetMap (takes it) or Transaction.GetMap (Transact holds it).
func (d *Doc) getMapLocked(name string) *YMap {
	if t, ok := d.share[name]; ok {
		if m, ok := t.(*YMap); ok {
			return m
		}
		if raw, ok := t.(*rawType); ok {
			m := &YMap{}
			d.upgradeRawType(raw, m, name)
			return m
		}
		panicRootTypeMismatch(name, t, "*crdt.YMap")
	}
	m := &YMap{}
	m.doc = d
	m.itemMap = make(map[string]*Item)
	m.owner = m
	m.name = name
	d.share[name] = m
	return m
}

// GetMap returns the named root YMap, creating it if it does not exist.
// It panics if that name already belongs to a different root type.
// It takes the document lock — inside a Transact callback use txn.GetMap
// instead (see Transact and GetText for the locking contract; issue #138).
func (d *Doc) GetMap(name string) *YMap {
	checkUTF8("Doc.GetMap", "name", name)
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.getMapLocked(name)
}

// getTextLocked is the lock-free body of GetText. Callers must hold d.mu —
// either Doc.GetText (takes it) or Transaction.GetText (Transact holds it).
func (d *Doc) getTextLocked(name string) *YText {
	if t, ok := d.share[name]; ok {
		if txt, ok := t.(*YText); ok {
			return txt
		}
		if raw, ok := t.(*rawType); ok {
			txt := &YText{}
			d.upgradeRawType(raw, txt, name)
			return txt
		}
		panicRootTypeMismatch(name, t, "*crdt.YText")
	}
	txt := &YText{}
	txt.doc = d
	txt.itemMap = make(map[string]*Item)
	txt.owner = txt
	txt.name = name
	d.share[name] = txt
	return txt
}

// GetText returns the named root YText, creating it if it does not exist.
// It panics if that name already belongs to a different root type.
//
// GetText takes the document lock and therefore MUST NOT be called from
// inside a Transact callback — that self-deadlocks a non-reentrant lock,
// silently (issue #138). Inside a transaction use txn.GetText, or resolve
// the handle first:
//
//	txt := doc.GetText("t")
//	doc.Transact(func(txn *crdt.Transaction) { txt.Insert(txn, 0, "hi", nil) })
func (d *Doc) GetText(name string) *YText {
	checkUTF8("Doc.GetText", "name", name)
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.getTextLocked(name)
}

// buildPhase2 runs under d.mu (called from Transact while the lock is held)
// and returns a closure that fires all observers in the correct order.
// The closure must be invoked OUTSIDE d.mu — observers may re-enter Doc
// methods that acquire d.mu, which would deadlock under the lock.
//
// Returns nil when there is nothing to fire: no observers, a refused remote
// apply (Transaction.refused), or one whole-or-nothing rolled back
// (Transaction.rolledBack); either left the document as it was.
func buildPhase2(d *Doc, txn *Transaction) func() {
	if txn.refused || txn.rolledBack {
		return nil
	}
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
	txn.typeObserved = len(fireFns) > 0

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

	// #63 — reconcile the subdoc registry and build the event under the lock.
	// This runs ABOVE the early-return below so GetSubdocs()/GetSubdocGUIDs()
	// reflect adds/removes even when there are zero OnSubdocs observers.
	hasSubdocs := len(txn.subdocsAdded)+len(txn.subdocsRemoved)+len(txn.subdocsLoaded) > 0
	var subdocsEv SubdocsEvent
	if hasSubdocs {
		parentCollectionID := d.CollectionID()
		for sd := range txn.subdocsAdded {
			sd.inheritCollectionID(parentCollectionID)
			d.subdocs[sd.guid] = sd
			subdocsEv.Added = append(subdocsEv.Added, sd)
		}
		for sd := range txn.subdocsRemoved {
			delete(d.subdocs, sd.guid)
			subdocsEv.Removed = append(subdocsEv.Removed, sd)
		}
		for sd := range txn.subdocsLoaded {
			subdocsEv.Loaded = append(subdocsEv.Loaded, sd)
		}
	}
	onSubdocsSnap := make([]func(SubdocsEvent), len(d.onSubdocs))
	for i, s := range d.onSubdocs {
		onSubdocsSnap[i] = s.fn
	}

	if len(fireFns) == 0 && len(deepSnap) == 0 && len(onUpdateSnap) == 0 &&
		len(onAfterTxnSnap) == 0 && (!hasSubdocs || len(onSubdocsSnap) == 0) {
		return nil
	}

	return func() {
		for _, fn := range fireFns {
			fn()
		}
		if txn.afterTypeObservers != nil {
			txn.afterTypeObservers()
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
		if hasSubdocs {
			for _, fn := range onSubdocsSnap {
				fn(subdocsEv)
			}
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
// Observers fire, unless a remote apply was refused (Transaction.refused),
// before fn's error is returned.
//
// See docs/superpowers/specs/2026-04-21-transact-panic-safety-design.md
// for the full rationale on the defer/recover structure.
func (d *Doc) transactInternal(ctx context.Context, fn func(*Transaction) error, origin ...any) (retErr error) {
	var orig any
	if len(origin) > 0 {
		orig = origin[0]
	}

	d.mu.Lock()
	// A parking apply records only what this transaction parks.
	if d.store.parked != nil {
		*d.store.parked = transactionParked{}
	}

	txn := &Transaction{
		doc:         d,
		Origin:      orig,
		Local:       true,
		deleteSet:   newOrderedDeleteSet(),
		beforeState: d.store.StateVector(),
		// Pre-size changed to common-case capacity (#54 A): most transactions
		// touch 1-3 types, and the zero-hint alloc forces immediate rehashing
		// on the very first append. newItems is left nil — pre-sizing it here
		// added 1 alloc per txn even when no ContentString was inserted, which
		// hurt array/map-only workloads more than it helped text workloads
		// (the squashRuns target).
		changed: make(map[*abstractType]map[string]struct{}, 4),
		ctx:     ctx,
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

		// Yjs cleans up the format markers a remote change left redundant
		// after the change's observers, in a follow-up local transaction,
		// and GCs the change only then. A type observer may give a text its
		// first marker, so with type observers to fire the gate waits.
		deferGC := false
		if r == nil && !txn.Local {
			if needsFormattingCleanup(txn) {
				txn.formatCleanup, deferGC = true, true
			} else if txn.typeObserved && changedText(txn) {
				txn.afterTypeObservers = func() {
					d.mu.Lock()
					txn.formatCleanup = needsFormattingCleanup(txn)
					d.mu.Unlock()
				}
				deferGC = true
			}
		}

		// #78 H1 — Auto-GC at transaction commit. Runs AFTER buildPhase2 so
		// the observer Deltas have already been computed against the original
		// content; runs BEFORE Unlock so other goroutines never see partially-
		// GC'd state. No-op when doc.gc is false.
		//
		// When an UndoManager is attached we skip auto-GC: undoing a deletion
		// re-inserts a COPY of the deleted item's content (applyStackItem →
		// redoItem), which only works if that original content is still present.
		// Yjs handles this with a per-item keep flag; we take the conservative
		// position of disabling auto-GC entirely while any UndoManager is
		// registered. RunGC remains available as the explicit manual entry point.
		if d.gc && d.undoManagerCount == 0 && !deferGC {
			gcTxnDeleteSet(d, txn)
		}

		// The document's park record drops this transaction's views of the queue.
		if d.store.parked != nil {
			*d.store.parked = transactionParked{}
		}

		// A remote transaction that moved this document's own clock gives it a
		// new client ID, as Yjs cleanupTransactions does, so a local write never
		// continues a clock that another peer set.
		if !txn.Local && txn.afterState != nil && txn.afterState.Clock(d.clientID) != txn.beforeState.Clock(d.clientID) {
			d.setClientID(NewClientID())
		}

		// Mark the transaction committed BEFORE releasing the lock so the
		// txn-scoped accessors fail loudly from observers (which receive the
		// *Transaction after unlock) or from a retained txn, instead of
		// mutating d.share unsynchronized. See Transaction.done.
		txn.done = true
		d.mu.Unlock()

		if deferGC {
			// Runs even if an observer panics, as Yjs runs the cleanup.
			defer d.afterRemoteObservers(txn)
		}
		if phase2 != nil {
			phase2()
		}

		if r != nil {
			panic(r)
		}
	}()

	retErr = fn(txn)
	// An observer that keeps the *Transaction must not keep the scans' marks.
	txn.scanMarks = nil

	rearbitrateMoves(txn)
	txn.afterState = d.store.StateVector()

	squashRuns(txn)
	// #78 H2 — Re-merge transient splits. Must run after squashRuns so any
	// new-item run that squash-collapsed disappears from tryMergeWithLefts's
	// view (its left.Right == item check filters out absorbed items).
	tryMergeWithLefts(txn)
	return retErr
}

// Transact executes fn inside a transaction. All insertions and deletions made
// during fn are batched; observers fire once after fn returns.
//
// The optional origin value tags the transaction: observers receive it
// (OnUpdate's origin parameter, Transaction.Origin) and UndoManager's
// WithTrackedOrigins filters on it — always by Go interface equality (==).
// If you mint per-caller origin tokens as pointers, use a pointer to a
// NON-zero-size type (e.g. type token struct{ _ byte }): pointers to
// zero-size types may all share one address (runtime.zerobase) and compare
// equal, silently merging origins that were meant to be distinct — see
// WithTrackedOrigins' doc for the full trap (#203).
//
// Transact holds the document write lock for the duration of fn. Anything
// that takes that same non-reentrant lock MUST NOT be called from inside fn
// — it deadlocks silently, and Go exposes no goroutine identity with which
// to detect it (issue #138). That includes the Doc-level root accessors
// (GetMap, GetText, GetArray, GetXmlFragment), Doc.Load, observer
// registration (Observe, ObserveDeep, OnUpdate, OnAfterTransaction,
// OnSubdocs), and — notably — nested Transact on the same Doc, which
// remains an undetected deadlock.
//
// Inside fn, resolve root types through the Transaction instead — its
// accessors reuse the lock the transaction already holds — or resolve
// handles before the transaction:
//
//	doc.Transact(func(txn *crdt.Transaction) {
//	    txn.GetMap("m").Set(txn, "k", 1) // in-transaction accessor: works
//	})
//
//	m := doc.GetMap("m") // or: resolve outside...
//	doc.Transact(func(txn *crdt.Transaction) {
//	    m.Set(txn, "k", 1) // ...and use the handle inside
//	})
//
// (Observer callbacks are NOT affected — they fire after the lock is
// released and may call any Doc method; see below.)
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
// Callbacks that compare the origin (an echo guard, a per-user filter) do so
// with Go ==, so the caution on Transact's doc applies: pointer origin
// tokens must point at non-zero-size types, or distinct tokens may alias
// (runtime.zerobase, #203).
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

// OnSubdocs registers a callback fired once after any transaction that adds,
// removes, or loads subdocuments. Returns an unsubscribe function. Mirrors
// OnUpdate (safe concurrent, out-of-order unsubscribe).
func (d *Doc) OnSubdocs(fn func(SubdocsEvent)) func() {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.subIDGen++
	id := d.subIDGen
	d.onSubdocs = append(d.onSubdocs, subdocsSub{id: id, fn: fn})
	return func() {
		d.mu.Lock()
		defer d.mu.Unlock()
		for i, s := range d.onSubdocs {
			if s.id == id {
				d.onSubdocs = append(d.onSubdocs[:i], d.onSubdocs[i+1:]...)
				return
			}
		}
	}
}

// GetSubdocs returns the resident subdocuments, sorted by guid.
func (d *Doc) GetSubdocs() []*Doc {
	d.mu.RLock()
	defer d.mu.RUnlock()
	out := make([]*Doc, 0, len(d.subdocs))
	for _, sd := range d.subdocs {
		out = append(out, sd)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].guid < out[j].guid })
	return out
}

// GetSubdocGUIDs returns the guids of the resident subdocuments, sorted.
func (d *Doc) GetSubdocGUIDs() []string {
	d.mu.RLock()
	defer d.mu.RUnlock()
	out := make([]string, 0, len(d.subdocs))
	for g := range d.subdocs {
		out = append(out, g)
	}
	sort.Strings(out)
	return out
}

// getXmlFragmentLocked is the lock-free body of GetXmlFragment. Callers must
// hold d.mu — either Doc.GetXmlFragment (takes it) or
// Transaction.GetXmlFragment (Transact holds it).
func (d *Doc) getXmlFragmentLocked(name string) *YXmlFragment {
	if t, ok := d.share[name]; ok {
		if f, ok := t.(*YXmlFragment); ok {
			return f
		}
		if raw, ok := t.(*rawType); ok {
			f := &YXmlFragment{}
			d.upgradeRawType(raw, f, name)
			return f
		}
		panicRootTypeMismatch(name, t, "*crdt.YXmlFragment")
	}
	f := &YXmlFragment{}
	f.doc = d
	f.itemMap = make(map[string]*Item)
	f.owner = f
	f.name = name
	d.share[name] = f
	return f
}

// GetXmlFragment returns the named root YXmlFragment, creating it if it does
// not exist. It panics if that name already belongs to a different root type.
// It takes the document lock — inside a Transact callback use
// txn.GetXmlFragment instead (see Transact and GetText for the locking
// contract; issue #138).
func (d *Doc) GetXmlFragment(name string) *YXmlFragment {
	checkUTF8("Doc.GetXmlFragment", "name", name)
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.getXmlFragmentLocked(name)
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

// PendingStats is a snapshot of the doc's pending-queue depth. Returned by
// Doc.PendingStats. Used for observability — operators can monitor queue
// growth to detect adversarial peers or persistent convergence gaps. The
// snapshot is immediately stale; do not use as a synchronization primitive.
type PendingStats struct {
	// Items is the number of decoded items parked in the pending queue
	// (waiting for cross-update Origin/OriginRight dependencies or
	// same-client clock-gap dependencies to arrive).
	Items int

	// DeleteRanges is the total number of delete-set ranges in pendingDs
	// across all clients. Each range is one [clock, clock+len) span.
	DeleteRanges int

	// MissingFor lists the ClientIDs that pending items reference but
	// haven't yet received. Sorted ascending. Read-only.
	MissingFor []ClientID

	// RootsDropped counts, by the client that wrote them, the parked structs
	// that a drain dropped because they landed under a root the allowed-roots
	// list does not name (WithAllowedRoots), when the apply that filled their
	// gap was not the one that sent them: that apply does not fail for them.
	// It counts from the document's creation and is a copy.
	RootsDropped map[ClientID]int
}

// PendingStats returns a snapshot of the pending-queue state. Cheap; takes
// a read lock and copies a few values. Intended for operational monitoring
// of out-of-order delta convergence; see v1.2.0 release notes for the
// pending-structs machinery this exposes.
func (d *Doc) PendingStats() PendingStats {
	d.mu.RLock()
	defer d.mu.RUnlock()

	stats := PendingStats{}
	if d.store.pending != nil {
		stats.Items = d.store.pending.heldStructs()
		// missing is a StateVector (map[ClientID]uint64) — extract sorted keys
		missing := make([]ClientID, 0, len(d.store.pending.missing))
		for c := range d.store.pending.missing {
			missing = append(missing, c)
		}
		sort.Slice(missing, func(i, j int) bool { return missing[i] < missing[j] })
		stats.MissingFor = missing
	}
	stats.DeleteRanges = d.store.pendingDsRanges
	if len(d.rootsDropped) > 0 {
		stats.RootsDropped = maps.Clone(d.rootsDropped)
	}
	return stats
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
	d.linkMu.Lock()
	d.item, d.parent, d.detached = nil, nil, true
	d.linkMu.Unlock()
}
