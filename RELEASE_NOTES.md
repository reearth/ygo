## Highlights

- Pure-Go Yjs CRDT library, binary-compatible with the Yjs v13.x JS reference implementation
- `RelativePosition` / `AbsolutePosition` — stable cursors that survive concurrent insertions and deletions
- `UndoManager` with undo/redo and per-user origin tracking
- `YText.ApplyDelta` — ingest Quill-compatible deltas from JS clients
- WebSocket provider with auth hook, per-room connection limits, and pluggable persistence
- 17 security vulnerabilities fixed; all packages pass the race detector at 90% test coverage

## Install

```
go get github.com/reearth/ygo@v1.0.0
```

See [CHANGELOG.md](https://github.com/reearth/ygo/blob/main/CHANGELOG.md) for the full list of changes.
