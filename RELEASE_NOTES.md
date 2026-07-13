## v1.31.6

A CRDT convergence + correctness patch. No API changes. Found by ygo's
cross-implementation convergence fuzzer (#70), which replays randomized
scenarios against live Yjs and compares the results. Three related,
tombstone-triggered fixes ([#160](https://github.com/reearth/ygo/issues/160)).
Recommended for anyone editing text (or arrays) that also gets deletions.

### Fixed

- **`YText.Insert` anchored a run before an adjacent tombstone, diverging from
  Yjs (text-path mirror of #158).** It anchored relative to the last **live**
  item (leaving `originRight` pointing at a tombstone) and landed *before* the
  tombstone; Yjs advances past deleted items and lands *after*. Two peers
  inserting next to the same tombstone then ordered their runs differently.
  `Insert`/`InsertEmbed` now advance the anchor past adjacent deleted items.
  Only text was affected; arrays/maps/XML already converged.
- **Stale `firstLiveCache` after inserting past leading tombstones.** A live run
  inserted after leading tombstones became the first live item without becoming
  the list head, so the cache was never invalidated and a later `Delete` walked
  a stale head and removed the wrong indices (it could silently no-op). Now
  invalidated when a live item lands after a deleted left neighbour.
- **Stale `posCache` after a remote delete-only apply (pre-existing).** A remote
  update that only tombstoned an item left the position cache stale, so the next
  local positioned insert resolved the wrong neighbour. The remote delete-set
  path now invalidates the position cache after deleting a countable item.

## Install

```
go get github.com/reearth/ygo@v1.31.6
```

See [CHANGELOG.md](https://github.com/reearth/ygo/blob/main/CHANGELOG.md) for full details.
