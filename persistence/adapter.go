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

	// KeepVersions bounds retained history when the websocket server asks the
	// adapter to compact (see the provider's CompactableAdapter / CompactEvery).
	// 0 (default) keeps all history — Compact becomes a cheap no-op. Set > 0 to
	// trim to the newest KeepVersions updates on each compaction. Adapter-side
	// retention policy; set before serving.
	KeepVersions int
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

// Compact satisfies the provider's optional CompactableAdapter interface. It
// forwards to the wrapped VersionedPersistence.Compact with the configured
// KeepVersions retention (0 = keep all). The deleted count is dropped.
func (a *LegacyAdapter) Compact(ctx context.Context, room string) error {
	_, err := a.store.Compact(ctx, room, a.KeepVersions)
	return err
}

// Store returns the wrapped VersionedPersistence so callers can reach the
// versioned API (ListVersions, MaterializeAt, snapshots, prune) alongside the
// legacy provider integration.
func (a *LegacyAdapter) Store() VersionedPersistence {
	return a.store
}
