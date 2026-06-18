## What's new

A snapshot-reconstruction API addition and a CRDT conflict-scan performance fix.
No breaking changes for callers following the documented snapshot contract.

### Added

- **`CreateDocFromSnapshot(src, snap)`** — reconstruct a document's historic state
  (the state a `Snapshot` captured) into a new, independent `Doc`. The Go
  equivalent of Yjs JS's `createDocFromSnapshot`: items inserted after the
  snapshot are excluded, and a key/element deleted *after* the snapshot reappears
  in the reconstruction. The returned doc is non-GC, so it can be snapshotted or
  restored from again.
- **`ErrSnapshotSourceGCed`** — returned when reconstructing from a GC-enabled
  source doc, which cannot be done faithfully (a GC-enabled doc discards deleted
  items' content at commit). Create the source `WithGC(false)`.

### Changed

- **`RestoreDocument`** now returns `ErrSnapshotSourceGCed` for a GC-enabled
  source instead of silently returning an incomplete document. It shares the new
  reconstruction path; callers already using `WithGC(false)` are unaffected.

### Performance

- **Faster, leaner convergence under high same-position contention.**
  `Item.integrate` now reuses its YATA conflict-tracking map via `clear()` rather
  than reallocating it on every conflict-group reset (matching Yjs). At 400 peers
  inserting at the same index, convergence does about 92% fewer allocations, 55%
  less memory and 29% less time; the common low-contention path is unchanged.

## Install

```
go get github.com/reearth/ygo@v1.28.0
```

See [CHANGELOG.md](https://github.com/reearth/ygo/blob/main/CHANGELOG.md) for full details.
