## v1.31.5

A CRDT convergence patch. No API changes. Found by ygo's cross-implementation
convergence fuzzer (#70), which replays randomized scenarios against live Yjs
and compares the results. Recommended for anyone whose Go peers push into arrays
that are also edited (and deleted from) by Yjs peers.

### Fixed

- **`YArray.Push` diverged from Yjs's `push` when the array's tail was a
  tombstone ([#158](https://github.com/reearth/ygo/issues/158)).** `Push`
  delegated to `Insert(Len())`, which anchors the new element after the last
  **live** element (the live-index walk skips tombstones). Yjs's `push` anchors
  after the last **physical** item, tombstones included. When the tail was a
  deleted element the two produced different YATA anchors, so a ygo peer and a
  Yjs peer that both pushed concurrently onto such an array ordered the merged
  result differently. `Push` now appends after the physical tail, matching Yjs.
  `Insert` at an explicit index is unchanged — it already matched Yjs's
  `insert`.

## Install

```
go get github.com/reearth/ygo@v1.31.5
```

See [CHANGELOG.md](https://github.com/reearth/ygo/blob/main/CHANGELOG.md) for full details.
