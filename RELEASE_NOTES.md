## What's new

Two goroutine leak fixes plus one performance cleanup. Recommended upgrade for any v1.4.0 deployment.

- **`runWriter` goroutine leak (#33)**: regression from v1.4.0. When a peer connected during a small race window where its target room was being deleted, the per-peer write goroutine could leak. Now correctly paired with cleanup on every code path.
- **`StartAutoExpiry` double-call leak (#34)**: calling `StartAutoExpiry` twice on the same `Awareness` would orphan the first goroutine. Now the previous one is stopped before the new one starts. Returned `stop` is also safe to call more than once.
- **Per-peer write goroutine spawn cleanup**: three call sites in the server-side inject paths (`BroadcastUpdate`, `Apply`) spawned a fresh goroutine per peer per write. After v1.4.0 made `peer.write()` non-blocking, these goroutines became wasteful and scrambled ordering. Now direct calls.

## Install

```
go get github.com/reearth/ygo@v1.4.1
```

See [CHANGELOG.md](https://github.com/reearth/ygo/blob/main/CHANGELOG.md) for full details.
