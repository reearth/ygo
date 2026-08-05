package persistence

import (
	"context"
	"errors"
	"fmt"
)

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

	// KeepSnapshots bounds retained labelled snapshots when the websocket server
	// asks the adapter to save a version (see the provider's VersionableAdapter /
	// AutoVersionEvery). 0 (default) keeps every version, matching KeepVersions.
	// Set > 0 to trim to the newest KeepSnapshots after each save, so an
	// always-connected document cannot grow an unbounded history.
	//
	// Note this is retention over VERSIONS (SnapshotStore entries), which is a
	// different axis from KeepVersions' retention over the raw update log.
	KeepSnapshots int
}

// ErrSnapshotsUnsupported is returned by LegacyAdapter.SaveVersion when the
// wrapped store does not implement SnapshotStore, so there is nowhere to record
// a version. It is surfaced rather than ignored: silently discarding versions
// would look like auto-versioning is working when it is not.
var ErrSnapshotsUnsupported = errors.New("persistence: store does not implement SnapshotStore")

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

// SaveVersion satisfies the provider's optional VersionableAdapter interface. It
// materializes the room's current head and records it as a labelled snapshot via
// the wrapped store's SnapshotStore, then applies KeepSnapshots retention.
//
// An empty room is a no-op returning (0, nil): there is no state worth
// versioning, and creating an empty version on every idle room would be exactly
// the history noise auto-versioning exists to avoid.
//
// Retention failures are deliberately NOT propagated: the version is already
// durable at that point, and reporting an error would make the provider log a
// failure for an operation that actually succeeded. A trim that does not happen
// is retried after the next save.
func (a *LegacyAdapter) SaveVersion(ctx context.Context, room, label string) (int64, error) {
	ss, ok := a.store.(SnapshotStore)
	if !ok {
		return 0, ErrSnapshotsUnsupported
	}
	res, err := a.store.Load(ctx, room)
	if err != nil {
		return 0, err
	}
	if len(res.Update) == 0 {
		return 0, nil
	}
	id, err := ss.SaveSnapshot(ctx, room, label, res.Update)
	if err != nil {
		return 0, err
	}
	// Best-effort, and scoped to the label just written so an auto version
	// cannot evict one a user named — see the doc above for why a retention
	// failure is not propagated.
	_, _ = a.TrimSnapshots(ctx, room, label)
	return id, nil
}

// TrimSnapshots deletes the oldest snapshots of room labelled label beyond
// KeepSnapshots, and reports how many it deleted.
//
// Retention is scoped to the LABEL CLASS: a call with the server's auto-version
// label never deletes a snapshot a user named, and a named save never disturbs
// the auto history. The label is a parameter rather than something the adapter
// infers because this package deliberately does not import provider/websocket,
// so it cannot know which label means "automatic" — and scoping to the label
// the caller just wrote needs no such knowledge.
//
// KeepSnapshots <= 0 keeps everything and returns (0, nil). Note this is the
// opposite of some other Yjs ports, where a keep of 0 deletes every version.
//
// Because the bound is per class, a room's TOTAL snapshot count is
// (distinct labels x KeepSnapshots) and is NOT itself bounded. A caller needing
// a hard per-room cap must enumerate the labels from ListSnapshots and trim
// each one; bounding the total is what evicts named snapshots, which is the
// behaviour this scoping exists to prevent.
//
// Every surplus snapshot is attempted even if an earlier delete fails: the
// count is how many were actually deleted, and the error joins each failure.
// DeleteSnapshot is contractually idempotent (deleting an unknown snapshot
// returns nil), so a concurrent trim of the same class cannot make this report
// a spurious failure.
func (a *LegacyAdapter) TrimSnapshots(ctx context.Context, room, label string) (int, error) {
	ss, ok := a.store.(SnapshotStore)
	if !ok {
		return 0, ErrSnapshotsUnsupported
	}
	if a.KeepSnapshots <= 0 {
		return 0, nil
	}
	snaps, err := ss.ListSnapshots(ctx, room)
	if err != nil {
		return 0, err
	}
	// ListSnapshots is newest-first by contract, so filtering preserves that
	// order and everything at or past KeepSnapshots within the class is surplus.
	var class []SnapshotInfo
	for _, sn := range snaps {
		if sn.Label == label {
			class = append(class, sn)
		}
	}
	if len(class) <= a.KeepSnapshots {
		return 0, nil
	}
	var (
		deleted int
		errs    []error
	)
	for _, sn := range class[a.KeepSnapshots:] {
		if derr := ss.DeleteSnapshot(ctx, room, sn.ID); derr != nil {
			errs = append(errs, fmt.Errorf("delete snapshot %d: %w", sn.ID, derr))
			continue
		}
		deleted++
	}
	return deleted, errors.Join(errs...)
}
