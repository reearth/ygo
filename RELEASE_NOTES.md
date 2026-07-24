## v1.38.0

A minor, additive release adding two independent features. On the awareness
layer, a new `OnUpdate` event channel fires on every applied entry (including
heartbeats) alongside the content-only `OnChange`, plus a `Meta(clientID)`
accessor — matching Yjs `y-protocols`/yrs semantics. On the websocket provider,
opt-in Hocuspocus docName framing and an `OnTokenAuth` hook bring in-band token
authentication for the `@hocuspocus/provider` client ecosystem. No breaking
API changes, though `OnChange` is tightened (see Changed).

### Added

- **Awareness `OnUpdate` + `Meta(clientID)`**
  ([#105](https://github.com/reearth/ygo/issues/105)): `OnUpdate` fires on every
  applied awareness entry including heartbeats, distinct from the content-only
  `OnChange`. `Meta(clientID)` returns per-client `{Clock, LastUpdated}`,
  retained for tombstones to match the reference implementations.
- **Hocuspocus in-band token auth + docName framing**
  ([#104](https://github.com/reearth/ygo/issues/104)): opt-in
  `Server.HocuspocusFraming` reads/writes docName-prefixed frames for real
  `@hocuspocus/provider` interop; `Server.OnTokenAuth` validates the tag-2 Auth
  token and replies `Authenticated`/`PermissionDenied`, closing denied
  connections with WebSocket code `4401`. Composes with `AuthFunc`/`Authorize`;
  it is a handshake reply + optional read-only downgrade, not a
  document-confidentiality gate (use the HTTP-boundary auth for that).

### Changed

- **`OnChange` no longer fires on content-identical re-emits**
  ([#105](https://github.com/reearth/ygo/issues/105)): remote heartbeats (via
  `ApplyUpdate`) and local same-content `SetLocalState` now fire `OnUpdate`
  only. `Heartbeat()` now fires `OnUpdate` (previously silent). A reactivated
  client is classified `Updated` rather than `Added`, matching Yjs/yrs.

## v1.37.0

A minor release hardening the websocket server's coalesced persistence path.
It closes a durability gap where a room could be evicted before its pending
batch was flushed, and gives adapters an optional way to bound stored-version
growth. No breaking changes; the default behaviour of servers without a
`CompactableAdapter` is unchanged.

### Fixed

- **Lost edits on quick refresh with coalesced persistence**
  ([#175](https://github.com/reearth/ygo/issues/175)): the room-teardown paths
  (`handleDisconnect`, `CloseRoom`) now flush the pending coalesced batch
  durably — and await it — while the room is still discoverable, then re-check
  and evict. A peer that reconnects during the flush reuses the live
  in-memory document instead of reloading stale state from the backing store.

### Added

- **`CompactableAdapter` and `Server.CompactEvery`**
  ([#175](https://github.com/reearth/ygo/issues/175)): an optional
  `PersistenceAdapter` extension the server calls on room unload, and — when
  `CompactEvery > 0` — after every N persistence flushes, letting an adapter
  bound stored-version growth. `persistence.LegacyAdapter` implements it via a
  new `KeepVersions` field, forwarding to the existing
  `VersionedPersistence.Compact`.

## Install

```
go get github.com/reearth/ygo@v1.37.0
```

See [CHANGELOG.md](https://github.com/reearth/ygo/blob/main/CHANGELOG.md) for full details.

---

## v1.36.0

A minor release that changes the default persistence behaviour for the
websocket server: backing-store writes are now debounce-coalesced (2s window,
10s max wait) into a single merged `StoreUpdate` per burst, instead of one
write per committed transaction, cutting persistence latency and version
churn under load. This only affects servers with a `PersistenceAdapter`
configured (`NewServerWithPersistence`); plain `NewServer()` is unaffected.
Set `Server.PersistCoalesceWindow = -1` to opt back into the previous strict
per-update behaviour.

### Changed

- **Websocket persistence writes are coalesced by default (behaviour
  change)** ([#175](https://github.com/reearth/ygo/issues/175)): the
  per-room persistence worker debounces writes and merges each burst into a
  single `StoreUpdate` call rather than writing once per update
  (Hocuspocus parity). Only servers with a `PersistenceAdapter` configured
  are affected.

### Added

- **`Server.PersistCoalesceWindow` and `Server.PersistCoalesceMaxWait`**
  ([#175](https://github.com/reearth/ygo/issues/175)): tune or disable
  persistence coalescing. Defaults are 2s and 10s; a negative
  `PersistCoalesceWindow` (e.g. `-1`) disables coalescing and restores strict
  per-update writes.

## Install

```
go get github.com/reearth/ygo@v1.36.0
```

See [CHANGELOG.md](https://github.com/reearth/ygo/blob/main/CHANGELOG.md) for full details.

---

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
