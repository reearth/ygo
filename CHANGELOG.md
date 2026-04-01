# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [1.0.1] — 2026-04-01

### Added
- `YArray.ToJSON()`, `YMap.ToJSON()`, `YText.ToJSON()` — convenience JSON serialisation methods.
- `YArray.Move(txn, fromIndex, toIndex)` — moves an element to a new logical position within the array.
- `UndoManager.WithTrackedOrigins(...any)` — restricts capture to transactions whose `Origin` matches one of the supplied values; enables per-user undo in multi-author documents.
- `YTextEvent.Delta` is now populated on every observer callback with a Quill-compatible insert/delete/retain changeset for the transaction.

### Fixed

**Security:**
- **C1 — Room memory leak (WebSocket)**: Rooms were never removed from `s.rooms` when all peers disconnected. Fixed: `handleDisconnect` now deletes the room from the server map under the write lock when the last peer leaves.
- **C2 — CORS bypass (WebSocket)**: `CheckOrigin` always returned `true`, enabling Cross-Site WebSocket Hijacking. Fixed: new `AllowedOrigins []string` field on `Server`; when empty, a same-origin check is applied (Origin host must match HTTP Host header); use `"*"` to explicitly allow all origins.
- **C3 — Unbounded VarBytes/VarString allocation (Encoding)**: `ReadVarBytes` allocated a slice sized by an attacker-controlled VarUint before verifying the buffer contained that many bytes. Fixed: length fields exceeding `maxStringBytes` (16 MiB) now return `ErrOverflow`.
- **C4 — Goroutine-unsafe read methods (CRDT types)**: `YArray.Get/ToSlice/ForEach/Slice`, `YText.ToString/ToDelta`, `YMap.Get/Has/Keys/Entries` walked the item linked list without holding the document lock. Fixed: `doc.mu` changed to `sync.RWMutex`; all read methods acquire `RLock` on entry. Read methods must not be called from inside a `Transact` callback.
- **C5 — Observer unsubscribe index-capture bug**: All type-level `Observe` / `ObserveDeep` methods captured the slice index at subscription time; out-of-order unsubscription removed the wrong handler. Fixed: ID-based lookup pattern (same as `Doc.OnUpdate`) applied to `YArray`, `YText`, `YMap`, `YXmlFragment`, `YXmlElement`, and all `ObserveDeep` methods.
- **H1 — Goroutine leak per peer (WebSocket)**: The context-watcher goroutine started in `ServeHTTP` had no guaranteed exit path on normal client disconnect. Fixed: `peer.done chan struct{}` is closed by the read loop; the goroutine selects on it as a third case.
- **H2 — Broadcast-to-closed-peer race (WebSocket)**: `broadcast` snapshots peers then releases the room lock; between snapshot and write, `handleDisconnect` could close `conn`. Fixed: `peer.closed bool` (guarded by `wmu`) is set before the connection is torn down; `write` skips the send if `closed` is true.
- **H3 — Awareness JSON depth unbounded**: `json.Unmarshal` on state strings has no depth limit; a 1 MiB payload of `[[[[...]]]]` triggers quadratic parsing. Fixed: `checkJSONDepth` scans the raw string and rejects inputs exceeding 20 nesting levels before unmarshalling.
- **H5 — Unknown ReadAny tag silent nil**: The default case of `readAny` returned `(nil, nil)`, silently injecting nil into documents. Fixed: returns `(nil, ErrUnknownTag)`.
- **H6 — POST accepts any Content-Type (HTTP)**: `handlePost` applied no Content-Type validation. Fixed: requests with a Content-Type other than `application/octet-stream` are rejected with 415 Unsupported Media Type.
- **L4 — Room name not validated (WebSocket)**: Room names were taken from the URL path without length or character validation. Fixed: `isValidRoomName` enforces max 255 bytes and allows only letters, digits, hyphen, underscore, and dot; invalid names return 400.
- **M2 — Awareness clock uint64 overflow**: `a.clock++` wrapped to 0 after 2^64 increments, making new states appear older. Fixed: saturates at `math.MaxUint64` instead of wrapping.

## [1.0.0] — 2026-04-01

### Added
- `crdt.RelativePosition` / `AbsolutePosition` — stable cursor positions that survive concurrent insertions and deletions. `CreateRelativePositionFromIndex`, `ToAbsolutePosition`, `EncodeRelativePosition`, `DecodeRelativePosition`. Wire format compatible with the Yjs JS reference implementation.
- `crdt.UndoManager` — tracks local transactions on one or more shared types and supports `Undo()` / `Redo()`. Consecutive transactions within a configurable capture timeout (default 500 ms) are merged into a single undo stack item. `OnStackItemAdded` callback hook for attaching cursor metadata. `StopCapturing()` forces an explicit undo boundary.
- `crdt.Doc.OnAfterTransaction` — lower-level observer that fires with the full `*Transaction` (beforeState, afterState, deleteSet, Local flag) after each committed transaction. Used internally by UndoManager; also useful for application code that needs richer change metadata.
- `provider/websocket.Server.AuthFunc` — optional `func(*http.Request) bool` hook called before upgrading each WebSocket connection. Return false to reject with 401 Unauthorized. Use for token validation, session checks, or IP allow-lists.
- Initial repository structure and CI/CD pipeline
- Project architecture documentation
- `sync.ReadSyncMessage` — parses incoming y-protocol messages into type + payload
- `awareness.StartAutoExpiry` — background goroutine that removes stale peer states after a configurable timeout
- `provider/websocket`: `PersistenceAdapter` interface, `MemoryPersistence` in-memory implementation, and `NewServerWithPersistence` constructor for pluggable document storage
- B4 editing-trace benchmark suite (`BenchmarkB4_Apply/Encode/EncodeV2/Decode/Size`) with baseline results in `benchmarks/README.md`
- LRU position cache (80 entries) in `abstractType` for O(1) average-case index lookups

### Changed
- `Doc.OnUpdate` callback signature changed from `func(origin any)` to `func(update []byte, origin any)` — the incremental binary update is now passed directly to observers
- `ClientID` generation changed from `rand.Uint64()` to `rand.Uint32()` to stay within the Yjs wire protocol's 53-bit VarUint limit

### Fixed
- `OnUpdate` unsubscribe closure captured the slice index at subscription time; removing subscribers out-of-order removed the wrong callback. Subscriptions now use a unique uint64 ID and search by ID on unsubscribe.
- ClientID values ≥ 2^53 caused encode/decode round-trip failures (~1 in 256 documents with the old random generation)
- Sequential insertions into large documents degraded to O(n²) because the LRU position cache was cleared on every insertion; cache is now only invalidated on middle insertions
- Crafted binary inputs could trigger multi-GB allocations in all V1/V2 decoder loops; OOM guards added throughout
- `RunGC` rewrote with a correct two-pass algorithm: first pass replaces deleted content with tombstones, second pass merges adjacent tombstones without breaking linked-list references

### Security (pre-release hardening — 2026-04-01)

**Critical fixes:**

- **C1 — Observer deadlock**: `Doc.Transact` previously fired all observer callbacks while holding the document mutex. Any callback that called back into `Transact`, `ApplyUpdate`, or any locked `Doc` method would deadlock permanently. Observers are now snapshotted under the lock and fired after releasing it.
- **C2 — ReadAny stack overflow DoS**: `encoding.Decoder.ReadAny` recursed without a depth limit. A crafted payload with 100 000-deep nested arrays/maps exhausted the goroutine stack. Recursion is now capped at `maxAnyDepth = 100` levels; deeper inputs return `ErrDepthExceeded`.
- **C3 — V2 readLen integer overflow**: `v2Decoder.readLen()` cast `uint64 → int` without bounds checking, silently wrapping large values into negatives that bypassed downstream size guards. Values exceeding `math.MaxInt32` now return `ErrInvalidUpdate`.
- **C4 — YText UTF-16 indexing**: `ContentString.Len()` and `Splice()` previously operated on Unicode rune counts. Yjs and JavaScript use UTF-16 code units. Emoji and other supplementary characters caused index misalignment between ygo and JS peers, silently corrupting collaborative documents. All `ContentString` length arithmetic now uses UTF-16 code units.
- **C6 — Unbounded WebSocket / HTTP body**: The WebSocket server had no per-frame size limit; the HTTP provider had no body size limit. Both accepted arbitrarily large inputs before any validation, enabling OOM with a single request. WebSocket frames are now capped at 64 MiB via `conn.SetReadLimit`; HTTP POST bodies via `http.MaxBytesReader`.
- **C7 — Awareness OOM**: `Awareness.ApplyUpdate` allocated a slice sized by the attacker-controlled `numClients` field before any validation. Sending `numClients = 2^63` caused a multi-exabyte allocation attempt. Inputs are now rejected if `numClients > maxAwarenessClients (100 000)` or any single state JSON exceeds `maxAwarenessStateBytes (1 MiB)`.
- **C9 — V1 struct count unbounded**: V2 decoding already capped the total item count via `maxV2Items`; V1 decoding did not. A crafted V1 update could loop indefinitely allocating items. V1 now applies the same `totalStructs ≤ maxV2Items` check.
- **C10 — Panic on unsplittable content**: A crafted update could encode a `ContentBinary`, `ContentEmbed`, `ContentFormat`, `ContentType`, or `ContentDoc` item at a clock offset that forced a split, triggering `panic("… is not splittable")` in the receiving process. `applyV1Txn` and `applyV2Txn` now recover such panics and return `ErrInvalidUpdate`.
- **C11 — LICENSE mismatch**: The README badge incorrectly claimed "Apache 2.0"; the LICENSE file has always been MIT. Badge corrected.
- **C12 — No context / shutdown support**: `Doc.TransactContext` added for context-aware transaction entry. WebSocket `Server.Shutdown(ctx)` closes all peer connections and returns. The per-peer read loop now exits when the HTTP request context is cancelled (e.g. on graceful server shutdown). A `writeTimeout` write deadline is set on every WebSocket write to prevent slow readers from blocking the broadcast loop.

[1.0.0]: https://github.com/reearth/ygo/releases/tag/v1.0.0
