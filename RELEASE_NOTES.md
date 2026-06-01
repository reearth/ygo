## What's new

Second Hocuspocus-compatibility release. Closes out the server-side parity story started in v1.18.0: lifecycle hooks for the WebSocket server, and a new optional `provider/webhook` subpackage for forwarding events to external HTTP endpoints.

### `provider/websocket` lifecycle hooks (#60)

Four new optional hook fields on `Server`:

| Hook | Fires when |
|---|---|
| `OnLoadDocument(ctx, room, doc) error` | After the persistence adapter has bootstrapped the doc, before any peer can interact. Returning an error fails room creation. |
| `OnUnloadDocument(ctx, room)` | Room is evicted from the server map (last-peer-leaves or `CloseRoom`). |
| `OnFirstPeer(room)` | 0→1 peer transition (warm-up tasks). |
| `OnLastPeer(room)` | 1→0 peer transition (cool-down tasks). Fires before `OnUnloadDocument`. |

All hooks fire after server locks are released, so implementations may block on I/O without contending with other peers.

### `provider/webhook` subpackage (#61)

POSTs ygo document events to a configurable HTTP endpoint. Mirrors Hocuspocus's `extension-webhook`:

- **HMAC-SHA256 signing** — every request carries `X-YGo-Signature-256: sha256=<hex>`. `webhook.VerifySignature` for receivers; constant-time comparison.
- **Debounce / coalescing** — rapid same-room updates collapse into a single POST carrying the latest update bytes. Default 1s, capped at 10s.
- **Retry with exponential backoff** — 5 attempts × 250ms base by default on 5xx and transport errors. 4xx drops immediately.
- **Drain on Close** — `webhook.Close(ctx)` flushes pending events before returning; events enqueued after Close are silently dropped.

```go
wh, _ := webhook.New(webhook.Config{
    URL:      "https://hooks.example.com/ygo",
    Secret:   []byte("shared-secret"),
    Debounce: time.Second,
})
defer wh.Close(context.Background())

srv.OnLoadDocument = func(_ context.Context, room string, doc *crdt.Doc) error {
    doc.OnUpdate(func(update []byte, _ any) {
        wh.Enqueue(webhook.Event{
            Type: webhook.EventUpdate, Room: room, Update: update,
        })
    })
    return nil
}
```

## Install

```
go get github.com/reearth/ygo@v1.19.0
```

See [CHANGELOG.md](https://github.com/reearth/ygo/blob/main/CHANGELOG.md) for full details.
