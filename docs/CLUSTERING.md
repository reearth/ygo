# Cluster Relay

When you run more than one `websocket.Server` instance behind a load balancer,
peers connected to different nodes hold independent in-memory copies of each
document. Without coordination they never see each other's edits. The `cluster`
package provides a first-class relay abstraction that mirrors **both document
updates and awareness (presence)** across nodes, so a logical room behaves the
same no matter which node a peer lands on.

> This supersedes the older "clustered persistence adapter" pattern shown in
> [PERSISTENCE.md](PERSISTENCE.md#multi-node-deployment), which piggy-backed
> cross-node fan-out on `StoreUpdate` and **punted on awareness** (each node
> only knew its own peers' cursors). The relay carries awareness too, so remote
> cursors/selections converge cluster-wide. Prefer the relay for new
> deployments; the adapter pattern remains valid for document-only setups.

---

## Architecture

```
Browser A ─→ Node 1 ─┐                          ┌─→ Browser C
                     ├─ cluster.Relay (NATS /   ─┤
Browser B ─→ Node 1 ─┘   Redis / Kafka / …)      └─→ Browser D ─→ Node 2
```

Each node attaches a `cluster.Relay`. On every **local** change (a peer edit or
a server-side `Apply`), the node publishes an `Outbound` event to the relay. The
relay delivers it to every other node, which injects it into its own in-memory
room and rebroadcasts to that node's local peers. CRDT updates converge
regardless of delivery order, so no global ordering is required.

---

## Interfaces

```go
// github.com/reearth/ygo/cluster

type Kind int
const (
    KindSync      Kind = iota // CRDT document update (lib0 V1 blob)
    KindAwareness             // awareness/presence update
)

type Outbound struct {
    Room   string
    Kind   Kind
    Data   []byte
    Origin any // observer-local; used only to drop echoes, never serialised
}

type Inbound struct {
    Room string
    Kind Kind
    Data []byte
}

// Sink is the node-local surface a Relay drives to apply remote changes.
// *websocket.Server satisfies it directly.
type Sink interface {
    Inject(ctx context.Context, in Inbound) error
    Rooms() []string
    GetAwareness(room string) (*awareness.Awareness, bool)
    GetDoc(room string) *crdt.Doc
}

// Relay is the cross-process transport.
type Relay interface {
    Publish(ctx context.Context, out Outbound) error
    Start(ctx context.Context, sink Sink) error
    RoomActivated(room string)
    RoomDeactivated(room string)
    Close() error
}
```

`*websocket.Server` implements `cluster.Sink` (`Inject`, `Rooms`,
`GetAwareness`, `GetDoc`) — there is a compile-time assertion in the provider
package — so you pass the server straight to the relay; you only implement
`Relay` for your transport of choice.

---

## Attaching a relay

There is no functional-options constructor; attach the relay to a constructed
server:

```go
import (
    "github.com/reearth/ygo/cluster"
    ws "github.com/reearth/ygo/provider/websocket"
)

srv := ws.NewServer() // or NewServerWithPersistence(adapter)

relay := cluster.NewMemRelay() // or your own Relay
if err := srv.AttachRelay(relay); err != nil {
    log.Fatal(err)
}

http.Handle("/yjs/{room}", srv)
```

`AttachRelay`:

- must be called **once**, before serving connections (a second call returns
  `ErrRelayAlreadyAttached`);
- calls `relay.Start(ctx, srv)` with a context cancelled on `Server.Shutdown`;
- wires `doc.OnUpdate` + `awareness.OnChange` for every room as it becomes
  resident (and calls `RoomActivated`), and unwires them on eviction (calling
  `RoomDeactivated`). `Server.Shutdown` cancels the relay context and calls
  `relay.Close()`.

When a remote change arrives, the relay calls `srv.Inject(ctx, Inbound)`:

- `KindSync` → applied to the room's doc with the relay sentinel origin, then
  rebroadcast to local peers via `BroadcastUpdate`.
- `KindAwareness` → merged into the room's `awareness.Awareness` with the
  sentinel origin, then fanned out to local peers.

`Inject` auto-creates the room if it is not yet resident, so a node with no
local peers for a room still materialises the converged state (a peer that later
connects there receives it via sync step-2). Such rooms linger like
`Apply`-created ones until `CloseRoom` or process exit.

---

## The echo guard (origin sentinel)

The danger in any relay is an infinite loop:

```
Node A publishes ─→ Node B injects ─→ B's local observer publishes it back
        ↑                                                        │
        └────────────────── Node A injects ←─────────────────────┘
```

ygo breaks this with an **origin sentinel**, the same pointer-identity trick
`Server.Apply` uses for its own writes:

1. `AttachRelay` allocates a private sentinel value (`new(struct{})`) per server.
2. When the relay injects a remote change, it applies it with that sentinel as
   the `origin` (`crdt.ApplyUpdateV1(doc, data, sentinel)` /
   `aw.ApplyUpdate(data, sentinel)`).
3. The per-room `doc.OnUpdate` / `awareness.OnChange` observers that drive
   `Publish` compare the change's origin against the sentinel **by pointer
   identity** and **drop matches** — a relay-injected change is never
   re-published.

So a change crosses the cluster exactly once. Re-injection of a node's own
change (if a relay happens to echo it back) is harmless: CRDT updates are
idempotent/commutative, and because it now carries the sentinel origin it is not
re-published. The sentinel is process-local and **never crosses the wire** —
`Outbound.Origin` is observer metadata only; `Inbound` has no `Origin` field.

---

## Reference implementation: `MemRelay`

`cluster.MemRelay` is an in-process, channel-backed `Relay`. Multiple
`websocket.Server` instances in one process share a single `MemRelay`; a change
published by one is delivered to every other node's `Sink`. It is the reference
implementation and the basis for the relay test suite.

```go
relay := cluster.NewMemRelay(cluster.WithBufferSize(1024)) // default buffer 256

a := ws.NewServer()
b := ws.NewServer()
_ = a.AttachRelay(relay)
_ = b.AttachRelay(relay) // each node Start()s itself on the shared relay
```

Characteristics:

- **Asynchronous delivery**: `Publish` enqueues to a per-node buffered channel
  drained by a goroutine; each node processes its deliveries in order.
- **No per-room subscription**: `RoomActivated`/`RoomDeactivated` are no-ops;
  every node receives every room's traffic and applies only the rooms it hosts.
  (A production relay keyed by a real broker should subscribe per room.)
- **Lifecycle**: `Close` stops all delivery goroutines and rejects further
  `Publish`/`Start` (`ErrRelayClosed`); `Publish` before any `Start` returns
  `ErrRelayNotStarted`.

`MemRelay` is ideal for tests and single-process multi-server simulations. For a
real cluster, implement `Relay` over your message bus (NATS, Redis Streams,
Kafka, Google Pub/Sub, …): `Publish` serialises `Outbound{Room,Kind,Data}` to a
subject/topic; the subscriber reconstructs an `Inbound` and calls
`sink.Inject`. Keep `Outbound.Origin` local — do not put it on the wire.

---

## Implementing a custom Relay

A minimal broker-backed relay:

```go
type BrokerRelay struct {
    bus  Bus // your pub/sub client
    sink cluster.Sink
    // …
}

func (r *BrokerRelay) Start(ctx context.Context, sink cluster.Sink) error {
    r.sink = sink
    return r.bus.Subscribe(ctx, "ygo.cluster", func(raw []byte) {
        room, kind, data := decode(raw) // your framing
        _ = r.sink.Inject(ctx, cluster.Inbound{Room: room, Kind: kind, Data: data})
    })
}

func (r *BrokerRelay) Publish(ctx context.Context, out cluster.Outbound) error {
    // Origin is intentionally NOT serialised.
    return r.bus.Publish(ctx, "ygo.cluster", encode(out.Room, out.Kind, out.Data))
}

func (r *BrokerRelay) RoomActivated(room string)   { /* optional: subscribe per room */ }
func (r *BrokerRelay) RoomDeactivated(room string) { /* optional: unsubscribe */ }
func (r *BrokerRelay) Close() error                { return r.bus.Close() }
```

Notes:

- Do **not** re-implement the echo guard inside your relay; the provider wiring
  already drops sentinel-origin changes before `Publish`. If your broker echoes
  a publisher's own messages back, the sentinel makes that harmless — but you
  may exclude the publisher to save bandwidth.
- `Publish` should be non-blocking or bounded; a slow broker must not stall the
  CRDT transaction that triggered it.
- Combine the relay with a `PersistenceAdapter` /
  [`VersionedPersistence`](PERSISTENCE.md) for durability — the relay handles
  live fan-out (including awareness), persistence handles restart recovery.

---

## Redis adapter (`cluster/redis`)

`cluster/redis` is a turnkey production relay backed by Redis pub/sub.
It's the recommended starting point for multi-process deployments behind
a load balancer — drop it in front of `AttachRelay` and your existing
single-server code becomes horizontally scalable.

```go
import (
    "github.com/redis/go-redis/v9"
    ygoredis "github.com/reearth/ygo/cluster/redis"
    ygws "github.com/reearth/ygo/provider/websocket"
)

rdb := redis.NewClient(&redis.Options{Addr: "redis:6379"})
relay, err := ygoredis.New(rdb, ygoredis.Config{
    // ChannelPrefix isolates this deployment from any other ygo
    // clusters sharing the same Redis. Defaults to "ygo:cluster:".
    ChannelPrefix: "ygo:prod:",
})
if err != nil { /* ... */ }
defer relay.Close()

srv := ygws.NewServer()
_ = srv.AttachRelay(relay)
```

**Per-room channels.** Each room subscribes to its own Redis channel —
`<prefix><room>`. `RoomActivated` SUBSCRIBES; `RoomDeactivated`
UNSUBSCRIBES. A node only receives traffic for rooms it actually hosts,
which scales cleanly when only a subset of rooms are hot on each node.

**Wire format.** `VarBytes(nodeID) + VarUint(kind) + VarString(room) +
VarBytes(data)` — self-describing and stable across go-redis versions.
The `nodeID` is a per-relay 16-byte identifier (auto-generated in `New`,
or supply via `Config.NodeID`) used to suppress self-delivery: Redis
pub/sub mirrors every publish back to the publisher's own subscription,
and the subscriber drops payloads whose nodeID matches its own before
calling `Sink.Inject`. The provider-side sentinel guard remains the
authoritative echo defence; the self-skip is a perf optimisation that
avoids the local decode + apply + observer round trip. `Origin` is
observer-local and intentionally never serialised (per the
`cluster.Relay` package contract).

**Bounded back-pressure.** Internally, `Publish` enqueues to a bounded
channel (default 256) drained by a dedicated publisher goroutine. When
the queue is full, `Publish` blocks until a slot frees, the caller's
ctx cancels, the relay closes, or the bound start context (the one
passed to `AttachRelay`/`Start`) is cancelled — surfacing as a clean
`ErrRelayClosed` rather than hanging. The inbound side has its own
buffer too (`Config.ChannelSize`, default 1024); size it for the
busiest expected room since go-redis silently drops messages when this
fills.

**Reference-counted activation.** `RoomActivated` / `RoomDeactivated`
are reference-counted at the relay layer, so duplicate calls collapse
into a single SUBSCRIBE / UNSUBSCRIBE round trip. The underlying Redis
RPCs are held under the relay's lifecycle mutex so concurrent calls for
the same room can never reorder the pub/sub state.

### Delivery semantics — fire-and-forget

Redis pub/sub is at-most-once. **A node that subscribes *after* a
publish does not receive that publish — there is no replay.** The
practical implication: if a server starts (or activates a room) while
edits are flowing, those edits are lost on the new server unless it
catches up through a different path.

The intended pattern is to pair the relay with a persistence layer:

1. On room activation, load the head state from
   [`VersionedPersistence`](PERSISTENCE.md) (or the legacy
   `PersistenceAdapter`).
2. Once loaded, the relay carries every subsequent edit.

This split — relay for live fan-out, persistence for catch-up — is also
how Hocuspocus's `extension-redis` is conventionally deployed.

If a deployment needs at-least-once delivery (no catch-up dependency on
persistence), a Redis Streams-based adapter (`XADD` + last-read-id
tracking) would replace this one. Tracked separately; not in v1.21.0.

### What's *not* in this adapter

- **Distributed lock / writer election** (Redlock pattern). Persistence
  write coordination is a different concern from doc-update fan-out and
  is the right place to add Redlock on top of the existing
  `VersionedPersistence` interface — not in the relay.
- **Redis cluster mode** — go-redis supports it but pub/sub semantics
  differ; this adapter targets single-node Redis (or Sentinel). Filed
  separately if needed.

---

## Relay vs. persistence

| Concern                       | Cluster relay                              | Persistence adapter                         |
|-------------------------------|--------------------------------------------|---------------------------------------------|
| Live cross-node fan-out       | ✅ document **and** awareness               | document only (and only if you publish in `StoreUpdate`) |
| Awareness / presence          | ✅ first-class (`KindAwareness`)            | ❌ not carried                               |
| Durability across restart     | ❌ (ephemeral transport)                    | ✅                                           |
| Version history / rollback    | ❌                                          | ✅ with `VersionedPersistence`               |

Use both together in production: the relay for real-time multi-node sync, a
versioned persistence store for durability and history.
