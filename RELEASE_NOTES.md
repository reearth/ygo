## What's new

- **WebSocket provider hardening (#18, #19, #20).** Three additions for production WebSocket deployments:
  - **Configurable per-message size cap** via `Server.MaxMessageBytes`. Default 64 MiB (matching `yrs-warp`); lower for untrusted multi-tenant operators.
  - **Structured logging** via `Server.Logger *slog.Logger`. Slow-peer write failures, malformed sync messages, and awareness errors that were previously silent now surface at `Warn` level.
  - **Bounded per-peer broadcast queue** via `Server.PeerWriteQueueSize` (default 256). When a peer can't keep up, it's disconnected (matching `yrs-warp`'s pattern); CRDT pending-structs machinery from v1.2.0 handles reconnect-and-resync cleanly. Replaces the previous unbounded "goroutine per broadcast" fanout.

- **All additive.** Existing `Server` config and behavior preserved. Defaults match prior behavior except the broadcast goroutine pattern (now bounded; no user-visible regression for well-behaved peers).

## Install

```
go get github.com/reearth/ygo@v1.4.0
```

See [CHANGELOG.md](https://github.com/reearth/ygo/blob/main/CHANGELOG.md) for full details.
