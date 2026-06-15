## What's new

A rich-text formatting correctness fix. `YText.Format` is now a faithful port of
the Yjs reference algorithm, so formatting no longer leaks outside the range it
was applied to, and `ToDelta` output matches Yjs.

### Fixed

- **`YText.Format` no longer strips formatting outside its range.** Re-applying
  or toggling a format over a sub-range of an already-formatted run used to
  delete the marker bounding content *after* the range, stripping the
  surrounding run's formatting — most visibly on documents loaded via
  `ApplyUpdate`, but also on freshly-typed text. `Format` now ports Yjs JS
  `formatText` (cursor-based, with negated-attribute restoration): it opens
  markers only where the value changes, deletes only in-range overlapping
  markers, and restores the post-range state. Verified against `yjs@13.6.30`
  across fresh and loaded documents.

### Changed

- **`YText.ToDelta` coalesces adjacent inserts with equal attributes** into one
  op, matching Yjs JS `toDelta`. A run split across multiple backing Items (e.g.
  by a `Format` boundary) previously emitted several adjacent ops with identical
  attributes; consumers will now see fewer, coalesced ops. The rendered text and
  attributes are unchanged.

### Compatibility

No breaking API changes. `ToDelta` returns the same text/attributes, just merged
into fewer ops where they were previously split — code that compares the full op
list (rather than the rendered content) should expect the coalesced shape.

## Install

```
go get github.com/reearth/ygo@v1.27.0
```

See [CHANGELOG.md](https://github.com/reearth/ygo/blob/main/CHANGELOG.md) for full details.
