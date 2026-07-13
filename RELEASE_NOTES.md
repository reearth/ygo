## v1.31.4

A CRDT convergence patch. No API changes. Found by ygo's randomized convergence
fuzzer (#70) once nested containers were exercised, and confirmed to converge in
real Yjs where ygo diverged. Recommended for anyone using nested containers
(XML elements, or nested shared types) with the default `gc:true`.

### Fixed

- **A deleted-and-GC'd container's orphaned attribute could be mis-grafted onto
  an unrelated root map, diverging peers
  ([#156](https://github.com/reearth/ygo/issues/156)).** When an XML element is
  deleted, auto-GC replaces its `ContentType` with a `ContentDeleted` tombstone,
  so a concurrent attribute write on that element arrives as a keyed item whose
  parent can no longer be resolved. A store-wide fallback then attached that
  orphan to the **first** map-type parent it found while scanning the store —
  which depends on integration order and Go map iteration, so two peers that saw
  the same updates in different orders ended up with a spurious key (e.g.
  `"id"`) on the root map on one side only. That fallback is now removed at all
  three resolve sites (V1 within-update, V1 doc-level drain, and V2); such
  orphans drop on every peer, matching Yjs, which integrates an item with an
  unresolved parent as a no-op. The divergence was pre-existing and had been
  masked by the pre-#154 hard abort; it hit 3 of the first 1000 fuzzer seeds,
  and all 1000 now converge.

## Install

```
go get github.com/reearth/ygo@v1.31.4
```

See [CHANGELOG.md](https://github.com/reearth/ygo/blob/main/CHANGELOG.md) for full details.
