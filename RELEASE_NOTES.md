## What's new

First Hocuspocus compatibility release. `provider/websocket` now accepts the seven additional message types Hocuspocus extends y-protocols with — so Hocuspocus-aware clients (Tiptap stateless extensions, custom liveness pings, application close signals) no longer have their frames silently dropped.

### Hocuspocus message types 4-10 (#55)

| Tag | Message | ygo behaviour |
|---|---|---|
| 4 | `SyncReply` | Apply locally, broadcast to other peers, never echo to sender. |
| 5 | `Stateless` | Fire `Server.OnStateless` with `IsBroadcast: false`. No broadcast. |
| 6 | `BroadcastStateless` | Fan out to other peers as `Stateless` (tag 5); fire `Server.OnStateless` with `IsBroadcast: true`. |
| 7 | `CLOSE` | Close the WebSocket connection; log the optional reason. |
| 8 | `SyncStatus` | Silently consume (server-to-client message). |
| 9 | `Ping` | Reply with single-byte `Pong` (tag 10). |
| 10 | `Pong` | Silently consume. |

### New public API

- `Server.OnStateless StatelessHook` — optional hook on `provider/websocket.Server`.
- `StatelessHook = func(StatelessInfo)` — invoked on the peer's read goroutine.
- `StatelessInfo` — carries `Room`, `Payload`, `IsBroadcast`.

### Framing limitation (intentional)

Hocuspocus's full client framing prepends a `VarString(docName)` to every frame so one WebSocket can multiplex multiple documents. ygo's framing remains the y-websocket layout (tag + payload), one document per WebSocket. This release adds the Hocuspocus message **types** on the existing y-websocket framing; Hocuspocus's multi-doc multiplex is a separate, larger architectural change not in scope here.

## Install

```
go get github.com/reearth/ygo@v1.18.0
```

See [CHANGELOG.md](https://github.com/reearth/ygo/blob/main/CHANGELOG.md) for full details.
