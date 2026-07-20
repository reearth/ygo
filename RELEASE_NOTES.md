## v1.34.0

A minor release: one new public awareness API that fixes a latent tombstone-
accumulation bug. No breaking changes.

### Added

- **`Awareness.PurgeTombstones(grace)` reclaims aged removal tombstones**
  ([#166](https://github.com/reearth/ygo/issues/166)). When a client is removed
  or expires, its entry is kept as a null-state tombstone so its clock can still
  encode removals and reject stale re-adds. These tombstones were never
  reclaimed: because they count toward `SetMaxClients` (deliberately, to bound a
  peer inventing null-state client IDs), a high-churn room's entry count grew
  monotonically and could eventually **refuse new, legitimate clients** — and
  every full `EncodeUpdate(nil)` kept re-broadcasting them. `PurgeTombstones`
  drops tombstones older than `grace` (a non-positive `grace` is a no-op; the
  local client is never purged; no observer event fires).

  `StartAutoExpiry` now runs it automatically each tick as a second stage —
  `RemoveExpired(timeout)` then `PurgeTombstones(2*timeout)` — so `grace`
  outlives normal update reordering before a tombstone's clock is forgotten.
  Callers driving expiry manually should pair `RemoveExpired` with
  `PurgeTombstones`. Awareness remains best-effort presence (not a convergent
  CRDT): a delayed low-clock update for an already-forgotten client is accepted
  as new and self-heals on the next broadcast, matching y-protocols' "forget
  after outdatedTimeout" semantics.
