## What's new

- **CRDT-safe array Move**: `YArray.Move()` is now a proper CRDT primitive. The previous delete-then-insert approach lost causal history and diverged under concurrent edits. The new implementation uses a `ContentMove` marker so the element preserves its identity, concurrent moves of different elements both apply, and concurrent moves of the same element converge to the lower-ClientID winner. Included in V1 and V2 wire encoding.
- **YText Format observer delta fix**: `YText.Format()` now emits an accurate `retain + attributes` delta to observers. Previously the delta was missing or reported the wrong range, which would cause collaborative editors to show stale formatting to peers.
- **XML insert API**: `YXmlFragment` and `YXmlElement` now expose `InsertElement` and `InsertText` as exported methods, making it possible to build XML documents programmatically from outside the `crdt` package.

## Install

```
go get github.com/reearth/ygo@v1.0.5
```

See [CHANGELOG.md](https://github.com/reearth/ygo/blob/main/CHANGELOG.md) for full details.
