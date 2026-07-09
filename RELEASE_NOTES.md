## [Unreleased]

Subdocument lifecycle (issue #63): a `Doc` can now embed another `Doc` as a
subdocument — its own clock space and GUID, nested inside a parent doc's
`YMap` — mirroring Yjs's subdocuments feature. This is the local half of the
feature (create, embed, enumerate, observe); live cross-peer subdocument sync
is a separate, tracked follow-up (see [#142](https://github.com/reearth/ygo/issues/142)).

### Added

- **Subdocument embedding** — `YMap.Set(txn, key, subdoc)` where `subdoc` is
  a `*crdt.Doc`; `YMap.Get` returns the `*crdt.Doc` back out. A `Doc` may be
  embedded only once — a second attempt panics with the new
  `crdt.ErrSubdocAlreadyIntegrated`.
- **`Doc.GetSubdocs()` / `Doc.GetSubdocGUIDs()`** — the subdocuments
  currently resident on a doc, sorted by GUID.
- **`Doc.OnSubdocs(func(crdt.SubdocsEvent)) func()`** — subscribes to
  subdocument add/remove/load events; returns an unsubscribe closure.
- **`crdt.SubdocsEvent{Added, Removed, Loaded []*Doc}`** — reports docs newly
  embedded, docs detached, and docs that should now be synced.
- **`Doc.Load()`** — signals a subdocument's data should be synced, flipping
  `ShouldLoad()` to `true` and emitting a `Loaded` event on the parent.
- **`crdt.WithAutoLoad`/`WithShouldLoad`/`WithCollectionID`** `DocOption`s,
  plus accessors `Doc.AutoLoad()`/`Doc.ShouldLoad()`/`Doc.CollectionID()`.
- **`crdt.Example_subdocs`** — a runnable godoc example
  (`crdt/example_subdocs_test.go`).
- `ContentDoc` opts (guid/gc/autoLoad/collectionId) now round-trip on both
  V1 and V2 wire formats and survive `MergeUpdatesV1`/`MergeUpdatesV2`; byte
  parity with real Yjs verified against a `yjs@13.6.30` fixture.

### Changed

- **`crdt.New()` now defaults a Doc's `guid` to a random uuidv4** (was
  `""`) — Yjs parity, and an observable change to `Doc.GUID()` for docs
  created without `WithGUID`. Docs constructed with `crdt.WithGUID(...)` are
  unaffected.

## Install

```
go get github.com/reearth/ygo@vX.Y.Z
```

*(Version to be assigned at release time.)*

See [CHANGELOG.md](https://github.com/reearth/ygo/blob/main/CHANGELOG.md) for full details.
