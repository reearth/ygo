## What's new

Read-only WebSocket connections. The WebSocket provider gains a richer auth entry
point, `Server.Authorize`, that can mark a connection read-only — it receives
document and awareness broadcasts but its inbound writes are dropped server-side.
This matches Hocuspocus's `readOnly` connection flag and covers public-read docs,
viewer roles, and monitoring connections. Additive: the existing `AuthFunc` is
unchanged and connections stay read-write unless you opt in.

### Added

- **`websocket.Server.Authorize func(*http.Request) (ConnectionConfig, bool)`** —
  accepts/rejects a connection (false → 401) *and* reports its config. Takes
  precedence over `AuthFunc` when both are set. (#59)
- **`websocket.ConnectionConfig{ ReadOnly bool }`** — per-connection config
  returned by `Authorize`; extensible for future settings. (#59)

### Behaviour

- A **read-only** peer still receives broadcasts, gets a `SyncStep2` in reply to
  its `SyncStep1`, and can query awareness — but its inbound document writes
  (`SyncStep2`/`Update`, Hocuspocus `SyncReply`) and awareness updates are dropped
  server-side. Stateless signals are not gated. Connections authorized via
  `AuthFunc` (or with no auth hook) remain read-write.

## Install

```
go get github.com/reearth/ygo@v1.30.0
```

See [CHANGELOG.md](https://github.com/reearth/ygo/blob/main/CHANGELOG.md) for full details.
