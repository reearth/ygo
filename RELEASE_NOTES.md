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
