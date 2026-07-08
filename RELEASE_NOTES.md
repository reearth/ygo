## What's new

A data-loss bug fix for `MergeUpdatesV1`/`DiffUpdateV1`. No API changes.

### Fixed

- **`MergeUpdatesV1`/`DiffUpdateV1` no longer drop a nested-type child whose
  parent is referenced by item-ID across client groups (#140).** The struct-level
  merge path decodes structs without integrating them, so a deferred child
  (`Parent` not yet resolved, only `parentID` known) was re-encoded either as a
  content-less GC placeholder or attached to an empty-named root type — silently
  detaching it. This made `MergeUpdatesV1(u)` diverge from apply-then-encode: a
  37-byte full-state update came back 20 bytes with the child reduced to a GC
  placeholder. It was a regression from the struct-level merge rewrite (#125),
  affecting v1.27.0–v1.30.0. Consumers that reconstruct a document by folding a
  base snapshot plus tail updates through `MergeUpdatesV1` (e.g. loading from
  persistence) lost nested-type children authored by a lower-clientID peer, with
  no error. The merge/diff encoder now re-emits the deferred parent-by-ID so the
  child is preserved.

## Install

```
go get github.com/reearth/ygo@v1.30.1
```

See [CHANGELOG.md](https://github.com/reearth/ygo/blob/main/CHANGELOG.md) for full details.
