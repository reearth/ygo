## v1.31.3

A CRDT convergence patch. No API changes. Found by ygo's randomized convergence
fuzzer (#70) once nested containers were exercised, and confirmed against real
Yjs. Recommended for anyone using nested containers (XML elements, or nested
shared types) with the default `gc:true`.

### Fixed

- **Deleting a GC'd nested container aborted a concurrent child-merge
  ([#154](https://github.com/reearth/ygo/issues/154)).** When a nested container
  (e.g. an XML element) is deleted, auto-GC replaces its `ContentType` with a
  `ContentDeleted` tombstone. A concurrent remote update still referencing that
  container by parent-ID then failed a `ContentType` type-assertion and returned
  a hard "parent item is not a ContentType" error, **aborting the entire
  update/merge** rather than dropping the single orphaned child. The V1/V2
  decode and within-update resolve paths now treat a non-ContentType
  parent-by-ID as an orphan (`parent = nil`) and continue — matching Yjs, which
  on the identical scenario throws nothing, drops the orphan, and converges.
  This also reverses the overly-strict resolve-pass error added in v1.31.2.

## Install

```
go get github.com/reearth/ygo@v1.31.3
```

See [CHANGELOG.md](https://github.com/reearth/ygo/blob/main/CHANGELOG.md) for full details.
