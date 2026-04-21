## What's new

- **Server-side document injection for AI agents and backend APIs (issue #8).** Three new methods on `*websocket.Server` let backend Go code push changes into a live room without simulating a WebSocket client:
  - `BroadcastUpdate(ctx, room, update)` — fan a pre-encoded V1 update out to all peers. Pair with `crdt.ApplyUpdateV1` to keep server state in sync.
  - `Apply(ctx, room, fn)` — run a callback, capture the delta via an origin-scoped `OnUpdate` subscription, and broadcast. Auto-creates the room. Persistence flows through the existing `OnUpdate` hook.
  - `CloseRoom(name, force)` — explicit teardown for peerless rooms.
- **`OnInject` hook** for access control. Receives `ctx` and an `InjectInfo` with `Op` (`OpBroadcastUpdate` | `OpApply`) and `UpdateSize`. Refusals wrap `ErrInjectRefused` so callers can match with `errors.Is`.
- **Resource caps:** `MaxUpdateBytes` (default 64 MiB) and `MaxRooms` (default unlimited; enforced uniformly across peer upgrades and Apply).
- **Typed error sentinels** for every new failure mode (`ErrRoomNotFound`, `ErrInvalidUpdate`, `ErrUpdateTooLarge`, `ErrTooManyRooms`, `ErrNoChanges`, `ErrRoomHasPeers`, `ErrServerShutdown`, `ErrInvalidRoomName`, `ErrInjectRefused`).
- **Shutdown fix** for persistence goroutines in peerless rooms — previously could hang `Shutdown`.

## Install

```
go get github.com/reearth/ygo@v1.1.0
```

See [CHANGELOG.md](https://github.com/reearth/ygo/blob/main/CHANGELOG.md) for full details.
