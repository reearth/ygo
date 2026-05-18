# ygo

[![CI](https://github.com/reearth/ygo/actions/workflows/ci.yml/badge.svg)](https://github.com/reearth/ygo/actions/workflows/ci.yml)
[![Go Reference](https://pkg.go.dev/badge/github.com/reearth/ygo.svg)](https://pkg.go.dev/github.com/reearth/ygo)
[![Go Report Card](https://goreportcard.com/badge/github.com/reearth/ygo)](https://goreportcard.com/report/github.com/reearth/ygo)
[![License](https://img.shields.io/badge/license-MIT-blue.svg)](LICENSE)

**ygo** is a pure-Go implementation of the [Yjs](https://github.com/yjs/yjs) CRDT (Conflict-free Replicated Data Type) library, enabling real-time collaborative applications in Go backends without CGO or embedded runtimes.

It is **binary-compatible** with the JavaScript Yjs reference implementation — updates produced by ygo can be applied by Yjs clients, and vice versa.

## API stability

ygo follows [semantic versioning](https://semver.org/). The v1.x public API is stable: new functionality lands as minor releases; bug fixes as patch releases; breaking changes are deferred to v2.

## Capabilities

ygo is a pure-Go CRDT library that interoperates with Yjs (JavaScript) and yrs (Rust):

- All Y-types: `YText`, `YArray`, `YMap`, `YXmlFragment`, `YXmlElement`, `YXmlText`
- Both update wire formats (V1 and V2, with V1↔V2 conversion)
- The y-protocols sync handshake and awareness layer
- WebSocket and HTTP transport bindings (the core is transport-agnostic)
- Snapshots, garbage collection, undo manager, persistence adapters

The current release is **v1.7.0**. See [CHANGELOG.md](CHANGELOG.md) for the per-release detail and [docs/HISTORY.md](docs/HISTORY.md) for the longer arc.

## Features

- **Pure Go** — no CGO, no V8, no embedded JavaScript engine.
- **Binary-compatible** with Yjs JS and yrs. Updates round-trip across all three implementations.
- **Full Y-type coverage** — `YText`, `YArray`, `YMap`, `YXmlFragment`, `YXmlElement`, `YXmlText`.
- **Both update formats** — V1 and V2, with V1↔V2 conversion.
- **Sync protocol** — implements [y-protocols](https://github.com/yjs/y-protocols) `SyncStep1`, `SyncStep2`, and incremental updates.
- **Awareness** — presence, cursor sharing, ephemeral state.
- **Snapshots** — point-in-time document history and restore.
- **Transport-agnostic** — core logic has no transport dependency; WebSocket and HTTP handlers are addons.

Post-v1.0 hardening:

- **Panic-safe transactions** (v1.1.1). If `fn` inside `Transact` panics, the document lock is still released, observers fire with the partial state that was committed before the panic, and the original panic propagates to the caller. No rollback — that's by design, matching Yjs JS and yrs. For atomic batching, recover above `Transact` and reconcile via sync.
- **Cooperative cancellation** (v1.1.2). `Doc.TransactContext` accepts a `context.Context` and exposes it inside `fn` via `txn.Ctx()`. `fn` can poll `txn.Ctx().Err()` to bail out early when the request context is cancelled.
- **Error-returning variants** (v1.3.0, v1.6.0, v1.7.0). Sibling methods that surface errors instead of panicking or silently succeeding: `TransactE`, `TransactContextE`, `Awareness.SetLocalStateContext`, `Awareness.ApplyUpdateContext`, `UndoManager.UndoContext`, `UndoManager.RedoContext`, `Encoder.WriteVarIntE`. All additive; original methods unchanged.
- **Out-of-order delta convergence** (v1.2.0). When an update references an item whose dependency hasn't arrived yet, the item is parked in a per-doc pending queue and integrated automatically when the missing predecessor arrives. Mirrors `pendingStructs` in Yjs JS and `Store.pending` in yrs.
- **WebSocket hardening** (v1.4.0). Structured logging via `*slog.Logger`, per-message size cap (`MaxMessageBytes`), bounded per-peer broadcast queue with disconnect-on-overflow (`PeerWriteQueueSize`), and the bounded broadcast pattern itself — replacing the previous goroutine-per-broadcast fan-out.
- **Operational observability** (v1.5.0). `Doc.PendingStats()` returns counts of items parked waiting for dependencies — useful when monitoring convergence in production.
- **Semaphore-backed hard caps** (v1.5.0). `MaxConnections` and `MaxPeersPerRoom` are now hard guarantees, not optimistic atomic counters with race windows.
- **`crypto/rand` ClientID** (v1.5.0). Predictable IDs in multi-tenant deployments are a footgun; the default `ClientID` is now cryptographically random. `crdt.NewClientID()` is exposed for callers who want to generate IDs externally.
- **Context-aware persistence** (v1.7.0). Adapters can opt into `PersistenceAdapterContext` to receive a context cancelled when `Server.Shutdown` begins, letting them abort in-flight DB calls instead of blocking shutdown.

See [CHANGELOG.md](CHANGELOG.md) for the full per-release picture.

## Requirements

- Go 1.23 or later

## Installation

```bash
go get github.com/reearth/ygo
```

## Quick Start

```go
package main

import (
    "fmt"
    "github.com/reearth/ygo/crdt"
)

func main() {
    // Create two peers
    alice := crdt.New()
    bob := crdt.New()

    // Obtain the shared type before entering a transaction —
    // GetText and Transact both acquire the document mutex.
    text := alice.GetText("content")

    // Alice makes edits
    alice.Transact(func(txn *crdt.Transaction) {
        text.Insert(txn, 0, "Hello, world!", nil)
    })

    // Encode Alice's state and send to Bob
    update := alice.EncodeStateAsUpdate()

    // Bob applies the update — both docs now converge
    if err := crdt.ApplyUpdateV1(bob, update, nil); err != nil {
        panic(err)
    }

    fmt.Println(bob.GetText("content").ToString()) // "Hello, world!"
}
```

## Examples

The [`examples/`](examples/) directory contains four runnable programs with detailed inline comments:

| Example | What it shows |
|---------|---------------|
| [`examples/peer-sync`](examples/peer-sync/) | In-process two-peer sync via the y-protocols handshake — no network needed |
| [`examples/http-sync`](examples/http-sync/) | Pull/push sync over HTTP with incremental state-vector diffs |
| [`examples/collab-editor`](examples/collab-editor/) | Real-time multi-tab collaborative editor with a browser client |
| [`examples/snapshot-history`](examples/snapshot-history/) | Document versioning — capture, store, and restore past states |

Run any example from the repository root:

```bash
go run ./examples/peer-sync
go run ./examples/http-sync
go run ./examples/snapshot-history
go run ./examples/collab-editor/server   # then open http://localhost:8080
```

**New users**: start with `peer-sync` for the smallest end-to-end demonstration of two docs converging in-process. Jump to `collab-editor` when you want to wire the WebSocket server to a real browser client.

## WebSocket Server

```go
package main

import (
    "net/http"
    "github.com/reearth/ygo/provider/websocket"
)

func main() {
    server := websocket.NewServer()
    http.Handle("/yjs/{room}", server)
    http.ListenAndServe(":8080", nil)
}
```

## Server-side document injection

Backend services — AI agents, HTTP handlers, content pipelines — can push
changes into a live room without simulating a WebSocket peer. Three APIs
are available on `*websocket.Server`.

### `BroadcastUpdate(ctx, room, update)`

Fans a pre-encoded V1 update out to all peers currently connected to a
room. Does **not** apply the update to the server's doc — callers who
want the server's state to reflect the broadcast must call
`crdt.ApplyUpdateV1` first (or use `Apply` below).

```go
doc := server.GetDoc("my-room")
if err := crdt.ApplyUpdateV1(doc, update, nil); err != nil {
    return err
}
if err := server.BroadcastUpdate(ctx, "my-room", update); err != nil {
    return err
}
```

**Skipping `ApplyUpdateV1` creates divergence.** Live peers see the
update, but peers joining afterwards receive the server's stale state
via sync step 2.

### `Apply(ctx, room, fn)`

Applies a callback to the doc and broadcasts the resulting delta atomically.
Auto-creates the room if needed. Persistence runs via the existing
`OnUpdate` hook — callers do not need to persist separately.

```go
err := server.Apply(ctx, "my-room",
    func(doc *crdt.Doc, transact func(func(*crdt.Transaction))) {
        frag := doc.GetXmlFragment("content") // OUTSIDE transact — see note
        transact(func(txn *crdt.Transaction) {
            elem := crdt.NewYXmlElement("p")
            frag.InsertElement(txn, 0, elem)
        })
    },
)
```

**Important:** calls to `doc.GetXmlFragment`, `doc.GetText`, `doc.GetMap`,
and the other root-type accessors must happen **outside** the `transact`
callback. These methods acquire the doc's write lock, which `transact`
already holds — calling them inside deadlocks.

`fn` should be fast. It runs inside the doc's write lock and blocks all
peer reads and writes to the room for the duration.

**On `ErrUpdateTooLarge`, the mutation sticks.** The size check runs
after `fn`'s transaction commits and after persistence has enqueued the
update, so the server's doc reflects `fn`'s changes and the update IS
persisted — but peers do NOT see it. Size-bound `fn`'s effects
explicitly or reconcile peers via a sync step 1/2 exchange.

### `CloseRoom(name, force)`

Explicit teardown for rooms created by `Apply` that never accumulated
peer connections. Without `CloseRoom`, such rooms linger until process
exit.

```go
if err := server.CloseRoom("my-room", false); err != nil { /* ... */ }
// force=true closes connected peers first.
```

### Access control: `Server.OnInject`

An optional hook gates all server-side writes:

```go
server.OnInject = func(ctx context.Context, info websocket.InjectInfo) error {
    tenant, _ := ctx.Value(tenantKey{}).(string)
    if !allowed(tenant, info.Room) {
        return fmt.Errorf("tenant %q may not write to %q", tenant, info.Room)
    }
    if info.Op == websocket.OpBroadcastUpdate && info.UpdateSize > 1<<20 {
        return errors.New("update too large for this tenant")
    }
    return nil
}
```

`info.Op` is `OpBroadcastUpdate` or `OpApply`. `info.UpdateSize` is the
length of the update bytes for `BroadcastUpdate`; zero for `Apply` (the
delta has not yet been produced — size capping for `Apply` is handled
by `MaxUpdateBytes`, post-hoc).

Refusals are returned wrapped with `ErrInjectRefused`, so callers can
match either the sentinel or the hook's own error via `errors.Is`.

### Resource caps

- `Server.MaxUpdateBytes` — per-update size cap, default 64 MiB (matches
  the peer frame limit).
- `Server.MaxRooms` — total-room cap applied uniformly to peer upgrades
  (HTTP 503) and `Apply` (`ErrTooManyRooms`). Default unlimited.

### Trust model

`Server.Apply` and `Server.BroadcastUpdate` grant total write authority
on the document. Treat the `*Server` handle with the same care as a
database connection — do not expose it directly to untrusted code.
`OnInject` is defense-in-depth, not a substitute for caller-side
authorization. A caller who can reach either API can craft updates that
spoof any client ID, which is equivalent to the authority already
granted by `GetDoc` + `ApplyUpdateV1`.

## Persistence

The WebSocket server takes an optional `PersistenceAdapter` so room state survives restarts:

```go
type PersistenceAdapter interface {
    LoadDoc(room string) ([]byte, error)
    StoreUpdate(room string, update []byte) error
}
```

`LoadDoc` is called once when the first peer connects to a room; the result seeds the in-memory doc. `StoreUpdate` is called on every committed transaction. Writes run on a per-room worker goroutine — slow storage doesn't block peers. Wire an adapter in via `NewServerWithPersistence(adapter)`.

For backend examples (Postgres, Redis, file-system) and the v1.7.0 context-aware extension that lets adapters abort in-flight writes during `Server.Shutdown`, see [docs/PERSISTENCE.md](docs/PERSISTENCE.md).

## Running in production

The library ships several operational hooks. See package godoc for the full reference; here's the short version of what to wire up.

### Logging

`Server.Logger *slog.Logger` defaults to `slog.Default()`. Surfaces slow-peer write failures, sync-dispatch errors, and awareness apply errors at `Warn` level with `room` and `peer` context.

```go
server := websocket.NewServer()
server.Logger = slog.New(slog.NewJSONHandler(os.Stdout, nil))
```

### Observability

`Doc.PendingStats()` returns a snapshot of the per-doc pending queue: how many items are parked, how many delete-set ranges are queued, which clients we're blocked on. Cheap (one read-lock). Useful when monitoring out-of-order delivery in production.

```go
stats := doc.PendingStats()
metrics.PendingItems.Set(float64(stats.Items))
metrics.PendingDeleteRanges.Set(float64(stats.DeleteRanges))
```

### Resource limits

All hard-capped (semaphore-backed for connection counts):

- `Server.MaxConnections` — server-wide cap on simultaneous WebSocket peers.
- `Server.MaxPeersPerRoom` — per-room cap.
- `Server.MaxRooms` — total-room cap (applies to peer upgrades and `Server.Apply`).
- `Server.MaxUpdateBytes` — per-update size cap (default 64 MiB).
- `Server.MaxMessageBytes` — per-message size on the WebSocket read path (default 64 MiB).
- `Server.PeerWriteQueueSize` — per-peer broadcast queue depth (default 256). When the queue fills, the peer is disconnected.
- `Server.MaxPendingItems` — per-document cap on items parked in the out-of-order pending queue (default 100,000). When the cap is reached, updates that would park additional items return `ErrInvalidUpdate`. Defends against a crafted update full of far-future-clock items that would otherwise grow the queue unboundedly. Same cap is available at the doc level via `crdt.WithMaxPendingItems(n)`.
- `Server.HandshakeTimeout` — first-read deadline applied after WebSocket upgrade (default 30s). Closes connections that complete the handshake but never send a message (slow-loris defense). Cleared after the first successful read.

Each defaults to a sensible value or unlimited where noted.

### Auth

`Server.AuthFunc func(*http.Request) bool` runs before the WebSocket upgrade. Return false to reject:

```go
server.AuthFunc = func(r *http.Request) bool {
    return validateBearer(r.Header.Get("Authorization"))
}
```

### Graceful shutdown

`Server.Shutdown(ctx)` drains pending writes and closes peers. Adapters that implement `PersistenceAdapterContext` (v1.7.0) receive a context derived from the shutdown signal so they can abort in-flight DB calls instead of waiting for the driver's timeout.

## Performance

### Running the benchmarks

```bash
# Run all benchmarks with memory allocation stats
go test ./... -run='^$' -bench='^Benchmark' -benchmem

# Run a specific package only
go test ./crdt/ -run='^$' -bench='^Benchmark' -benchmem

# Run with more iterations for tighter confidence intervals
go test ./... -run='^$' -bench='^Benchmark' -benchmem -benchtime=5s -count=3
```

To compare two branches (e.g. before and after an optimization), install [`benchstat`](https://pkg.go.dev/golang.org/x/perf/cmd/benchstat):

```bash
go install golang.org/x/perf/cmd/benchstat@latest

# Capture baseline
git checkout main
go test ./... -run='^$' -bench='^Benchmark' -benchmem -count=5 | tee old.txt

# Capture candidate
git checkout my-branch
go test ./... -run='^$' -bench='^Benchmark' -benchmem -count=5 | tee new.txt

# Compare
benchstat old.txt new.txt
```

The CI benchmark workflow (`.github/workflows/benchmark.yml`) runs this comparison automatically on every pull request.

### Reference numbers

Measured on Apple M4 Max (arm64, Go 1.23). Your numbers will vary by hardware.

**Encoding (`encoding/`)** — the codec runs on every item; these are sub-10 ns, zero-alloc:

| Benchmark | ns/op | Allocs |
|-----------|-------|--------|
| ReadVarUint (1 byte) | 1.0 | 0 |
| WriteVarUint (1 byte) | 1.7 | 0 |
| WriteVarString (1000 chars) | 15 | 0 |
| ReadVarString (1000 chars) | 89 | 1 (string copy) |
| Encoder reuse (`Reset`) vs new | 7.7 vs 12.4 | 0 vs 1 |

**CRDT core (`crdt/`)** — realistic document operations:

| Benchmark | ns/op | Notes |
|-----------|-------|-------|
| `YText_InsertBulk` (1000 chars) | 2 006 | Single transaction — fast path |
| `YText_Insert` (1000 × 1 char) | 344 048 | ~344 ns per keystroke |
| `YText_Delete` (1000 × 1 char) | 891 456 | ~891 ns per delete |
| `EncodeStateAsUpdateV1` (1000 items) | 21 360 | ~21 µs to serialise a document |
| `ApplyUpdateV1` (1000 items) | 109 806 | ~110 µs to integrate a full state |
| `EncodeStateAsUpdateV2` | 33 029 | V2 is ~1.5× larger to encode… |
| `ApplyUpdateV2` | 679 207 | …and ~6× slower to decode |
| `TwoPeerConvergence` | 16 284 | Encode + apply incremental sync |
| `YMap_Set` (100 keys) | 19 557 | |
| `YArray_Push` (100 elements) | 59 209 | |

**Sync protocol (`sync/`)** — message framing overhead is negligible:

| Benchmark | ns/op |
|-----------|-------|
| `EncodeSyncStep1` | 179 |
| `ApplySyncMessage_Step1` | 631 |
| `ApplySyncMessage_Update` (1000-item doc) | 1 404 |
| `FullHandshake` | 1 303 |

**Awareness (`awareness/`)** — per-peer ephemeral state:

| Benchmark | ns/op |
|-----------|-------|
| `SetLocalState` | 65 |
| `EncodeUpdate` (1 client) | 226 |
| `EncodeUpdate` (50 clients) | 12 901 |
| `ApplyUpdate` (50 clients) | 19 801 |

## Architecture

See [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md) for a detailed explanation of the CRDT algorithm, data model, and package design.

## Compatibility

ygo targets compatibility with:

- [Yjs](https://github.com/yjs/yjs) v13.x (JavaScript reference implementation)
- [y-protocols](https://github.com/yjs/y-protocols) sync and awareness protocol
- [lib0](https://github.com/dmonad/lib0) binary encoding format

Compatibility is verified by golden-file tests that compare binary output byte-for-byte with Yjs-generated fixtures.

For a Go-vs-Rust port comparison, see [docs/comparison/ygo-vs-yrs.md](docs/comparison/ygo-vs-yrs.md).

## Gotchas

### No read methods or observer registration inside `Transact`

`Transact` acquires the document **write lock** for the duration of its callback.
Calling any of the read methods (`Get`, `ToSlice`, `Keys`, `Entries`, `ToString`,
`ToDelta`) or registering/unregistering observers (`Observe`, `ObserveDeep`) from
**inside** a `Transact` callback will **deadlock** because those operations try to
acquire the same lock.

```go
// ✗ WRONG — deadlocks
doc.Transact(func(txn *crdt.Transaction) {
    arr.Get(0)         // tries to RLock — deadlock
    arr.Observe(fn)    // tries to Lock  — deadlock
})

// ✓ CORRECT — acquire references and register observers before Transact
arr.Observe(func(e crdt.YArrayEvent) { /* ... */ })
doc.Transact(func(txn *crdt.Transaction) {
    arr.Push(txn, []any{"value"})
})
fmt.Println(arr.ToSlice()) // read after Transact returns
```

This constraint applies to `YArray`, `YText`, `YMap`, `YXmlFragment`, and
`YXmlElement`. UndoManager callbacks (`OnStackItemAdded`) also run outside
the lock and are safe to use normally.

### `Doc.ClientID` is read-only after creation

Use `crdt.WithClientID(id)` at construction time. Changing the ID after the
document has started accepting operations will corrupt the item store.

## What's changed since v1.0

Eleven minor and patch releases between v1.1.0 and v1.7.0, focused on hardening: panic safety, out-of-order convergence, WebSocket production hooks, observability, error-returning variants, and an optional context-aware persistence extension. See [CHANGELOG.md](CHANGELOG.md) for the per-release detail and [docs/HISTORY.md](docs/HISTORY.md) for the design narrative.

## Contributing

Contributions are welcome! Please read [CONTRIBUTING.md](CONTRIBUTING.md) before submitting a pull request.

For significant changes, open an issue first to discuss what you'd like to change.

## Security

ygo's security model is **defense-in-depth**, not authentication:

- **`ClientID` is collision-avoidance, not authentication.** The protocol does not validate that incoming updates match a peer's declared `ClientID`. Use `Server.AuthFunc` and/or transport-level auth.
- **Transport security is the embedder's responsibility.** ygo does not enforce TLS, signed updates, or peer authentication. Wrap the WebSocket server behind your usual reverse proxy.
- **`Server.Apply` and `Server.BroadcastUpdate` grant total write authority.** Treat the `*Server` handle like a database connection. `OnInject` is defense-in-depth, not a substitute for caller-side authorization.
- **`ClientID` generation uses `crypto/rand`** (v1.5.0). 32-bit space matches Yjs JS for wire compatibility; collision probability at multi-tenant scale is documented in [SECURITY.md](SECURITY.md).

Please report vulnerabilities by following the process in [SECURITY.md](SECURITY.md). Do not open public issues for security problems.

## License

MIT License — see [LICENSE](LICENSE).

This project is not affiliated with the Yjs authors. Yjs is developed by [Kevin Jahns](https://github.com/dmonad) and contributors.
