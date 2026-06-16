## What's new

A batch of three CRDT correctness fixes, each verified against the Yjs reference
(`yjs@13.6.30`). No breaking API changes.

### Fixed

- **`YText.Format` no longer strips formatting outside its range.** Re-applying
  or toggling a format over a sub-range of an already-formatted run used to
  delete the marker bounding content *after* the range — most visibly on
  documents loaded via `ApplyUpdate`. `Format` now ports the Yjs `formatText`
  algorithm; `YText.ToDelta` also coalesces adjacent equal-attribute inserts to
  match Yjs `toDelta`.
- **Undo of a deletion is now CRDT-sound and propagates to peers.** `UndoManager`
  previously flipped the deleted flag in place, which produced no wire record —
  so a peer never revived the content and a back-sync re-deleted it, silently
  losing the undo. Undo now re-inserts a copy of the content as a new item (Yjs
  `redoItem`), converging across peers. Works for YText, YArray, and YMap.
- **`MergeUpdatesV1` / `DiffUpdateV1` no longer drop non-integrable structs.**
  They re-encoded an integrated temp document, so a struct whose dependency was
  in a prior update was silently dropped. They now merge at the struct level
  (Yjs `mergeUpdates`/`diffUpdate` parity), preserving every struct.

### Added

- **`crdt.SharedType`** is exported, so `NewUndoManager`'s scope can be named
  from outside the package (it was effectively unusable externally before).
- **`MergeUpdatesV2`, `DiffUpdateV2`, `EncodeStateVectorFromUpdate` /
  `EncodeStateVectorFromUpdateV2`** — the V2 columnar equivalents of the
  merge/diff fixes, plus extracting a state vector straight from an update
  without integrating it (closes #57).

### Compatibility

No breaking API changes. One behavioral note: `YText.ToDelta` now returns the
same text/attributes merged into fewer ops where a run was previously split
across backing Items — code comparing the full op list should expect the
coalesced shape.

## Install

```
go get github.com/reearth/ygo@v1.27.0
```

See [CHANGELOG.md](https://github.com/reearth/ygo/blob/main/CHANGELOG.md) for full details.
