package persistence

import "context"

// LegacyAdapter adapts a VersionedPersistence to the provider/websocket
// PersistenceAdapter contract (LoadDoc / StoreUpdate), so a versioned store can
// be passed to websocket.NewServerWithPersistence. It is intentionally defined
// without importing provider/websocket — the websocket package consumes its
// PersistenceAdapter dependency structurally, so matching method signatures is
// sufficient and avoids an import cycle.
//
// Mapping:
//   - LoadDoc(room)          → Load(ctx, room).Update          (materialized head)
//   - StoreUpdate(room, upd) → AppendUpdate(ctx, room, upd)    (drops the Version)
//
// The provider calls StoreUpdate once per committed transaction with the
// incremental V1 update, which is exactly AppendUpdate's contract — so every
// transaction becomes one persisted Version, giving the version history for
// free.
type LegacyAdapter struct {
	store VersionedPersistence
	ctx   context.Context
}

// NewLegacyAdapter wraps store. The provider's LoadDoc/StoreUpdate are
// context-free; the adapter uses context.Background() for the underlying calls.
// Use NewLegacyAdapterContext to supply a context (e.g. one cancelled on
// shutdown) so I/O-backed stores can abort in-flight work.
func NewLegacyAdapter(store VersionedPersistence) *LegacyAdapter {
	return &LegacyAdapter{store: store, ctx: context.Background()}
}

// NewLegacyAdapterContext is like NewLegacyAdapter but threads ctx through every
// underlying store call.
func NewLegacyAdapterContext(ctx context.Context, store VersionedPersistence) *LegacyAdapter {
	if ctx == nil {
		ctx = context.Background()
	}
	return &LegacyAdapter{store: store, ctx: ctx}
}

// LoadDoc returns the merged V1 head state for room, or (nil, nil) when empty —
// matching the PersistenceAdapter contract.
func (a *LegacyAdapter) LoadDoc(room string) ([]byte, error) {
	res, err := a.store.Load(a.ctx, room)
	if err != nil {
		return nil, err
	}
	return res.Update, nil
}

// StoreUpdate appends one incremental V1 update as a new Version. The assigned
// version is discarded to satisfy the PersistenceAdapter signature; callers
// needing the version should use the VersionedPersistence directly.
func (a *LegacyAdapter) StoreUpdate(room string, update []byte) error {
	_, err := a.store.AppendUpdate(a.ctx, room, update)
	return err
}

// StoreUpdateContext satisfies the optional PersistenceAdapterContext extension,
// threading the supplied ctx through to AppendUpdate so the provider can abort
// in-flight writes on shutdown.
func (a *LegacyAdapter) StoreUpdateContext(ctx context.Context, room string, update []byte) error {
	_, err := a.store.AppendUpdate(ctx, room, update)
	return err
}

// Store returns the wrapped VersionedPersistence so callers can reach the
// versioned API (ListVersions, MaterializeAt, snapshots, prune) alongside the
// legacy provider integration.
func (a *LegacyAdapter) Store() VersionedPersistence {
	return a.store
}
