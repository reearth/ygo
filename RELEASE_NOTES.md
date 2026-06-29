## What's new

A clustering bug fix. When multiple WebSocket server instances share a document
through a `cluster.Relay`, the second instance to activate a room that already had
history on the shared stream could deadlock — and because it deadlocked while
holding the server's rooms lock, the whole instance stopped serving every room.
This release fixes that; no API changes.

### Fixed

- **WebSocket clustering deadlock (#133)** — `getOrCreateRoom` fired the relay's
  `RoomActivated` callback while holding the rooms lock. A relay that replays
  caught-up history from inside `RoomActivated` (via `Sink.Inject`) re-entered
  `getOrCreateRoom` and blocked on the same non-reentrant lock, wedging the whole
  instance. `RoomActivated` now fires after the lock is released and the room is
  published, so the re-entrant `Inject` finds the room and returns. This matches
  how `RoomDeactivated` was already invoked off-lock. Single-node and
  first-instance deployments were never affected.

## Install

```
go get github.com/reearth/ygo@v1.29.1
```

See [CHANGELOG.md](https://github.com/reearth/ygo/blob/main/CHANGELOG.md) for full details.
