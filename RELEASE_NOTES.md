## What's new

Production-ready Redis transport for the `cluster.Relay` abstraction. With this release a multi-process ygo deployment behind a load balancer can share one logical document per room via Redis pub/sub — the canonical Hocuspocus `extension-redis` / y-hub topology, in pure Go.

### `cluster/redis` subpackage (#62)

```go
import (
    "github.com/redis/go-redis/v9"
    ygoredis "github.com/reearth/ygo/cluster/redis"
    ygws "github.com/reearth/ygo/provider/websocket"
)

rdb := redis.NewClient(&redis.Options{Addr: "redis:6379"})
relay, _ := ygoredis.New(rdb, ygoredis.Config{ChannelPrefix: "ygo:prod:"})
defer relay.Close()

srv := ygws.NewServer()
_ = srv.AttachRelay(relay)
```

- **Per-room pub/sub** — a node only receives traffic for rooms it hosts. Reference-counted at the relay layer.
- **Bounded back-pressure** — `OutboundBuffer` (default 256) decouples `Publish` from the Redis RPC so the CRDT transaction never blocks on network I/O.
- **Echo prevention** rides the existing provider-side sentinel guard. No special handling in the transport.
- **Self-describing wire format** — `VarUint(kind) + VarString(room) + VarBytes(data)`.

### Delivery semantics — fire-and-forget

Redis pub/sub is at-most-once. **A node that subscribes after a publish does not receive that publish.** The intended pattern pairs the relay with `VersionedPersistence` for catch-up state: on room activation, load the head state from persistence; from there the relay carries every subsequent edit. This is the same split Hocuspocus's `extension-redis` is conventionally deployed with.

For at-least-once delivery, Redis Streams would replace this adapter — tracked separately.

### Not in this release (intentional)

- **Distributed lock / writer election** (Redlock pattern). Persistence write coordination is a different concern from doc-update fan-out and belongs in the persistence layer on top of `VersionedPersistence`.
- **Redis Streams** as an at-least-once alternative — meaningful architectural shift, own design pass.
- **Redis cluster mode** (multi-shard pub/sub). Targets single-node / Sentinel deployments.

## Install

```
go get github.com/reearth/ygo@v1.21.0
```

See [CHANGELOG.md](https://github.com/reearth/ygo/blob/main/CHANGELOG.md) for the full entry and [docs/CLUSTERING.md](https://github.com/reearth/ygo/blob/main/docs/CLUSTERING.md) for setup and the catch-up-via-persistence pattern.
