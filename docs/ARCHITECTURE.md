# Architecture

ygo is a pure-Go implementation of the [Yjs](https://github.com/yjs/yjs) CRDT algorithm. It is **binary-compatible** with the JavaScript reference implementation: updates produced by ygo can be consumed by Yjs clients, and vice versa.

## Package dependency graph

```
provider/{websocket,http}
         │
        sync/ ── awareness/
         │
        crdt/
         │
      encoding/
```

**Rule:** no upward imports. `encoding/` has zero runtime dependencies. `crdt/` depends only on `encoding/`. `sync/` and `awareness/` depend on `crdt/` and `encoding/`. Providers depend on `sync/` and `awareness/`.

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

1. Locate the position immediately after `Origin` in the current list.
2. Scan right past any concurrent items that have the same `Origin` and a **lower** `ClientID` (they win the tie-break).
3. Insert the new item at the resolved position.

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

The protocol is **transport-agnostic**: messages are plain `[]byte` and work over WebSocket, HTTP, WebRTC, or in-process pipes.

---

## `awareness/` — ephemeral state

Separate from document updates. Stores `map[ClientID]AwarenessState{Clock uint64, State any}`.

- Last-write-wins per client by `Clock`.
- States expire after 30 s of inactivity.
- Encoded as `VarUint(numClients)` + per-client `(clientID, clock, jsonState)`.

---

## `provider/` — transport handlers

### `provider/websocket/`

`net/http`-compatible handler. One `Doc` per named room. On connect: exchanges `SyncStep1/2` and awareness state. On message: applies update and broadcasts to all other peers in the room. Accepts a `PersistenceAdapter` interface for pluggable storage backends.

### `provider/http/`

| Method | Path | Semantics |
|--------|------|-----------|
| `GET` | `/doc/{room}?sv=<base64>` | Return binary update diff |
| `POST` | `/doc/{room}` | Apply binary update from request body |

---

## Concurrency model

`Doc` is protected by `sync.RWMutex`. Transactions are serialised. Observer callbacks fire synchronously after the transaction completes. Providers fan out updates to peers under per-room locks.

---

## Garbage collection

When `doc.GC = true` (default), deleted item content is freed at the end of each transaction. Set `doc.GC = false` to preserve full history for snapshots and undo/redo.

---

## Compatibility testing

`testutil/gen_fixtures.js` generates canonical `.bin` files from the JS Yjs reference implementation. These are committed to `testutil/fixtures/` and loaded by `TestCompat_*` tests, which assert exact document state and — for encoding tests — byte-for-byte output equality.
