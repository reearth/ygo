## What's new

Second Hocuspocus-compatibility release. Closes out the server-side parity story started in v1.18.0: lifecycle hooks for the WebSocket server, and a new optional `provider/webhook` subpackage for forwarding events to external HTTP endpoints.

### `provider/websocket` lifecycle hooks (#60)

Four new optional hook fields on `Server`:

| Hook | Fires when | Locking |
|---|---|---|
| `OnLoadDocument(ctx, room, doc) error` | After the persistence adapter has bootstrapped the doc, before any peer can interact. Returning an error fails room creation. | **Under the server room-map lock** (same as `PersistenceAdapter.LoadDoc`). Return promptly; defer heavy I/O to a goroutine. |
| `OnUnloadDocument(ctx, room)` | Room is evicted from the server map (last-peer-leaves or `CloseRoom`). | After locks released; safe to block on I/O. |
| `OnFirstPeer(ctx, room)` | 0→1 peer transition (warm-up tasks). | After locks released. |
| `OnLastPeer(ctx, room)` | 1→0 peer transition (cool-down tasks). Fires before `OnUnloadDocument`. | After locks released. |

All four hooks are panic-safe: a `recover()` wraps each invocation and logs the panic + stack via the server logger.

### `provider/webhook` subpackage (#61)

POSTs ygo document events to a configurable HTTP endpoint. Mirrors Hocuspocus's `extension-webhook`:

- **`webhook.AttachTo(srv, wh)`** wires every relevant Server hook + per-doc `OnUpdate` in one call. Returns an idempotent detach func.
- **HMAC-SHA256 signing** — every request carries `X-YGo-Signature-256: sha256=<hex>`. `webhook.VerifySignature` for receivers; constant-time comparison.
- **Debounce / coalescing** — events with the same `(Room, Type)` pair collapse into a single POST. Different event types for the same room never collapse into each other.
- **Retry with exponential backoff and jitter** — 5 attempts × 250ms base by default on 5xx and transport errors, capped at `MaxBackoff` (default 30s) with ±20% jitter to defeat thundering-herd retry alignment. 4xx drops immediately.
- **Bounded delivery concurrency** — `MaxConcurrentDeliveries` (default 8) keeps a slow receiver from spawning unbounded goroutines under burst.
- **Drain on Close** — `webhook.Close(ctx)` flushes pending events before returning; events enqueued after Close are silently dropped.

```go
wh, _ := webhook.New(webhook.Config{
    URL:      "https://hooks.example.com/ygo",
    Secret:   []byte("shared-secret"),
    Debounce: time.Second,
})
defer wh.Close(context.Background())

srv := ygws.NewServer()
detach := webhook.AttachTo(srv, wh) // wires Load/Update/Unload/Connect/Disconnect
defer detach()
```

## Install

```
go get github.com/reearth/ygo@v1.19.0
```

See [CHANGELOG.md](https://github.com/reearth/ygo/blob/main/CHANGELOG.md) for full details.
