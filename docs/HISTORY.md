# Project history

A short retrospective of how ygo got to where it is, and where things stand. For per-release detail see [CHANGELOG.md](../CHANGELOG.md).

## Where we are

The library is at v1.7.0. The CRDT core, both update formats, sync protocol, awareness, WebSocket and HTTP providers, snapshots, and undo manager are all in place and used in production. The post-1.0 work has focused on hardening rather than feature breadth — panic safety, out-of-order convergence, structured logging, observability hooks. See [CHANGELOG.md](../CHANGELOG.md) for the per-release detail.

## What was built

ygo was developed in eight phases, all complete:

- **`encoding/`** implements the lib0 variable-length binary format. Wire-compatible with Yjs JS and yrs.
- **`crdt/`** holds the YATA integration algorithm, content types, item store, and the `Doc` type that everything hangs off.
- **`crdt/` Y-types** — `YText`, `YArray`, `YMap`, `YXmlFragment`, `YXmlElement`, `YXmlText` — wrap the item store with type-specific operations and observer support.
- **Update encoding V1 and V2** — both wire formats supported in both directions, with V1↔V2 conversion.
- **`sync/`** implements the y-protocols sync handshake (`SyncStep1`, `SyncStep2`, incremental updates).
- **`awareness/`** holds the ephemeral state layer for cursors, presence, and per-peer metadata.
- **`crdt/snapshot.go`** captures and restores documents at point-in-time states.
- **`provider/websocket/` and `provider/http/`** are transport bindings — peer fan-out for WebSocket, request/response sync for HTTP. The core is transport-agnostic.

## The post-1.0 arc

Eleven minor and patch releases between v1.1.0 and v1.7.0, mostly focused on hardening:

- **Panic safety** (v1.1.1) — `Doc.Transact` no longer leaks the document lock when `fn` panics.
- **Cooperative cancellation** (v1.1.2) — `Doc.TransactContext` and `Transaction.Ctx()` let `fn` observe context cancellation.
- **Out-of-order delta convergence** (v1.2.0) — pending-structs queue parks items whose dependencies haven't arrived; mirrors `pendingStructs` in Yjs JS and `Store.pending` in yrs.
- **Error-returning variants** (v1.3.0, v1.6.0, v1.7.0) — `TransactE`, `TransactContextE`, `SetLocalStateContext`, `ApplyUpdateContext`, `UndoContext`, `RedoContext`, `WriteVarIntE`. Sibling-method pattern; all additive.
- **WebSocket hardening** (v1.4.0, v1.4.1) — slog, `MaxMessageBytes`, `PeerWriteQueueSize`, disconnect-on-overflow, goroutine-leak fixes via TOCTOU and double-call audits.
- **Security and observability** (v1.5.0) — `crypto/rand` for `ClientID`, `Doc.PendingStats`, semaphore-backed hard caps on connections.
- **Documentation polish** (v1.5.0, v1.6.0) — runnable godoc examples, stability statements, comparison docs.
- **Internal refactor** (v1.6.0, v1.6.1) — split `provider/websocket/server.go` into focused files; `applyV1Txn` refactored into helpers (-1.76% on `BenchmarkApplyUpdateV1`).
- **Optional context-aware persistence** (v1.7.0) — `PersistenceAdapterContext` extension lets adapters abort in-flight writes on `Server.Shutdown`.

## Upstream alignment

Several design decisions were grounded by comparing how Yjs JS and yrs handle the same problem before committing to an approach. Examples:

- **Pending-structs (#11)** — researched `StructStore.pendingStructs` in Yjs JS and `Store.pending` in yrs. Adopted the yrs-style decoded-objects queue with a watermark-based retry gate, adapted for Go's non-reentrant mutex.
- **Broadcast model (#19)** — Yjs JS queues unbounded with a 30 s ping timeout; yrs uses a bounded broadcast channel that drops the peer on overflow. Adopted the yrs pattern; the CRDT pending-structs machinery handles reconnect-and-resync.
- **`Item.integrate` shape (#22)** — both upstreams keep their YATA conflict-resolution function monolithic. Investigated whether to split in Go; benchstat showed every split regressed the hot path. Closed the issue with the architecture-note finding rather than forcing a refactor.
- **Error variants (#26)** — looked at Yjs JS's `doc.transact(f)` (generic return) and Rust's `Result` patterns. Adopted the sibling-method approach (`TransactE`, `WriteVarIntE`) so existing callers stay source-compatible.

Treating upstream as a reference point — not a transliteration target — is one of the project's design values.

## What's next

The issue tracker is empty as of v1.7.0. Future work happens as issues surface. Performance, correctness, and operational hooks are the most likely areas. v2.0 is not currently planned; if it happens, breaking changes will land bundled, with a migration guide, and a beta tag before stable.

## How to contribute

See [CONTRIBUTING.md](../CONTRIBUTING.md) for the contribution process and PR conventions.
