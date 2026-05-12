## What's new

- **`crdt` internal refactor (#29)** — `applyV1Txn` (the V1 update decoder) refactored from a 277-line function into a thin orchestrator + three focused helpers (`decodeAndPark`, `resolveWithinUpdatePending`, `drainPending`). Pure refactor: zero behavior change, all existing tests pass without modification.

## Install

```
go get github.com/reearth/ygo@v1.6.1
```

See [CHANGELOG.md](https://github.com/reearth/ygo/blob/main/CHANGELOG.md) for full details.
