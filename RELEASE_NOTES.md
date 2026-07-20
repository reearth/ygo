## v1.35.0

A minor release hardening the websocket broadcast path against slow peers. It
fixes head-of-line blocking in the writer and adds an opt-in in-place resync
policy so a transiently-slow peer recovers without reconnect churn. No breaking
changes; the default behaviour is unchanged.

### Fixed

- **Head-of-line blocking in the websocket broadcast writer**
  ([#172](https://github.com/reearth/ygo/issues/172)): the per-peer write mutex
  (`wmu`) was held across the blocking `conn.WriteMessage`, so a single slow or
  stalled peer could block broadcasts to every other peer for up to
  `writeTimeout`, and the queue-overflow branch could never fire while a write
  was in flight. The write path now holds `wmu` only to read the `closed` flag,
  then writes without it.

### Added

- **`SlowPeerResync` policy for graceful slow-peer recovery**
  ([#172](https://github.com/reearth/ygo/issues/172)): new
  `Server.SlowPeerPolicy`. `SlowPeerDisconnect` (default) closes a peer whose
  broadcast queue overflows; `SlowPeerResync` keeps the connection open, drops
  the stale delta, and sends a full-state resync once the queue drains, so the
  peer converges in place without a reconnect.

### Changed

- **Default `PeerWriteQueueSize` bumped 256 → 512**: more slack before a
  transiently-slow peer overflows (matching the yrs broadcast ring of 512);
  override via `Server.PeerWriteQueueSize`.

## Install

```
go get github.com/reearth/ygo@v1.35.0
```

See [CHANGELOG.md](https://github.com/reearth/ygo/blob/main/CHANGELOG.md) for full details.

---

## v1.34.0

A minor release bundling a full mobile on-device editor with an awareness
tombstone-reclamation fix. No breaking changes to the core library.

### Added

- **On-device editing for the mobile bindings**
  ([#118](https://github.com/reearth/ygo/issues/118)): `mobile.Doc` gains
  gomobile-safe mutators — `InsertText`, `InsertTextWithAttributes`, `DeleteText`,
  `FormatText`, `InsertArray`, `DeleteArray`, `SetMap`, `DeleteMapKey` — each
  validated and transaction-wrapped, returning an error (never panicking) on bad
  input. A Swift/Kotlin app is now a full editor, not just a viewer.

- **Push change-notifications for the mobile bindings**
  ([#119](https://github.com/reearth/ygo/issues/119)): `Doc.Observe` delivers the
  V1 update bytes plus a `local` flag after each committed transaction;
  `Awareness.Observe` delivers `{added,updated,removed}` client-id sets. Delivery
  is on a background goroutine; `Subscription.Close()` unsubscribes and all
  observers detach on `Doc`/`Awareness` `Close`.

- **`Awareness.PurgeTombstones(grace)` reclaims aged removal tombstones**
  ([#166](https://github.com/reearth/ygo/issues/166)): removal tombstones (kept so
  a client's clock can still encode removals and reject stale re-adds) were never
  reclaimed, so a high-churn room's entry count grew without bound against
  `SetMaxClients` and could eventually refuse new, legitimate clients.
  `PurgeTombstones(grace)` drops tombstones older than `grace`; `StartAutoExpiry`
  now runs it each tick as a second stage (`RemoveExpired(timeout)` then
  `PurgeTombstones(2*timeout)`).

### Changed

- **Idiomatic Yjs JSON from the mobile read accessors**
  ([#109](https://github.com/reearth/ygo/issues/109)): `Doc.GetTextJSON` now emits
  idiomatic single-op Yjs delta (`[{"insert":"hi","attributes":{...}}]`) and
  `Awareness.StatesJSON` emits `{"<clientID>": <state>}` without the internal
  clock. These reshape two mobile read accessors whose output was pre-stable
  (`GetTextJSON` was explicitly documented as unstable); no core-library change.

## Install

```
go get github.com/reearth/ygo@v1.34.0
```

See [CHANGELOG.md](https://github.com/reearth/ygo/blob/main/CHANGELOG.md) for full details.
