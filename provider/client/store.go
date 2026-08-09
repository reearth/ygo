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
// server, and calls StoreUpdate for every locally-applied transaction — the
// same "durable before it matters" ordering the server's PersistenceAdapter
// documents, applied to a client that may spend most of its life offline.
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
// closed with Close when the client is done with it.
func OpenSQLiteStore(path string) (*SQLiteStore, error) {
	store, err := sqlite.Open(path)
	if err != nil {
		return nil, err
	}
	return &SQLiteStore{
		LegacyAdapter: persistence.NewLegacyAdapter(store),
		store:         store,
	}, nil
}

// Close releases the underlying SQLite database handle.
func (s *SQLiteStore) Close() error {
	return s.store.Close()
}
