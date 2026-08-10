# Offline-First Client

`provider/client` is an embeddable, offline-first sync client for a single
`*crdt.Doc`: hydrate it from a local store, edit it at any time — connected,
disconnected, or never-yet-connected — and let a background dial loop
reconcile it with a y-websocket/Hocuspocus server whenever one is reachable.
It speaks the same wire protocol `provider/websocket` serves, so it dials
`ygo-server`, the `collab-editor` example server, or any y-websocket-
compatible backend without modification.

Where `provider/websocket` is the server that answers many peers,
`provider/client` is the peer: a Go process (or, via
[`mobile.SyncClient`](#mobile-ios--android), a native iOS/Android app) that
wants a Yjs document synced to that server without hand-rolling the dial,
handshake, reconnect, and persistence bookkeeping itself.

---

## Quickstart

```go
import (
    "context"
    "log"

    "github.com/reearth/ygo/crdt"
    client "github.com/reearth/ygo/provider/client"
)

doc := crdt.New()
text := doc.GetText("notes") // resolve shared roots before any Transact

c, err := client.New(client.Options{
    URL:       "wss://example.com/yjs/my-room",
    Doc:       doc,
    StorePath: "my-room.db", // SQLite-backed local durability; "" = memory-only
})
if err != nil {
    log.Fatal(err)
}

// Doc is usable RIGHT NOW — before Connect, and even if Connect never
// succeeds. An offline-first app binds its UI here, not after a sync.
doc.Transact(func(txn *crdt.Transaction) {
    text.Insert(txn, 0, "written before the network exists")
})

c.OnStatus(func(st client.Status) {
    log.Printf("status: %v (err: %v)", st.State, st.Err)
})

ctx, cancel := context.WithCancel(context.Background())
defer cancel()

// Connect hydrates from the store, then blocks for the client's whole sync
// lifetime — dialing, handshaking, and reconnecting with backoff on its own
// — so run it on its own goroutine.
go func() {
    if err := c.Connect(ctx); err != nil {
        log.Printf("Connect: %v", err)
    }
}()

<-c.Synced() // optional: block until the Doc has reconciled with the server at least once

// ... edit doc / text at any time from any goroutine ...

_ = c.Close() // store durability first, then a bounded network drain, then teardown
```

A `Client` is single-use: one `Connect` call per `Client` (a second call
returns `ErrAlreadyConnected`); construct a fresh `Client` for a fresh
session. See [`examples/offline-client`](../examples/offline-client) for a
complete, runnable, flag-driven version of the above that edits on a ticker
and logs every status transition.

---

## The offline story: the handshake *is* the offline flush

There is deliberately **no separate offline-op queue** anywhere in this
client. That is the central design point, and it follows straight from how
the y-protocol sync handshake already works:

- **SyncStep1** is a peer's state vector — "here is everything I have."
- **SyncStep2** is the other side's answer — "here is everything you're
  missing."

Any edit made while disconnected simply sits in the `Doc` (Yjs updates are
just more state) until the next connection's handshake runs. That handshake
doesn't know or care whether the edit happened five seconds or five days ago
— it just sees a state vector that is missing something and sends it. A
client that reconnects successfully never needs a queue to converge: the
handshake alone reconciles offline edits with the server the moment a
connection reappears.

So what is the **local store** (`Options.Store` / `Options.StorePath`) for,
if not that? It exists for the one case the handshake cannot help with at
all: **the process itself going away while still offline** — the app is
killed, the OS reaps it, the laptop sleeps mid-edit. A `*crdt.Doc` lives in
memory; nothing about the sync protocol writes it to disk. The store's whole
job is to survive that gap: every update applied to the Doc — the caller's
own edits **and** everything ever received from the server — is persisted
as it happens (`LocalStore.StoreUpdate`), and the next `New`/`Connect`
loads the same bytes back (`LocalStore.LoadDoc`) into a fresh in-memory Doc
**before that Doc is exposed to the caller or a dial is even attempted**.

That hydrate-before-dial ordering is what makes this "offline-first" rather
than merely "offline-tolerant": call `New`, start reading and editing the
`Doc` immediately, and get back everything persisted last time — even with
the server unreachable or down, because hydration never waits on the
network to begin with. A dial failure inside `Connect` does not make
`Connect` give up either: it reports the failure through `OnStatus` and
retries with backoff, indefinitely, because for an offline-first client "the
server is unreachable" is the ordinary case, not a terminal one.

Practical consequence: **storing remote updates matters as much as storing
local ones.** A client that syncs a large room, closes, and reopens offline
should not hydrate back only the edits it made itself, having silently
discarded everything it ever learned from the server. This package stores
both, and only skips storing the one update hydration itself applies
(re-persisting what just came out of the store would be pointless, forever).

No `Options.Store`/`StorePath` at all is a legal configuration — the Doc is
still fully usable in memory, it just starts empty on every process restart,
exactly like using a bare `*crdt.Doc` without this package.

---

## Reconnect, backoff, and keepalive

`Connect` never returns on a connection failure (except the one terminal
case below); it reports the failure via `OnStatus` and keeps retrying until
`ctx` is cancelled or `Close` is called.

- **Backoff** is Full Jitter: each retry delay is drawn uniformly from
  `[0, min(Options.MaxBackoff, 500ms * 2^attempt))`. The base (500ms) is
  fixed; `Options.MaxBackoff` (default **30s**) caps how wide the range
  grows. The attempt counter resets only when a connection's sync handshake
  actually **completes** (its first `SyncStep2` applied) — not merely on a
  successful dial or WebSocket upgrade. A server that accepts a TCP/TLS
  connection and then immediately drops it (a load balancer with no healthy
  backend, say) is not "connected" in the sense that should let a client
  hammer it every half-second; only a real handshake resets the schedule.
- **Keepalive** answers the half-open-connection problem: a peer that
  vanishes without a clean FIN/RST never produces a read error on its own,
  so `Connect` would otherwise sit forever on a dead socket. The client
  sends a WebSocket PING every `Options.PingInterval` (default **30s**,
  matching `provider/websocket`'s own default) and treats
  `2 * PingInterval` of total silence (no pong, no data frame at all) as a
  dead connection, surfacing as an ordinary read error that feeds the
  reconnect loop above. A single missed pong is not fatal — the next ping
  still has a full interval to succeed before the 2x deadline fires.
- **Auth is separate from both.** `Options.Token` rejection
  (`ErrAuthRejected`) is the one connection outcome that is **not** retried
  — see [Auth token caveat](#auth-token-is-not-a-confidentiality-gate)
  below.

`Options.ReadLimit` (default 64 MiB) caps the size of a single inbound
frame, mirroring `provider/websocket.Server.MaxMessageBytes` so a client
talking to ygo's own server needs no tuning to interoperate.

---

## Local store and compaction

```go
type LocalStore interface {
    LoadDoc(room string) ([]byte, error)
    StoreUpdate(room string, update []byte) error
}
```

Shaped after `provider/websocket.PersistenceAdapter` on purpose: any
adapter already written for the server side plugs in here with zero glue.
`Options.StorePath` is the common case — it opens (and later, `Close`s) a
CGo-free SQLite-backed store for you via `OpenSQLiteStore`; `Options.Store`
takes a pre-constructed `LocalStore` instead, for a caller that wants to
reuse it elsewhere (a `Store` passed this way is *never* closed by
`Client.Close` — the caller keeps ownership). The two are mutually
exclusive; `New` rejects setting both.

`CompactableStore` (an optional extension `*SQLiteStore` satisfies) lets a
long-lived local database collapse its append-only update log instead of
growing forever. The client asks for compaction once `Options.CompactEvery`
(default **500**) successful stored updates have accumulated for a room,
checked between processed messages on the sync loop's own goroutine —
which means compaction only ever runs while a connection is live and
cycling through that loop. A device that stays offline for its entire
lifetime accumulates an uncompacted log for as long as that lasts; this is
an accepted trade-off, not an oversight, since the alternative (a
free-standing ticker) would need its own goroutine and its own
Close-coordination for a case this package's design doesn't otherwise need
to solve. `OpenSQLiteStore` pre-sets `KeepVersions` to 500 (matching the
default compaction trigger) rather than the server-side adapter's own "keep
everything" default — a client has no use for the version history a server
adapter's `KeepVersions=0` preserves, since this package never exposes
`ListVersions`/`MaterializeAt` to a caller.

---

## `Stats()` and alerting

```go
stats := c.Stats()
// stats.Coalesced, stats.AwarenessSuperseded, stats.HardDrops, stats.Dropped
```

Mirrors `provider/websocket`'s `RelayStats` in shape and in alerting voice:

- **`Coalesced`** and **`AwarenessSuperseded`** are **routine** — merged
  backlog batches and superseded presence announcements under ordinary
  load. Never evidence of loss. Watch their *rate*, not their presence.
- **`HardDrops`** should always be zero in this package today (kept for
  shape parity with `RelayStats`; nothing here retries, so nothing here
  hard-drops).
- **`Dropped`** is the one to alert on — but its exact rule is
  **durability-based, not connection-based**, and getting this distinction
  backwards is the easiest way to over- or under-react to it:

  | What left the outbound lane without reaching the wire | Counted in `Dropped`? |
  |---|---|
  | A document update, **with a `Store` configured** | **No.** The store already has it (writes happen synchronously, before the update is ever queued); the next hydrate+handshake — this client's own reconnect, or an entirely new `Client` after a restart — delivers it from there. Not a loss, only a delay. |
  | A document update, **with no `Store` configured** | **Yes.** Nothing durable backs it once it leaves the lane. |
  | An awareness (presence) update — Store or no Store | **Always yes.** Awareness is not document state: the sync handshake never carries it, and there is no store equivalent for it. |
  | A local `Store.StoreUpdate` call that itself failed | **Always yes.** The edit survives in memory and in the Doc, but is no longer durable across a restart — this is real loss even though the network was never involved. |

  In a healthy deployment **with a `Store` configured**, `Dropped` should
  stay at zero. Alert on it going non-zero exactly as you would
  `RelayStats.Dropped` server-side.

`Stats()` is lock-free and cheap to poll from any goroutine, including from
inside an `OnStatus` callback.

---

## Auth token is not a confidentiality gate

`Options.Token` sends ygo's Hocuspocus in-band auth token (the client-side
counterpart of `provider/websocket.Server`'s `OnTokenAuth`, #104) on every
connection, before the sync handshake. If the server rejects it,
`Connect` returns `ErrAuthRejected` and does **not** retry — a bad
credential will be rejected again forever, so retrying it would just hammer
the server with a request that can never succeed.

What it is **not**: a way to withhold document content from an
unauthenticated caller. ygo's own server pushes `SyncStep1` + a full
`SyncStep2` + `Awareness` the moment a connection is accepted — **before it
has read anything the client sent, Token included**. Concretely, all of the
following can be true on the same connection:

- This client's `Doc` is already fully populated with the room's real
  content.
- `Synced()` has already closed (and `StateSynced` has already been
  reported via `OnStatus`).
- `Token` is rejected moments later, surfacing as
  `StateDisconnected{Err: ErrAuthRejected}` or as `Connect`'s return value.

So: **do not treat a closed `Synced()`, or `StateSynced`, as proof of a
successful auth exchange.** They mean "the Doc has the server's state," not
"this connection was authorized." If a deployment needs to withhold
document contents from an unauthenticated caller, that has to happen at the
HTTP boundary — `provider/websocket.Server`'s `AuthFunc` or `Authorize` —
which rejects the connection before the WebSocket upgrade ever completes,
rather than at the in-band `Token` exchange this client speaks.

---

## Close semantics

`Client.Close` follows the same discipline `provider/websocket.Server`'s
`Shutdown` established for the server side (#202): **durability first, then
a bounded best-effort network drain that counts what it could not deliver,
then teardown** — never a silent discard.

1. Store writes are already done by the time `Close` starts winding down:
   every store write happens synchronously inside the Doc/Awareness
   observer, on the caller's own `Transact`/`SetLocalState` goroutine —
   there is no asynchronous buffer to flush separately.
2. `Close` signals the sync loop to stop and joins it — no goroutine this
   `Client` owns is still reading frames or holding the socket open by the
   time this step returns.
3. The Doc and Awareness observers are unsubscribed, so nothing can queue
   further outbound work.
4. Whatever is still queued on the outbound lane at this point is drained
   and counted into `Stats().Dropped` per the table above — a document
   update backed by a `Store` is still not counted (the store already has
   it); everything else is.
5. If `Options.StorePath` was used (this `Client` opened its own store),
   that store is closed. A `Store` supplied directly via `Options.Store` is
   never closed here — the caller retains that handle.

`Close` is idempotent and safe to call concurrently with `Connect`.

---

## Mobile (iOS / Android)

[`mobile.SyncClient`](../mobile/) is the `gomobile`-bindable wrapper around
this package: it makes an on-device [`mobile.Doc`](../mobile/README.md) (the
full on-device editor `mobile/` already ships) **self-syncing** — dialing,
persisting, and reconnecting on its own, off the platform UI thread.

```kotlin
// Kotlin (Android)
val client: SyncClient
try {
    client = Mobile.newSyncClient(
        "wss://example.com/yjs/my-room",
        "/data/data/com.example.app/files/my-room.db", // dbPath: "" = memory-only
        "" // token: Hocuspocus in-band auth, "" = none
    )
} catch (e: Exception) {
    Log.e("ygo", "newSyncClient failed", e) // bad URL, or store open failure
    return
}

val doc = client.doc() // usable immediately, before connect()
client.setOnStatus(object : SyncStatusObserver {
    override fun onStatus(state: Long, errMsg: String) {
        // state is one of Mobile.SyncStateConnecting / Connected / Synced / Disconnected
    }
})
client.connect() // returns immediately; runs in the background

// ... later, e.g. ViewModel.onCleared() ...
client.close() // stops syncing; does NOT close doc — content stays readable
```

```swift
// Swift (iOS)
var error: NSError?
let client = MobileNewSyncClient(
    "wss://example.com/yjs/my-room",
    dbPath, // "" = memory-only
    "", // token: "" = none
    &error
)
guard let client = client, error == nil else {
    print("newSyncClient failed: \(error!)") // bad URL, or store open failure
    return
}

let doc = client.doc() // usable immediately, before connect()
client.setOnStatus(statusObserver) // your SyncStatusObserver implementation
client.connect() // returns immediately; runs in the background

// ... later, e.g. deinit ...
client.close() // stops syncing; does NOT close doc — content stays readable
```

`SyncStateConnecting`/`Connected`/`Synced`/`Disconnected` are the gomobile-
safe `int64` mirror of `provider/client.State` — see
[`mobile/syncclient.go`](../mobile/syncclient.go)'s doc for the exact
mapping and why it is pinned independently of that package's own enum
order. `SyncedOnce()` gives platform code a poll-friendly boolean instead of
a Go channel it cannot receive on across the binding boundary. Every other
behaviour above — the offline story, `Options.Token`'s auth caveat, backoff
and keepalive — applies unchanged; `SyncClient` is a thin translation layer,
not a second implementation.

See [`mobile/README.md`](../mobile/README.md) for the full gomobile build
matrix, threading rules, and lifecycle discipline that also govern
`SyncClient`.
