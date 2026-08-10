package client

import (
	"context"

	"github.com/reearth/ygo/persistence"
	"github.com/reearth/ygo/persistence/sqlite"
)

// LocalStore is the client's local-durability contract: enough to survive a
// process restart without a server round-trip. It is deliberately shaped
// after provider/websocket.PersistenceAdapter (LoadDoc/StoreUpdate) rather
// than defining a new shape, so any adapter written for the server side
// (SQLite, or a future one) is reusable client-side with zero glue beyond
// this package, and so the sync loop added by later tasks in #165 can persist
// exactly like the server's persistence worker does.
//
// The client hydrates its in-memory Doc from LoadDoc before it ever dials a
// server, and calls StoreUpdate for every update applied to that Doc —
// local edits and server-received updates alike — the same "durable before it
// matters" ordering the server's PersistenceAdapter documents, applied to a
// client that may spend most of its life offline.
type LocalStore interface {
	// LoadDoc returns the full binary V1 update representing stored state for
	// room, or (nil, nil) if no state exists yet.
	LoadDoc(room string) ([]byte, error)
	// StoreUpdate is called with each incremental V1 update produced locally
	// (or received and applied from a peer). The store is responsible for
	// merging or appending updates as appropriate for its storage model.
	StoreUpdate(room string, update []byte) error
}

// CompactableStore is an optional extension to LocalStore, mirroring
// provider/websocket.CompactableAdapter: a store that accumulates an
// append-only update log can be asked to collapse it into a compact form.
//
// Client.maybeCompact calls Compact automatically, from the sync loop's own
// goroutine, once Options.CompactEvery (default 500 — see that field's own
// doc) successful stored updates have accumulated for a room; an
// implementer of this interface does not need to schedule Compact itself,
// only make it correct. (This corrects an earlier version of this doc,
// which said the opposite — that callers were expected to invoke Compact on
// their own schedule. That was true before Client.maybeCompact existed and
// is false now; #165 final whole-branch review, Important D.)
//
// The one case the automatic trigger cannot reach: a device that never
// makes it into the sync loop at all — every Connect attempt fails during
// hydration, or Connect is never called — accumulates an uncompacted log for
// as long as that lasts, since maybeCompact only ever runs from inside
// runLoop's own select (see that method's doc). This is an accepted
// trade-off (#165 Task 10 YAGNI), not something a caller needs to work
// around.
type CompactableStore interface {
	LocalStore
	// Compact collapses room's stored update log without changing the state
	// LoadDoc returns for it. Retention policy is the store's concern.
	Compact(ctx context.Context, room string) error
}

// defaultKeepVersions is the non-zero retention OpenSQLiteStore applies to
// the SQLiteStore.KeepVersions field it sets (forwarded to the
// persistence.LegacyAdapter this store wraps at Compact time — see
// SQLiteStore.Compact).
//
// LegacyAdapter.KeepVersions defaults to 0 ("keep all history") when built
// directly via persistence.NewLegacyAdapter, which is the right default for
// the SERVER side (provider/websocket): an operator opts into retention
// explicitly via CompactEvery + KeepVersions, because the raw update log
// there doubles as a room's audit/version history, and trimming it by
// default would be a worse surprise than the disk growth it saves. A
// client's SQLiteStore has no such use for that history — this package
// never exposes the wrapped store's ListVersions/MaterializeAt to a caller
// (see SQLiteStore's own doc for why that is now an enforced boundary, not
// an incidental fact) — so inheriting the SAME zero default here would make
// every Compact call this package ever makes (see Client.maybeCompact) a
// permanent, silent no-op: exactly the gap #165's Task 1 review flagged
// (TestSQLiteStore_CompactPreservesState passed trivially because nothing
// was ever actually deleted; see TestSQLiteStore_CompactActuallyDeletesRows
// for the corrected assertion).
//
// The value mirrors defaultCompactEvery deliberately, not coincidentally:
// with the client's own default compaction trigger (fire after 500 stored
// updates), retaining the newest 500 keeps this store's steady-state row
// count sawtoothing between roughly 500 and 1000 rather than growing without
// bound for the lifetime of a long-running device. KeepVersions is an
// exported field directly on SQLiteStore precisely so a caller with
// different needs — more retained history, or explicit keep-everything via
// 0 — can override this starting point on the returned *SQLiteStore;
// OpenSQLiteStore's choice here is a default, not a policy this package
// enforces.
const defaultKeepVersions = defaultCompactEvery

// SQLiteStore is the default LocalStore: a persistence.LegacyAdapter (the
// same context-free LoadDoc/StoreUpdate/Compact shape the server exposes)
// wrapping a persistence/sqlite.Store, so the client gets a durable,
// dependency-free (no CGO — persistence/sqlite is a pure-Go driver) local
// database with no extra adapter code of its own.
//
// # Why the wrapped LegacyAdapter is an unexported field, not embedded
//
// An earlier version of this type embedded *persistence.LegacyAdapter
// directly, which promotes EVERY method and field LegacyAdapter has —
// including ones with no business being reachable from a client at all:
// StoreUpdateContext, Store() VersionedPersistence, SaveVersion,
// TrimSnapshots, and KeepSnapshots. Store() in particular is the concrete
// counter-example to this exact doc's own claim, one paragraph up, that
// "this package never exposes ListVersions/MaterializeAt to a caller" — it
// returns the VersionedPersistence interface that has both, so the claim
// was false as long as the embedding stood. #165's final whole-branch
// review (Important E) flagged this as a public-API problem, not merely a
// documentation one: every one of those promoted members is exported
// surface on client.SQLiteStore, permanently, and narrowing it after a
// semver-tagged release ships would be a MAJOR bump (a public method
// disappearing breaks any caller that happened to use it, however
// unintentionally it became reachable). Wrapping via an unexported adapter
// field plus the explicit LoadDoc/StoreUpdate/Compact/Close methods below
// exposes exactly LocalStore/CompactableStore's shape — the contract this
// type is documented to satisfy — and nothing else, while leaving behaviour
// completely unchanged for every existing caller (Options.Store,
// Options.StorePath, and every method this package itself calls go through
// the same four methods either way).
type SQLiteStore struct {
	adapter *persistence.LegacyAdapter
	store   *sqlite.Store

	// KeepVersions bounds retained history: Compact (below) trims to the
	// newest KeepVersions stored updates each time it runs. 0 means "keep
	// all" — never trim. See defaultKeepVersions' own doc for why
	// OpenSQLiteStore pre-sets this to a non-zero value rather than
	// inheriting persistence.LegacyAdapter's own 0 default; override it on
	// the returned value before first use for a different retention policy.
	//
	// This field lives directly on SQLiteStore, copied into the wrapped
	// LegacyAdapter's own KeepVersions field at the top of every Compact
	// call, rather than being promoted from that adapter via embedding —
	// see this type's own "why the wrapped LegacyAdapter is an unexported
	// field" doc section for why that indirection exists. The two values
	// can only ever differ between the moment a caller sets this field and
	// the next Compact call, which reconciles them before doing anything
	// else; nothing else in this package ever reads the adapter's copy.
	KeepVersions int
}

// OpenSQLiteStore opens (creating if necessary) a SQLite-backed LocalStore at
// path. The returned *SQLiteStore satisfies CompactableStore and must be
// closed with Close when the client is done with it (Options.StorePath does
// this automatically for a store the Client itself opened; see its doc and
// Close's ownership rule).
//
// The returned store's KeepVersions is pre-set to defaultKeepVersions rather
// than left at persistence.LegacyAdapter's own 0 ("keep all") default — see
// defaultKeepVersions' own doc for why 0 would be wrong here specifically.
// Override it on the returned value before first use if a different
// retention policy is wanted.
func OpenSQLiteStore(path string) (*SQLiteStore, error) {
	store, err := sqlite.Open(path)
	if err != nil {
		return nil, err
	}
	return &SQLiteStore{
		adapter:      persistence.NewLegacyAdapter(store),
		store:        store,
		KeepVersions: defaultKeepVersions,
	}, nil
}

// LoadDoc satisfies LocalStore by forwarding to the wrapped
// persistence.LegacyAdapter.
func (s *SQLiteStore) LoadDoc(room string) ([]byte, error) {
	return s.adapter.LoadDoc(room)
}

// StoreUpdate satisfies LocalStore by forwarding to the wrapped
// persistence.LegacyAdapter.
func (s *SQLiteStore) StoreUpdate(room string, update []byte) error {
	return s.adapter.StoreUpdate(room, update)
}

// Compact satisfies CompactableStore. It reconciles the wrapped
// persistence.LegacyAdapter's own KeepVersions field with this store's
// (see KeepVersions' own doc for why that reconciliation, rather than a
// single shared value, is how this indirection is kept behaviour-identical
// to the pre-#165-final-review embedding) and then forwards to it.
func (s *SQLiteStore) Compact(ctx context.Context, room string) error {
	s.adapter.KeepVersions = s.KeepVersions
	return s.adapter.Compact(ctx, room)
}

// Close releases the underlying SQLite database handle.
func (s *SQLiteStore) Close() error {
	return s.store.Close()
}
