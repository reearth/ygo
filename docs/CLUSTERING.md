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
- **One goroutine per node, no per-room isolation**: `memNode.run` calls
  `sink.Inject` inline, for every room, from that single per-node goroutine —
  so a slow `Inject` for one room stalls delivery for **every other room**
  on that node. This is exactly the head-of-line-blocking defect #187 fixed
  for `cluster/redis` (each room gets its own worker there); `MemRelay` does
  not (yet) get that isolation — it is the in-process reference
  implementation, not a production transport.
- **No per-room subscription**: `RoomActivated`/`RoomDeactivated` are no-ops;
  every node receives every room's traffic and applies only the rooms it hosts.
  (A production relay keyed by a real broker should subscribe per room.)
- **Lifecycle**: `Close` stops all delivery goroutines and rejects further
  `Publish`/`Start` (`ErrRelayClosed`); `Publish` before any `Start` returns
  `ErrRelayNotStarted`.

`MemRelay` is ideal for tests and single-process multi-server simulations —
keep the one-goroutine-per-node head-of-line limitation above in mind if a
simulation involves a deliberately slow `Sink.Inject` for one room. For a
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
- `Publish` should still be non-blocking or bounded — a slow broker is bad
  practice regardless — but since #187 it no longer stalls the CRDT
  transaction that triggered it directly: the provider buffers per room and
  drives `Publish` from that room's own worker goroutine, off the Transact
  commit path. What a slow, unbounded `Publish` *does* stall is delivery for
  that one room specifically (and, once its lane saturates, the room's own
  commit path pays an occasional bounded merge cost — see "Observability:
  `Stats()`" below for the vocabulary this shows up under).
- `Publish` may be called **concurrently for distinct rooms**, and — briefly,
  across a room's eviction/reload handoff — **twice concurrently for the same
  room**. It must also **return promptly when its ctx is cancelled** —
  `Server.Shutdown` depends on that to stay bounded. See "Contract:
  concurrent calls and cancellation" below; your `Relay` must be safe for all
  three.
- Combine the relay with a `PersistenceAdapter` /
  [`VersionedPersistence`](PERSISTENCE.md) for durability — the relay handles
  live fan-out (including awareness), persistence handles restart recovery.

### Contract: concurrent calls and cancellation

Both halves of `cluster.Relay`/`cluster.Sink` now permit concurrency that a
relay written against the pre-#187 provider could safely assume away, and
since #202 `Publish` carries a cancellation obligation. Anyone implementing
a custom `Relay` (or a custom `Sink`, though the shipped `*websocket.Server`
already handles this) needs all three of these:

- **`Sink.Inject` may be called concurrently for distinct rooms.** Calls for
  the *same* room must still be serialised and delivered in publish order.
  `*websocket.Server` is already safe for this (it is the same path
  concurrent peer connections already take). This is a permission the
  interface grants, not a guarantee every relay exercises: `cluster/redis`
  takes advantage of it (one worker per room, so one slow room's `Inject`
  can't stall another's); `MemRelay` does not (see its Characteristics
  above).
- **`Relay.Publish` may be called concurrently for distinct rooms, and two
  concurrent calls for the SAME room are possible.** `provider/websocket`
  drives `Publish` from one worker goroutine per room, so distinct rooms are
  expected to overlap; additionally, across a room's eviction/reload
  handoff, a predecessor lane's final drain can briefly overlap with the
  successor lane's worker publishing for the same room name. Unlike
  `Inject`, this means `Publish` implementations must tolerate that
  same-room overlap too — the contract imposes no per-room ordering on
  `Publish` at all. This was reviewed and accepted as benign because
  `KindSync` payloads are commutative/idempotent V1 update blobs regardless
  of arrival order, and a stale `KindAwareness` payload is dropped by the
  receiving `Awareness`'s own per-client clock gate — but a `Relay`
  implementation still has to be safe for the concurrent calls themselves.
  Both shipped relays already are: `MemRelay.Publish` snapshots its node
  list under its own mutex, releases it, then sends on per-node channels;
  `cluster/redis`'s `Publish` deliberately takes no lock at all and uses
  atomics plus channels.
- **`Relay.Publish` must return promptly once its ctx is cancelled** (#202,
  v1.46.0), returning the ctx error for whatever it could not deliver.
  `Server.Shutdown` relies on this to unwedge a blocked `Publish` after the
  caller's deadline: it drains each room's outbound lane while the relay
  context is still live, joins the lane workers bounded by `Shutdown`'s own
  ctx, then cancels the relay context — and counts every payload an aborted
  `Publish` abandons in `RelayStats().Dropped`. A `Publish` that ignores
  cancellation stalls that join and leaves its worker goroutine running past
  `Shutdown`. Both shipped relays conform: `MemRelay` selects on ctx around
  its per-node channel sends, and `cluster/redis`'s `Publish` selects on ctx
  at the hand-off to its publisher goroutine. A custom relay whose broker
  client offers no cancellable send should wrap the send so the `Publish`
  call itself can still abandon it and return.

A relay that appends to an unsynchronised buffer, or keeps an unlocked
per-room sequence counter, on either the inbound or outbound side, is not
safe under this contract and must add its own synchronisation.

---

## Redis adapter (`cluster/redis`)

`cluster/redis` is a turnkey production relay backed by Redis. It's the
recommended starting point for multi-process deployments behind a load
balancer — drop it in front of `AttachRelay` and your existing
single-server code becomes horizontally scalable.

It offers two delivery tiers, selected by `Config.Transport`: pub/sub
(the default, at-most-once) and Redis Streams (at-least-once within a
bounded window). The subsections immediately below describe the pub/sub
tier and the lifecycle both tiers share; the tier choice, its cost, and
the migration path between the two are in
[Delivery tiers: pub/sub and Streams](#delivery-tiers-pubsub-and-streams).

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
buffer too (`Config.ChannelSize`, default 1024): go-redis feeds every
subscribed message onto it and only drops a message if a send to it
blocks for the client's `ChanSendTimeout` (go-redis defaults this to
60 seconds — not immediately on a full buffer). Since #187 the
subscriber is a thin router that decodes and hands each message
straight to its room's lane (`Lane.Push`, which never blocks) without
ever calling `Sink.Inject` itself, so the channel should no longer
back up from a slow `Inject` in the first place; raise `ChannelSize`
only if you expect bursts large enough to outrun that per-room
hand-off.

**Reference-counted activation.** `RoomActivated` / `RoomDeactivated`
are reference-counted at the relay layer, so duplicate calls collapse
into a single SUBSCRIBE / UNSUBSCRIBE round trip. The underlying Redis
RPCs are held under the relay's lifecycle mutex so concurrent calls for
the same room can never reorder the pub/sub state.

### Delivery semantics — pub/sub is fire-and-forget

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

If a deployment cannot tolerate that, the same adapter also ships an
at-least-once tier backed by Redis Streams — see
[Delivery tiers: pub/sub and Streams](#delivery-tiers-pubsub-and-streams)
below. It does not replace pub/sub; you select one with
`Config.Transport`.

**`Shutdown` drains queued outbound updates, and counts what it cannot
deliver** (#202). `Server.Shutdown` used to cancel the relay context as its
second act — before closing peer connections and before the persistence
drain — so everything peers committed for the rest of shutdown was discarded
with neither `Dropped` nor `HardDrops` incrementing. Since the #202 fix the
relay context is cancelled at the *end* of `Shutdown`: each room's outbound
lane is retired and drained while publishing still works, the lane workers
are joined (bounded by `Shutdown`'s ctx, so no `Publish` call outlives a
`Shutdown` that completed within its budget), and only then is the context
cancelled. A backlog that cannot be delivered before the caller's ctx
expires is counted in `RelayStats().Dropped` instead of vanishing. `Dropped`
and `HardDrops` both reading zero after a `Shutdown` therefore means what it
should: nothing was lost. Give `Shutdown` a real deadline — it is also the
delivery budget for the final outbound tail.

### Observability: `Stats()`

Since #187, both sides of the relay expose health counters — watch these,
not just error logs, since a saturated lane degrades silently otherwise:

- **Inbound** — `cluster/redis.Relay.Stats()` returns `Coalesced`,
  `AwarenessSuperseded`, `HardDrops`, and `RouterDrops`.
- **Outbound** — `websocket.Server.RelayStats()` returns `Coalesced`,
  `AwarenessSuperseded`, `HardDrops`, and `Dropped`.

Alerting posture is the same on both sides:

- `HardDrops` (inbound and outbound) and `Dropped` (outbound) **should
  always be zero** — alert on presence. They mean an update was actually
  lost and nodes may have diverged.
- `Coalesced` and `AwarenessSuperseded` are **routine** on a busy room —
  they increment on ordinary backlog merges / superseded heartbeats, well
  before a lane is anywhere near saturated. Alert on their *rate* trending
  up, not on their mere presence.
- `RouterDrops` (inbound only) is likewise **routine** under ordinary room
  churn — a message for a room this node no longer hosts arriving just
  after `RoomDeactivated`, or one whose `Kind` this node does not recognise.
  Alert on its rate, not its presence.

`Coalesced`, `AwarenessSuperseded`, and `HardDrops` are **monotonic but not
exact**: they are guaranteed to never decrease across the life of the
relay/server, but a small number of documented, benign race windows around
room retirement can *undercount* a value (never overcount, never decrease
relative to a total already returned). See `Relay.Stats()`'s and
`RelayStats()`'s own doc comments for the exact windows if you need the
detail.

`RouterDrops` (inbound) and `Dropped` (outbound) are different: each is a
single atomic counter on a direct increment path with no fold-on-retirement
step, so both are **exact**, not just monotonic.

### Delivery tiers: pub/sub and Streams

`cluster/redis` moves payloads over one of two Redis mechanisms, selected by
`Config.Transport`. The zero value is `PubSub`, so an existing deployment that
never sets the field behaves exactly as it always has.

```go
relay, err := ygoredis.New(rdb, ygoredis.Config{
    Transport: ygoredis.Streams, // PubSub (default) | Streams | Both
    // Everything below is optional; these are the defaults.
    StreamPrefix:       "ygo:stream:",
    StreamRetention:    60 * time.Second, // age bound, enforced by the MINID sweeper
    StreamMaxLen:       4096,             // entry bound, enforced inline on XADD
    AwarenessMaxLen:    64,
    AwarenessRetention: 10 * time.Second,
    Readers:            4,                // goroutines multiplexing this node's rooms
    TrimInterval:       30 * time.Second, // must be < StreamRetention
    ReadBlock:          250 * time.Millisecond,
})
```

In `Streams` and `Both` mode, `New` **rejects** a configuration it cannot
honour rather than silently adjusting it (in `PubSub` mode it validates none of
these fields, so an existing caller cannot start failing construction because a
new field exists):

- an unknown `Transport`;
- a client `PoolSize` not greater than `Readers` — a reader holds a pool
  connection for as long as its `XREAD` blocks, so a pool that small leaves
  publishes and the trim sweeper waiting on a connection every cycle;
- a `TrimInterval` not less than `StreamRetention` — a sweeper slower than the
  window it enforces cannot enforce it;
- a `ReadBlock` outside **1ms – 250ms**. The ceiling is 250ms because a blocked
  `XREAD` cannot be interrupted (go-redis arms the socket deadline from the
  context *deadline*, and never watches for cancellation), so `ReadBlock` is
  simultaneously the upper bound on how long `Close` waits for a reader to
  notice shutdown and on how long a newly activated room waits before its
  stream is read at all. Lowering it shortens both at the cost of more commands
  per second; raising it is not offered, because those latencies could not then
  be delivered. The floor is 1ms because go-redis truncates a sub-millisecond
  value to `BLOCK 0`, which Redis reads as *block forever*.

Delivery latency for a room already being read is **not** bounded by
`ReadBlock`: an `XREAD` returns as soon as any of its keys gets an entry.

#### What each tier guarantees

| | `PubSub` (default) | `Streams` |
|---|---|---|
| Mechanism | `PUBLISH` / `SUBSCRIBE`, one channel per room | `XADD` / `XREAD`, two streams per room |
| Guarantee | **at-most-once** | **at-least-once within `min(StreamRetention, StreamMaxLen / publish-rate)`** |
| Late joiner / restart | no replay — catch up via persistence | replays the retained window |
| Backpressure response | merge the backlog, or drop it | leave the entries in Redis and re-read |
| Redis cost | none beyond the fan-out itself | a write: replication, AOF/RDB, and memory |

**`PubSub` is at-most-once by Redis's own definition.** A subscriber that
cannot keep up — an error, a network blip, or Redis disconnecting a client
whose output buffer overflowed — loses the message for good, and no amount of
client-side buffering changes that. Because CRDT updates carry causal
dependencies, one lost update parks every *later* edit from that client on the
receiving node: the apply reports no error, the node just stops converging for
that client until the room is next reloaded from persistence.

**`Streams` is at-least-once, and the window is bounded.** Each room gets two
Redis streams — `<StreamPrefix>s:<room>` for document updates and
`<StreamPrefix>a:<room>` for awareness. `Publish` `XADD`s inline on the
caller's goroutine, so a failed write fails the call instead of being
swallowed. Readers `XREAD` a room's sync stream **from its oldest retained
entry** rather than from the tail: V1 updates are idempotent and commutative,
so replay is harmless, which removes the race between loading a snapshot and
starting to read, and makes late-joiner catch-up fall out for free. No cursor
is persisted anywhere — a lost cursor costs a replay, not a loss.

This is **not** a no-loss guarantee, and the docs will not call it one. A
stream is trimmed from both ends of the same window: `StreamMaxLen` is enforced
inline on every `XADD` (approximately — `MAXLEN ~`, which keeps *more* entries
than asked, never fewer), and `StreamRetention` is enforced by a periodic
`XTRIM MINID` sweeper. Whichever binds first is the real window, hence
`min(StreamRetention, StreamMaxLen / publish-rate)`: on a hot room 4096 entries
may be only a few seconds' worth. A reader that lags past its room's window
loses whatever was trimmed underneath it — and, unlike pub/sub, *says so*: a
per-node, per-stream sequence number in every entry makes that loss provable
and it is counted in `StreamStats().Gaps`. Size `StreamRetention` and
`StreamMaxLen` for the worst lag you intend to survive (a deploy rollover, a GC
pause, a node restart); the 60s default matches y-redis's own
`REDIS_MIN_MESSAGE_LIFETIME`.

**Awareness is a separate stream, read from the tail, and never replayed.**
Sharing the sync stream would let high-frequency presence traffic evict sync
entries out of the retention window — silently shrinking the very guarantee the
tier exists to provide — and replaying presence would resurrect clients that
are long gone. Presence is idempotent heartbeat state, so a skipped awareness
entry is re-sent within one heartbeat interval; awareness entries are therefore
excluded from gap accounting on purpose. One residual is worth knowing about
rather than glossing: across a room's eviction/reload handoff, an in-flight
awareness read is fenced against the room's *current* residency, but the fence
**narrows that race, it does not close it** — a payload accepted microseconds
before the handoff lands can still reach the new occupants, bounded at one
in-flight read times one drain window. It is the same window the pub/sub path
has always had, and presence self-heals on the next interval.

#### The cost trade — pub/sub is supported, not deprecated

`PUBLISH` costs nothing: Redis fans the payload out to whoever is connected and
forgets it. `XADD` is a **write** — it replicates, it lands in AOF/RDB if
persistence is on, and it holds memory proportional to
`retention × publish-rate × update size` for every room on the node, plus the
`MAXLEN ~` overshoot. The default 4096 entries is roughly 800KB per hot room at
200-byte updates.

**Size for distinct rooms *ever* used, not for concurrently active rooms.**
`MAXLEN ~` and the `MINID` sweeper bound the size of each stream; neither
bounds the *number* of streams, and this package never sets an `EXPIRE`. The
sweeper only visits rooms some node currently holds, so once a room has gone
idle everywhere, its two stream keys are swept nowhere and keep whatever they
last held for the life of the Redis instance. Redis memory therefore grows
monotonically with the number of distinct rooms that have ever been published
to. If your room identifiers are long-lived and bounded — a fixed set of
documents — this is a one-off ceiling you can multiply out. If they are
per-session, per-tenant, or otherwise unbounded, budget for the whole history
or reclaim the keys yourself (`XTRIM`/`DEL`/`EXPIRE` from an operations job
against `StreamPrefix*`) until the built-in reclaim lands: setting an `EXPIRE`
on each `XADD`, so an untouched key falls out on its own with no keyspace scan
and nothing to coordinate between nodes, is the intended fix and is tracked as
a follow-up issue.

The tier also adds a steady command floor: `Readers × (1 /
ReadBlock)` blocking `XREAD`s per second per node even when nothing is
happening (4 readers at 250ms is 16/s), and one `XTRIM MINID` per stream per
`TrimInterval`.

So `PubSub` is not a legacy mode and is not deprecated. **At-most-once is a
legitimate choice** when your rooms are hot, your Redis is sized for fan-out
rather than for writes, and persistence-on-reload is an acceptable healer.
Choose `Streams` when a missed update between reloads is not acceptable, and
pay for it in Redis memory and write throughput.

#### Migration path

**A `Streams`-only node does not `PUBLISH`, so a `PubSub`-only node never sees
its edits.** The two transports do not interoperate, and you should not rely on
the asymmetry in the other direction either. Flipping a live cluster from
`PubSub` straight to `Streams` therefore splits it for the length of the roll.

Use `Both`, which publishes to *and* reads from both tiers:

1. Roll every node from `PubSub` to `Both`. Every node can now hear every other
   node, whichever tier it publishes on.
2. Once no `PubSub`-only node remains, roll every node from `Both` to `Streams`.

To go back, reverse it: roll to `Both`, then to `PubSub`.

`Both` needs **no deduplication**. A payload delivered twice — once over each
tier — is a V1 update blob, which is idempotent and commutative, so applying it
again is a no-op. Awareness is delivered twice too and is absorbed by the
receiving `Awareness`'s own per-client clock gate. Both inbound paths hand off
to the same per-room lane and so to the same single worker goroutine, which is
what keeps `Sink.Inject` serialised per room as the contract requires. What
`Both` does cost is double the publish work and double the inbound traffic, so
it is a migration state rather than a destination.

The stream key layout — the `s:`/`a:` discriminator *before* the room name —
was chosen because a room name may legally contain any printable character,
`:` included, and any suffix-based scheme lets a room called `a:foo` forge
another room's awareness key.

#### Redis Cluster is not supported

Both tiers target single-node Redis or Sentinel. `New` takes a
`*redis.Client`, so a `*redis.ClusterClient` will not even compile in, and for
the Streams tier that exclusion is deliberate rather than merely unfinished:

- A multi-key `XREAD` requires every key in the command to live in **one hash
  slot**. Rooms hash to arbitrary slots, so the reader's whole batching design
  (up to 256 rooms per `XREAD`) collapses to one command per room.
- Forcing the slot with a hash tag means baking the shard assignment into the
  key names — and the assignment depends on `Readers`. Two nodes configured
  with different `Readers` values would then compute *different key names for
  the same room* and silently address different streams. That is split-brain
  produced by a config typo, which is a worse failure than the one this tier
  exists to fix.

Scale Redis vertically, or use Sentinel for availability. If one Redis cannot
hold the write rate, run independent ygo deployments — each with its own Redis
and its own `StreamPrefix` — behind a room-aware router, rather than reaching
for Cluster.

#### Observability: `StreamStats()`

`Relay.StreamStats()` is separate from `Relay.Stats()` on purpose: `Stats`'
fields are permanently zero under `Streams` and these are permanently zero
under `PubSub`, and a metric that is meaningless half the time stops being
trusted. It is safe to call on a `PubSub` relay, where every field reads zero.
Every counter is monotonic for the life of the relay, so scrape rates — except
`Gaps`, where presence alone is the signal.

- **`Gaps` — alert on presence.** A source node's sequence number jumped, which
  means entries existed and were trimmed before this reader reached them. A
  single gap means data was lost and the retention window was too small for the
  reader's actual lag. This is the one counter that should always be zero.
- **`Replayed` — a merge/batching gauge; do *not* alert on its rate.** It
  counts `len(batch)-1` for every multi-entry batch a reader folded into one
  merged update — whether or not any of those entries had been delivered
  before. It is therefore not a count of re-deliveries and says nothing about
  cursor loss: any room taking more than one remote update per `ReadBlock`
  (250ms by default) batches on every cycle, so a busy room shows a permanent,
  perfectly healthy rate. What it is good for is inbound volume — divided by
  the read rate it approximates merged entries per cycle, so a step change
  tracks a step change in cluster write traffic, and a catch-up burst
  (activation, a node restart, a stall clearing) reads as a spike against that
  room's own baseline. Because that baseline exists, an absolute threshold on
  it is meaningless. `Gaps`, `Stalled` and `Deferred` are the counters that
  report loss and lag.
- **`Stalled` — alert on a sustained rate.** Cursor advances declined because a
  room's inbound lane was full. Routine in bursts, and declining is the *safe*
  response — the entries stay in the stream and are read again next cycle, so
  nothing is discarded and the lane is not made to coalesce. What it costs is
  lag, and lag is only safe while the entries survive it: a stall that outlasts
  the delivery window shows up as `Gaps`. A sustained rate means a room's
  consumer cannot keep up.
- **`Deferred` — alert on a sustained rate.** Cursor advances declined because
  the room had no inbound delivery worker yet (the brief window inside
  `RoomActivated` between a room joining a reader's assignment set and its
  worker existing). Routine and self-clearing; a sustained rate is an
  activation bug, and no amount of capacity fixes it — which is why it is not
  folded into `Stalled`.
- **`Trimmed` — informational.** Entries removed by the `MINID` sweeper.
  Routine; it is the retention window being enforced.
- **`Restarts` — informational.** Source nodes observed restarting, inferred
  from a sequence-number *decrease*. Expected once per node per deploy. It
  exists so that a restart is never miscounted as a `Gaps`.

`Stats()`' `RouterDrops` also serves the Streams tier: a malformed stream
entry, an unrecognised `Kind`, or an entry whose kind disagrees with the stream
it arrived on is dropped and counted there.

#### `NodeID` and self-healing after a restart

Entries carry the publishing relay's `NodeID` and readers skip their own, so a
node never pays the decode-and-inject round trip for its own writes. That makes
`Config.NodeID` a *recovery* decision under `Streams`, not just an identity one:

- **Leave it empty (the default).** `New` generates 16 crypto-random bytes, so
  a restarted process is a new identity. Its own pre-restart entries no longer
  match the self-delivery filter, so it replays them out of Redis and **heals
  its own last window of writes from the relay** — including anything it had
  published but not yet persisted. Readers see a first-ever sequence number
  from that identity, which is a baseline, so no counter fires.
- **Set it to a stable value.** The restarted process filters its own
  pre-restart entries as self-delivery and must recover them from
  [persistence](PERSISTENCE.md) instead. Sequence counters are in memory and
  restart at 0 under the same identity, which readers classify as a restart
  (`Restarts`), never as a `Gaps`.

Unless you have a reason to want stable node identity, leave `NodeID` unset.

### What's *not* in this adapter

- **Distributed lock / writer election** (Redlock pattern). Persistence
  write coordination is a different concern from doc-update fan-out and
  is the right place to add Redlock on top of the existing
  `VersionedPersistence` interface — not in the relay.
- **Redis cluster mode** — go-redis supports it but pub/sub semantics
  differ, and the Streams tier cannot be made safe on it at all; this
  adapter targets single-node Redis (or Sentinel). See
  [Redis Cluster is not supported](#redis-cluster-is-not-supported).

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
