# Architecture

ygo is a pure-Go implementation of the [Yjs](https://github.com/yjs/yjs) CRDT algorithm. It is **binary-compatible** with the JavaScript reference implementation: updates produced by ygo can be consumed by Yjs clients, and vice versa.

## Package dependency graph

```
provider/webhook                                        mobile/
       │                                                    │
       ▼                                                    ▼
provider/websocket ────────────────────────────────── provider/client        provider/http
       │                                                                            │
       ├──────────────────┬───────────────┤                                         │
       ▼                  ▼               ▼                                         │
     sync/            cluster/       persistence/                                   │
       │                  │               │                                         │
       │                  ▼               │                                         │
       │             awareness/           │                                         │
  ┌────┼──────────────────┘               │                                         │
  │    │                                  │                                         │
  │    └─────────────────────────┬────────┘                                         │
  │                              ▼                                                  │
  │                            crdt/ ◄──────────────────────────────────────────────┘
  │                              │
  │                              ▼
  └─────────────────────────►encoding/
```

**Rule:** no upward imports. `encoding/` has zero runtime dependencies. `crdt/` depends only on `encoding/`. `sync/` depends on `crdt/` and `encoding/`; `persistence/` depends only on `crdt/`; `cluster/` depends on `awareness/` and `crdt/`. `awareness/` is the one exception to the otherwise-neat layering: it depends only on `encoding/`, not `crdt/`, despite sitting next to packages that do. `provider/websocket` and `provider/client` both import `sync/`, `awareness/`, `cluster/`, and `persistence/` directly; `provider/http` skips that whole tier and depends on `crdt/` directly. `provider/webhook` and `mobile/` sit one layer up, wrapping `provider/websocket` and `provider/client` respectively — but each also imports lower tiers directly for its own use: `provider/webhook` imports `crdt/`, and `mobile/` imports both `awareness/` and `crdt/`.

> **Note**: Since v1.0, the library has added several mechanisms not detailed here — the pending-structs queue for out-of-order delivery, structured logging via `slog`, per-peer broadcast queues, and context-aware methods. See [CHANGELOG.md](../CHANGELOG.md) for the per-release picture.

---

## `encoding/` — lib0 binary codec

Implements the [lib0](https://github.com/dmonad/lib0) variable-length binary encoding used by Yjs on the wire.

| Primitive | Description |
|-----------|-------------|
| `VarUint` | 7-bit chunks, LSB-first, continuation bit in MSB. 1–8 bytes. |
| `VarInt` | ZigZag-encoded signed integer stored as VarUint. |
| `VarString` | `VarUint(byteLen)` + raw UTF-8 bytes. |
| `VarBytes` | `VarUint(len)` + raw bytes. |
| `Float32/64` | 4/8-byte little-endian IEEE 754. |
| `Any` | Tagged union covering nil, bool, int, float, string, []byte, []any, map[string]any. |

---

## `crdt/` — core CRDT engine

### ID and StateVector

```
ID = { Client ClientID, Clock uint64 }
StateVector = map[ClientID]uint64   // highest integrated clock per client
```

Only insertions increment the clock. Deletions do not.

### Item

The fundamental unit of the CRDT. Each insertion creates one `Item`.

| Field | Purpose |
|-------|---------|
| `ID` | Unique logical timestamp |
| `Origin` | ID of left neighbour **at insertion time** |
| `OriginRight` | ID of right neighbour at insertion time |
| `Left / Right` | Current neighbours in the doubly-linked list |
| `Parent` | Owning shared type |
| `ParentSub` | Map key (for YMap entries) |
| `Content` | The actual data (see content types below) |
| `Deleted` | Tombstone flag — item stays in list when deleted |

### YATA integration algorithm

When integrating a new item:

1. Resolve `Right` from `OriginRight` via `getItemCleanStart` (splitting the target item if it contains the right-origin clock mid-content). The conflict-scan loop in step 3 uses `Right` as its upper bound — without this resolution the scan has no termination and can place items past their declared right boundary (fixed in v1.8.1, see #65/#68).
2. Locate the position immediately after `Origin` in the current list.
3. Scan right past any concurrent items that have the same `Origin` and a **lower** `ClientID` (they win the tie-break). The scan terminates at `Right` (resolved in step 1).
4. Insert the new item at the resolved position.

This guarantees identical final state on all replicas regardless of message arrival order, because the tie-break on `ClientID` is deterministic and total.

### Content types

| Type | Holds |
|------|-------|
| `ContentString` | UTF-8 text |
| `ContentBinary` | Raw bytes |
| `ContentAny` | Any JSON-compatible value |
| `ContentEmbed` | Embedded object (e.g. image metadata) |
| `ContentFormat` | Formatting attribute key/value (YText) |
| `ContentDeleted` | Tombstone placeholder (length only) |
| `ContentType` | Reference to a nested shared type |
| `ContentDoc` | Reference to a subdocument |

### StructStore

`map[ClientID][]*Item` — items are appended in clock order per client (append-only). Lookups by ID use binary search.

### Pending-structs queue

When an update references an item whose dependency hasn't arrived yet — a same-client clock gap or a cross-update Origin reference — the item is parked in a per-doc pending queue rather than silently orphaned. The queue drains automatically on each subsequent `ApplyUpdate` when the missing predecessors arrive. This mirrors Yjs JS's `pendingStructs` and yrs's `Store.pending`. See the v1.2.0 CHANGELOG entry for the design.

The same machinery handles delete-set entries that target not-yet-integrated items (`pendingDs`). State-vector computation is unaffected — pending items don't appear in `StateVector()` until they're integrated, so peers correctly retry the missing dependencies.

The queue is bounded (default 100,000 parked items; configurable via `crdt.WithMaxPendingItems` or `Server.MaxPendingItems`). Updates that would push past the cap return `ErrInvalidUpdate` — see the v1.8.0 CHANGELOG and #46 for the security rationale.

### DeleteSet

Tracks deleted ranges as `map[ClientID][]DeleteRange{Clock, Len}`. Items are tombstoned (marked `Deleted = true`) rather than removed, keeping linked-list positions stable.

### Transaction

Batches multiple operations. Observers fire **once per transaction**, not per operation.

Lifecycle:
1. Collect all inserts and deletes.
2. Squash consecutive same-client items (run-length optimisation).
3. Fire `beforeObserverCalls`.
4. Fire `observe()` on each changed type.
5. Fire `observeDeep()` recursively.
6. Fire `afterTransaction`.
7. Emit the binary update event for the transport layer.

### Doc

The root object. Holds the `StructStore`, named root types (`Share` map), `Subdocs` map, a `GC` flag, and observer subscriptions.

---

## `crdt/types/` — shared types

All types embed `abstractType` which holds the linked-list head/tail and observer lists.

| Type | Conflict resolution |
|------|---------------------|
| `YArray` | Ordered by insertion position |
| `YMap` | Last-write-wins by ID (higher clock wins) |
| `YText` | YArray with run-length squashing + `ContentFormat` items |
| `YXmlFragment` | Ordered child nodes |
| `YXmlElement` | YXmlFragment + element name + attributes (YMap) |
| `YXmlText` | YText inside an XML tree |

---

## Update encoding (V1 and V2)

**V1:** each struct is serialised with full client/clock metadata. Simple but verbose.

**V2:** differential clock encoding + run-length encoding of same-client runs. Typically 30–40% smaller than V1.

Both formats append a `DeleteSet` section. Conversion between V1 and V2 is lossless. The public API provides `EncodeStateAsUpdateV1/V2`, `ApplyUpdateV1/V2`, `UpdateV1ToV2`, `UpdateV2ToV1`, and `MergeUpdates`.

---

## `sync/` — y-protocols sync messages

Three message types (matching the [y-protocols spec](https://github.com/yjs/y-protocols/blob/master/PROTOCOL.md)):

| Type | Value | Purpose |
|------|-------|---------|
| `SyncStep1` | 0 | Send local `StateVector` to peer |
| `SyncStep2` | 1 | Respond with missing update (diff against received SV) |
| `Update` | 2 | Incremental update after initial sync |

`ReadSyncMessage(msg []byte) (msgType int, payload []byte, err error)` parses any incoming message into its type and raw payload, making it easy to dispatch in custom transport handlers.

The protocol is **transport-agnostic**: messages are plain `[]byte` and work over WebSocket, HTTP, WebRTC, or in-process pipes.

---

## `awareness/` — ephemeral state

Separate from document updates. Stores `map[ClientID]AwarenessState{Clock uint64, State any}`.

- Last-write-wins per client by `Clock`.
- States expire after 30 s of inactivity. Call `StartAutoExpiry(timeout)` to run expiry automatically in a background goroutine; it returns a stop function.
- Encoded as `VarUint(numClients)` + per-client `(clientID, clock, jsonState)`.

---

## `persistence/` — versioned storage layer

Depends only on `crdt/`. Layers an **append-only, versioned** store on top of the provider's `LoadDoc`/`StoreUpdate` primitive: every incremental update becomes a numbered `Version`, `MaterializeAt(v)` rebuilds the document at any past version via `crdt.MergeUpdatesV1`, and named snapshots plus crash-safe pruning/compaction round out the log.

Ships two reference implementations — `NewMemoryPersistence()` (in-process maps) and `NewFilePersistence(dir)` (atomic temp+rename writes to one directory per store) — plus `persistence/sqlite`, a pure-Go (CGo-free) SQLite backend. `LegacyAdapter` bridges a `VersionedPersistence` back to the provider's `PersistenceAdapter` shape without either package importing the other, avoiding a cycle. See [PERSISTENCE.md](PERSISTENCE.md) for the full interface and a conformance suite external adapters can run against.

## `cluster/` — cross-node relay

Depends on `awareness/` and `crdt/`. Defines the `Relay`/`Sink` abstraction that fans document updates **and** awareness out across multiple `provider/websocket` (or `provider/client`) processes sharing rooms, superseding the older persistence-adapter-as-pub/sub pattern. `MemRelay` is the in-process reference implementation, used by tests and single-process multi-server simulations; production deployments plug in `cluster/redis` or an equivalent backend. See [CLUSTERING.md](CLUSTERING.md).

---

## `provider/` — transport handlers

### `provider/websocket/`

`net/http`-compatible handler. One `Doc` per named room. On connect: exchanges `SyncStep1/2` and awareness state. On message: applies update and broadcasts to all other peers in the room.

**Persistence** is pluggable via the `PersistenceAdapter` interface:
```go
type PersistenceAdapter interface {
    LoadDoc(room string) ([]byte, error)
    StoreUpdate(room string, update []byte) error
}
```
Pass an implementation to `NewServerWithPersistence(p)`. The built-in `MemoryPersistence` (returned by `NewMemoryPersistence()`) appends updates in memory and periodically folds a room's backlog (`CompactEvery`, default 500) rather than re-merging on every write; suitable for single-process deployments.

### `provider/http/`

| Method | Path | Semantics |
|--------|------|-----------|
| `GET` | `/doc/{room}?sv=<base64>` | Return binary update diff |
| `POST` | `/doc/{room}` | Apply binary update from request body |

### `provider/webhook/`

Wraps a `provider/websocket` server with outbound HTTP callbacks — HMAC-SHA256-signed, debounced/coalesced, retried with backoff on transient failure — fired on room lifecycle and document-update events, so an external service can react to changes without holding a live connection.

### `provider/client/`

The embeddable, offline-first counterpart to `provider/websocket`: a Go peer (not a server) that hydrates a `*crdt.Doc` from local storage, lets the caller edit it immediately regardless of connectivity, and runs a background dial loop that reconciles with a `provider/websocket`-served (or Hocuspocus-compatible) endpoint whenever one is reachable. Speaks the same wire protocol `provider/websocket` serves. See [CLIENT.md](CLIENT.md).

---

## `mobile/` — Go Mobile bindings

A `gomobile bind`-safe façade over `crdt/`, `awareness/`, and `provider/client`, for embedding ygo natively in iOS/Android apps with no JavaScript runtime and no CGo. Because `gomobile bind` only supports a restricted set of cross-language types, every exported method uses only `string`, `int64`, `bool`, `[]byte`, `error`, and the bound pointer types `*Doc`/`*Awareness`/`*SyncClient`; the package translates ygo's internal `uint64` IDs and maps at the boundary. `SyncClient` is the mobile-facing wrapper around `provider/client`'s dial/sync loop.

---

## Concurrency model

`Doc` is protected by `sync.RWMutex`. Transactions are serialised. Observer callbacks fire synchronously after the transaction completes. Providers fan out updates to peers under per-room locks.

---

## Garbage collection

When `doc.GC = true` (default), deleted item content is freed at the end of each transaction. Set `doc.GC = false` to preserve full history for snapshots and undo/redo.

---

## Compatibility testing

`testutil/gen_fixtures.js` generates canonical `.bin` files from the JS Yjs reference implementation. These are committed to `testutil/fixtures/` and loaded by `TestCompat_*` tests, which assert exact document state and — for encoding tests — byte-for-byte output equality.
