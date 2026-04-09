# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [1.0.2] — 2026-04-09

### Added
- `Doc.GUID()` accessor and `WithGUID(string)` option for subdocument identity.

### Fixed

- **V1 GC struct decoding (tag 0)**: Yjs encodes garbage-collected items as `{info=0, VarUint(length)}`. The V1 decoder didn't recognize tag 0, misaligning the decoder for all subsequent items. Fixed: tag 0 returns a `ContentDeleted` placeholder added directly to the store.
- **V1 skip struct decoding (tag 10)**: Yjs uses skip structs for clock-range placeholders in partial updates. The V1 decoder rejected them as "unknown content tag: 10". Fixed: tag 10 is decoded and the clock advances without storing anything, matching V2 behavior.
- **Cross-client parent resolution (V1 and V2)**: When items from a lower-client-ID group reference items from a higher-client-ID group via `Origin`, the parent resolution failed because the higher group hadn't been decoded yet. Fixed: unresolvable items are collected in a pending queue and retried in a loop after all client groups are decoded.
- **ContentDoc discarded subdocument GUID**: Both V1 and V2 decoders read the subdocument GUID from the wire but discarded it, creating an empty Doc. Fixed: GUID is preserved via `WithGUID` and correctly round-trips through V1 and V2 encoding.
- **Room name validation too restrictive**: `isValidRoomName` only allowed `[A-Za-z0-9._-]`, rejecting room names with spaces or Unicode that the y-websocket JS client permits. Fixed: now allows all printable characters (rejects only control chars, empty string, `"."`, `".."`, and names > 255 bytes).
- **y-websocket auth message (type 2) unhandled**: Message type 2 (auth) is defined by y-websocket but was not explicitly handled. Fixed: silently ignored with a documented `case msgAuth`.

### Changed
- `YArray.Move` godoc now warns that it is not CRDT-safe for concurrent multi-client use (delete-then-insert loses causal history).

## [1.0.1] — 2026-04-09

### Fixed

- **Room-splitting race in WebSocket server**: `handleDisconnect` checked room emptiness without holding the room lock under the server map lock, allowing a concurrent join to slip in between the check and room deletion. This could fork one logical document into two independent rooms for the same name. Fixed: peer removal and room deletion are now atomic under both `server.rmu` and `room.mu` (consistent lock ordering); peer addition in `ServeHTTP` holds `server.rmu.RLock` to prevent concurrent room deletion.
- **Invalid awareness updates broadcast to all peers**: The `msgAwareness` handler ignored the return value of `Awareness.ApplyUpdate` and broadcast the raw payload unconditionally. A malicious peer could fan out rejected payloads to every client in the room. Fixed: updates that fail server-side validation are now dropped silently.
- **Persistence failures silently converted to success**: `LoadDoc` and `ApplyUpdateV1` errors during room bootstrap were ignored, and `StoreUpdate` ran in fire-and-forget goroutines that swallowed both panics and errors. After a restart, accepted edits could vanish. Fixed: `getOrCreateRoom` propagates persistence errors (returns HTTP 500); `StoreUpdate` writes are serialised through a per-room buffered channel with error/panic logging; `Shutdown` waits for all persistence goroutines to drain.

## [1.0.0] — 2026-04-01

### Added
- `YArray.ToJSON()`, `YMap.ToJSON()`, `YText.ToJSON()` — convenience JSON serialisation methods.
- `YArray.Move(txn, fromIndex, toIndex)` — moves an element to a new logical position within the array.
- `UndoManager.WithTrackedOrigins(...any)` — restricts capture to transactions whose `Origin` matches one of the supplied values; enables per-user undo in multi-author documents.
- `YTextEvent.Delta` is now populated on every observer callback with a Quill-compatible insert/delete/retain changeset for the transaction.
- `crdt.RelativePosition` / `AbsolutePosition` — stable cursor positions that survive concurrent insertions and deletions. `CreateRelativePositionFromIndex`, `ToAbsolutePosition`, `EncodeRelativePosition`, `DecodeRelativePosition`. Wire format compatible with the Yjs JS reference implementation.
- `crdt.UndoManager` — tracks local transactions on one or more shared types and supports `Undo()` / `Redo()`. Consecutive transactions within a configurable capture timeout (default 500 ms) are merged into a single undo stack item. `OnStackItemAdded` callback hook for attaching cursor metadata. `StopCapturing()` forces an explicit undo boundary.
- `crdt.Doc.OnAfterTransaction` — lower-level observer that fires with the full `*Transaction` (beforeState, afterState, deleteSet, Local flag) after each committed transaction. Used internally by UndoManager; also useful for application code that needs richer change metadata.
- `provider/websocket.Server.AuthFunc` — optional `func(*http.Request) bool` hook called before upgrading each WebSocket connection. Return false to reject with 401 Unauthorized.
- `provider/websocket.Server.MaxConnections` and `MaxPeersPerRoom` — server-wide and per-room peer caps; requests that would exceed either limit receive 503 before the WebSocket upgrade.
- Initial repository structure and CI/CD pipeline.
- `sync.ReadSyncMessage` — parses incoming y-protocol messages into type + payload.
- `awareness.StartAutoExpiry` — background goroutine that removes stale peer states after a configurable timeout.
- `provider/websocket`: `PersistenceAdapter` interface, `MemoryPersistence` in-memory implementation, and `NewServerWithPersistence` constructor for pluggable document storage.
- B4 editing-trace benchmark suite (`BenchmarkB4_Apply/Encode/EncodeV2/Decode/Size`) with baseline results in `benchmarks/README.md`.
- LRU position cache (80 entries) in `abstractType` for O(1) average-case index lookups.

### Changed
- `Doc.OnUpdate` callback signature changed from `func(origin any)` to `func(update []byte, origin any)` — the incremental binary update is now passed directly to observers.
- `ClientID` generation changed from `rand.Uint64()` to `rand.Uint32()` to stay within the Yjs wire protocol's 53-bit VarUint limit.
- `Doc.ClientID` and `Doc.GC` are now unexported (`clientID`, `gc`). Use `WithClientID` and `WithGC` options at construction time; a read-only `ClientID() ClientID` getter is provided.

### Fixed

- **V2 XML type-class mismatch**: `typeClassOf` encoded `YXmlText` as type-ref 5, but the V2 decoder reserved 5 for `YXmlHook` (which reads an extra key field) and used 6 for `YXmlText`. This caused `ApplyUpdateV2` to fail with "unexpected end of input" for any document containing `YXmlText` nodes. Both the V1 and V2 decoders now use type-ref 6 for `YXmlText`, matching the Yjs wire protocol.

**Security — Critical:**
- **C1 — Observer registration/fire data race**: `Observe()` and `ObserveDeep()` mutated per-type observer slices without holding the document lock while `Transact` read those slices outside the lock. Fixed: `prepareFire()` snapshots the observer slice inside the write lock and returns a pre-built closure; `Observe()`, `ObserveDeep()`, and their unsubscribe functions now acquire `doc.mu.Lock()`.
- **C2 — ReadAny array/map allocation OOM bypass**: The `n > d.Remaining()` guard was insufficient — `make([]any, 1_000_000)` allocates ~8 MiB before any element is decoded even if each element is 1 byte. Fixed: `const maxAnyElements = 100_000`; both array and map allocation return `ErrDepthExceeded` when exceeded.
- **C3 — checkJSONDepth miscounts brackets inside JSON strings**: `{"key": "[[[["}` was incorrectly counted as depth 5 (4 false-positive brackets). Fixed: tracks `inString` and escape context.
- **C4 — WriteVarInt(math.MinInt64) integer overflow**: `uint64(-math.MinInt64)` overflows in Go's two's complement. Fixed: special-cased to `mag = 1 << 63`.
- **C5 — Observer unsubscribe index-capture bug**: All type-level `Observe` / `ObserveDeep` methods captured the slice index at subscription time; out-of-order unsubscription removed the wrong handler. Fixed: ID-based lookup pattern applied to all CRDT types.
- **C6 — Goroutine-unsafe read methods**: `YArray.Get/ToSlice/ForEach/Slice`, `YText.ToString/ToDelta`, `YMap.Get/Has/Keys/Entries` walked the item linked list without holding the document lock. Fixed: `doc.mu` changed to `sync.RWMutex`; all read methods acquire `RLock` on entry.
- **C7 — Observer deadlock**: `Doc.Transact` previously fired all observer callbacks while holding the document mutex. Any callback that called back into `Transact`, `ApplyUpdate`, or any locked `Doc` method would deadlock. Observers are now snapshotted under the lock and fired after releasing it.
- **C8 — ReadAny stack overflow DoS**: `encoding.Decoder.ReadAny` recursed without a depth limit. Fixed: recursion capped at `maxAnyDepth = 100` levels.
- **C9 — V2 readLen integer overflow**: `v2Decoder.readLen()` cast `uint64 → int` without bounds checking. Fixed: values exceeding `math.MaxInt32` return `ErrInvalidUpdate`.
- **C10 — YText UTF-16 indexing**: `ContentString.Len()` and `Splice()` previously operated on Unicode rune counts. Fixed: all `ContentString` length arithmetic now uses UTF-16 code units.
- **C11 — Unbounded WebSocket / HTTP body**: Fixed: WebSocket frames capped at 64 MiB via `conn.SetReadLimit`; HTTP POST bodies via `http.MaxBytesReader`.
- **C12 — Awareness OOM**: `Awareness.ApplyUpdate` allocated a slice sized by the attacker-controlled `numClients` field. Fixed: inputs rejected if `numClients > maxAwarenessClients (100,000)` or any single state JSON exceeds `maxAwarenessStateBytes (1 MiB)`.
- **C13 — V1 struct count unbounded**: V1 decoding could loop indefinitely allocating items. Fixed: same `totalStructs ≤ maxV2Items` check applied.
- **C14 — Panic on unsplittable content**: A crafted update could force a split on non-splittable content types. Fixed: `applyV1Txn` and `applyV2Txn` recover such panics and return `ErrInvalidUpdate`.
- **C15 — CORS bypass (WebSocket)**: `CheckOrigin` always returned `true`. Fixed: new `AllowedOrigins []string` field; same-origin fallback when empty; `"*"` to explicitly allow all.
- **C16 — Room memory leak (WebSocket)**: Rooms were never removed from `s.rooms` when all peers disconnected. Fixed: `handleDisconnect` deletes the room when the last peer leaves.
- **C17 — Unbounded VarBytes/VarString allocation**: `ReadVarBytes` allocated before verifying buffer size. Fixed: length fields exceeding `maxStringBytes` (16 MiB) return `ErrOverflow`.

**Security — High:**
- **H1 — O(n²) in DeleteSet.applyTo**: Triple loop scaled as O(n²) for large stores. Fixed: binary search to the first item in each range; break when past the range end.
- **H2 — Integer underflow in store.getItemCleanEnd**: `clock - item.ID.Clock + 1` would wrap for malformed updates. Fixed: guard before arithmetic.
- **H3 — CreateRelativePositionFromIndex missing doc lock**: Walked the item list without a read lock. Fixed: acquires `doc.mu.RLock()` for the walk.
- **H4 — Unbounded awareness clients per peer**: `trackAwarenessClients` map grew unboundedly. Fixed: `const maxAwarenessClientsPerPeer = 10_000`.
- **H5 — Sequential broadcast stalls all peers**: Writing to N slow peers sequentially with 10s deadline each could stall updates. Fixed: each peer write runs in its own goroutine.
- **H6 — Persistence StoreUpdate blocks broadcast loop**: `StoreUpdate` called synchronously in the `OnUpdate` callback. Fixed: runs in a separate goroutine.
- **H7 — Goroutine leak per peer (WebSocket)**: The context-watcher goroutine had no guaranteed exit path. Fixed: `peer.done chan struct{}` closed by the read loop.
- **H8 — Broadcast-to-closed-peer race (WebSocket)**: `broadcast` could write to a peer after `handleDisconnect` closed its connection. Fixed: `peer.closed bool` (guarded by `wmu`) checked before every write.
- **H9 — Awareness JSON depth unbounded**: `json.Unmarshal` on state strings had no depth limit. Fixed: `checkJSONDepth` rejects inputs exceeding 20 nesting levels.
- **H10 — Unknown ReadAny tag silent nil**: The default case of `readAny` returned `(nil, nil)`, silently injecting nil into documents. Fixed: returns `(nil, ErrUnknownTag)`.
- **H11 — POST accepts any Content-Type (HTTP)**: Fixed: requests with a Content-Type other than `application/octet-stream` are rejected with 415.
- **H12 — Room name not validated (WebSocket)**: Fixed: `isValidRoomName` enforces max 255 bytes and allows only letters, digits, hyphen, underscore, and dot.

**Security — Medium:**
- **M1 — HTTP ClientID used rand.Uint64()**: IDs > 2^53 break JS interop. Fixed: changed to `rand.Uint32()`.
- **M2 — WriteAny silently encoded unsupported types as null**: Channels, funcs, and other unsupported types caused data loss. Fixed: panics with a descriptive message including the type name.
- **M3 — Non-deterministic map key encoding in WriteAny**: Fixed: keys sorted before encoding.
- **M4 — HTTP error messages leaked internal decoder details**: Fixed: generic message returned.
- **M5 — Awareness clock uint64 overflow**: `a.clock++` wrapped to 0 after 2^64 increments. Fixed: saturates at `math.MaxUint64`.

**Correctness:**
- `OnUpdate` unsubscribe closure captured the slice index at subscription time; subscriptions now use a unique uint64 ID and search by ID on unsubscribe.
- `ClientID` values ≥ 2^53 caused encode/decode round-trip failures (~1 in 256 documents). Fixed via `rand.Uint32()` default.
- Sequential insertions into large documents degraded to O(n²); LRU position cache now only invalidated on middle insertions.
- Crafted binary inputs could trigger multi-GB allocations in V1/V2 decoder loops; OOM guards added throughout.
- `RunGC` rewritten with a correct two-pass algorithm.
- `YArray.Move` had two bugs: (1) the `toIndex > fromIndex` adjustment caused adjacent forward moves to be no-ops; (2) calling `Get()` (which acquires `doc.mu.RLock()`) from inside a Transact callback (which holds `doc.mu.Lock()`) caused a deadlock. Both fixed.
- `Doc.TransactContext` added for context-aware transaction entry.
- WebSocket `Server.Shutdown(ctx)` closes all peer connections and waits for goroutines to exit.

[1.0.2]: https://github.com/reearth/ygo/releases/tag/v1.0.2
[1.0.1]: https://github.com/reearth/ygo/releases/tag/v1.0.1
[1.0.0]: https://github.com/reearth/ygo/releases/tag/v1.0.0
