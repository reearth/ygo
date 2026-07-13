## v1.31.1

A CRDT correctness and performance patch bundling two fix PRs (#145, #150). No
API changes. Several of these were surfaced by ygo's new randomized convergence
fuzzer (#70) on its first runs and confirmed by independent byte-exact
isolation. Upgrading is recommended for anyone using `YMap` or the V2 codec
across concurrent peers.

### Fixed

- **YMap key silently lost on `ApplyUpdateV1` ([#149](https://github.com/reearth/ygo/issues/149)).**
  When a map-keyed item's origin was authored by a higher-clientID peer, the
  item decoded before its origin's client group and took the V1 deferred-parent
  retry path, which resolved the parent but did not inherit `ParentSub`. The
  item integrated keyless and vanished from the map — a single document's own
  full-state encode could fail to round-trip (`EncodeStateAsUpdateV1` →
  `ApplyUpdateV1` dropping a live key), and two peers applying the same updates
  in different orders could diverge. Present since the Yjs wire-format work; in
  v1.31.0.

- **`ApplyUpdateV2` hard-failed on a deferred parent-by-ID child
  ([#146](https://github.com/reearth/ygo/issues/146)).** When a nested
  container's first child was authored by a higher-clientID peer, V2's
  descending client-group order decoded the child before its parent, and
  `ApplyUpdateV2` returned `parent item not found`. The V2 decoder now defers
  and resolves it, matching the V1 fix (#140).

- **V2 struct-level merge/diff dropped a deferred parent-by-ID child.**
  `MergeUpdatesV2`/`DiffUpdateV2` re-encoded such a child as a GC placeholder,
  losing its content and parent link. Fixed, with the V1/V2 resolve passes now
  returning a consistent error on a genuinely corrupt parent-by-ID.

- **`examples/peer-sync` deadlocked ([#138](https://github.com/reearth/ygo/issues/138)).**
  It called `GetText`/`GetArray`/`GetMap` inside a `Transact` callback (a
  re-entrant lock hang); handles are now resolved before the transaction.

### Performance

- **V2 string-column decode is now O(n), not O(n²)** — `ApplyUpdateV2` on
  string-heavy documents drops roughly 6×, on par with `ApplyUpdateV1`.

## Install

```
go get github.com/reearth/ygo@v1.31.1
```

See [CHANGELOG.md](https://github.com/reearth/ygo/blob/main/CHANGELOG.md) for full details.
