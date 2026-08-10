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
// append-only update log can be asked to collapse it into a compact form. An
// offline-first client is the case this matters most for — a long-lived
// local device with no server present to trigger the periodic compaction the
// server does on its own — so callers (later #165 tasks) are expected to
// invoke Compact on their own schedule (e.g. on reconnect, or periodically)
// rather than relying on anything in this package to do it for them.
type CompactableStore interface {
	LocalStore
	// Compact collapses room's stored update log without changing the state
	// LoadDoc returns for it. Retention policy is the store's concern.
	Compact(ctx context.Context, room string) error
}

// defaultKeepVersions is the non-zero retention OpenSQLiteStore applies to
// the LegacyAdapter it constructs (see SQLiteStore.KeepVersions, embedded).
//
// LegacyAdapter.KeepVersions defaults to 0 ("keep all history") when built
// directly via persistence.NewLegacyAdapter, which is the right default for
// the SERVER side (provider/websocket): an operator opts into retention
// explicitly via CompactEvery + KeepVersions, because the raw update log
// there doubles as a room's audit/version history, and trimming it by
// default would be a worse surprise than the disk growth it saves. A
// client's SQLiteStore has no such use for that history — this package
// never exposes ListVersions/MaterializeAt to a caller — so inheriting the
// SAME zero default here would make every Compact call this package ever
// makes (see Client.maybeCompact) a permanent, silent no-op: exactly the gap
// #165's Task 1 review flagged (TestSQLiteStore_CompactPreservesState passed
// trivially because nothing was ever actually deleted; see
// TestSQLiteStore_CompactActuallyDeletesRows for the corrected assertion).
//
// The value mirrors defaultCompactEvery deliberately, not coincidentally:
// with the client's own default compaction trigger (fire after 500 stored
// updates), retaining the newest 500 keeps this store's steady-state row
// count sawtoothing between roughly 500 and 1000 rather than growing without
// bound for the lifetime of a long-running device. KeepVersions is an
// exported field on the embedded *persistence.LegacyAdapter precisely so a
// caller with different needs — more retained history, or explicit
// keep-everything via 0 — can override this starting point on the returned
// *SQLiteStore; OpenSQLiteStore's choice here is a default, not a policy
// this package enforces.
const defaultKeepVersions = defaultCompactEvery

// SQLiteStore is the default LocalStore: a persistence.LegacyAdapter (the
// same context-free LoadDoc/StoreUpdate/Compact shape the server exposes)
// wrapping a persistence/sqlite.Store, so the client gets a durable,
// dependency-free (no CGO — persistence/sqlite is a pure-Go driver) local
// database with no extra adapter code of its own.
type SQLiteStore struct {
	*persistence.LegacyAdapter
	store *sqlite.Store
}

// OpenSQLiteStore opens (creating if necessary) a SQLite-backed LocalStore at
// path. The returned *SQLiteStore satisfies CompactableStore and must be
// closed with Close when the client is done with it (Options.StorePath does
// this automatically for a store the Client itself opened; see its doc and
// Close's ownership rule).
//
// The returned store's KeepVersions is pre-set to defaultKeepVersions rather
// than left at LegacyAdapter's own 0 ("keep all") default — see
// defaultKeepVersions' doc for why 0 would be wrong here specifically.
// Override it on the returned value before first use if a different
// retention policy is wanted.
func OpenSQLiteStore(path string) (*SQLiteStore, error) {
	store, err := sqlite.Open(path)
	if err != nil {
		return nil, err
	}
	adapter := persistence.NewLegacyAdapter(store)
	adapter.KeepVersions = defaultKeepVersions
	return &SQLiteStore{
		LegacyAdapter: adapter,
		store:         store,
	}, nil
}

// Close releases the underlying SQLite database handle.
func (s *SQLiteStore) Close() error {
	return s.store.Close()
}
