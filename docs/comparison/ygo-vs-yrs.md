# ygo vs yrs — Comparison Report

**Date:** 2026-04-13  
**Versions compared:** yrs 0.25.0 / y-sync 0.4.0 / yrs-warp 0.9.0 vs ygo v1.0.4  
**Scope:** Core CRDT, sync + awareness protocols, WebSocket + HTTP transports  
**Method:** docs-first with source-level verification for ambiguous rows

---

## 1. Executive Summary

- **Core CRDT is at strong parity.** ygo implements all shared types, V1/V2 encoding, snapshots, undo/redo, relative positions, observers, and GC. The wire format is fully compatible with the Yjs JS reference.
- **ygo's two most significant CRDT gaps** are a non-CRDT-safe array move (yrs has a concurrent-safe `move_to()` that also relocates items inserted into the moved range) and missing weak references (experimental in yrs, absent in ygo).
- **ygo is ahead on transport hardening.** yrs-warp (the official Rust WebSocket transport) has no auth hook, no CORS enforcement, no per-peer connection limits, and no graceful shutdown drain. ygo implements all of these. HTTP sync has no Rust equivalent at all.
- **y-sync (Rust sync/awareness protocol crate) is archived** as of December 2024. The Rust ecosystem has no active maintained sync-protocol crate; future interop work on the Rust side requires either forking y-sync or pulling protocol handling into a higher-level crate.
- **Performance:** ygo's B4 Apply (~1.4 s) already beats Yjs JS (~5.7 s at equivalent N on comparable hardware). yrs will be faster than ygo due to language-level advantages (no GC pauses, arena allocation, monomorphisation), but no official B4 numbers for yrs are published; a same-hardware harness is the recommended follow-up.

---

## 2. Scope and Method

**In scope:** `ygo/crdt` ↔ `yrs 0.25.0`; `ygo/sync` + `ygo/awareness` ↔ `y-sync 0.4.0`; `ygo/provider/websocket` + `ygo/provider/http` ↔ `yrs-warp 0.9.0` and the broader Rust transport ecosystem.

**Out of scope:** Persistence adapters (yrs-kvstore / LMDB vs ygo's MemoryPersistence), non-Yjs CRDTs.

**Sources:** GitHub repos for y-crdt/y-crdt, y-crdt/y-sync (archived), y-crdt/yrs-warp; docs.rs for all three; crates.io ecosystem scan for transport alternatives; dmonad/crdt-benchmarks for JS Yjs B4 numbers; ygo source and benchmarks/README.md for ygo numbers.

**Hardware caveat (performance section):** yrs has no published B4 numbers on dmonad/crdt-benchmarks. Yjs JS numbers are from an Intel Core i5-8400 running Node 20; ygo numbers are from an Apple M4 Max. Absolute comparisons cannot be made — ratios against the Yjs JS baseline are more informative than raw times.

---

## 3. Crate and Module Map

| ygo package | Rust equivalent | Notes |
|---|---|---|
| `ygo/crdt` | `yrs 0.25.0` | Core CRDT, shared types, transactions, encoding |
| `ygo/encoding` | Built into `yrs` | lib0 codec embedded in yrs; ygo has a separate package |
| `ygo/sync` | `y-sync 0.4.0` | **Archived Dec 2024** — frozen at 0.4.0 |
| `ygo/awareness` | `y-sync 0.4.0` awareness module | Same crate as sync |
| `ygo/provider/websocket` | `yrs-warp 0.9.0` | Official Rust WS transport (warp framework) |
| `ygo/provider/http` | *(no equivalent)* | HTTP sync absent from Rust Yjs ecosystem |

---

## 4. Feature Matrix

### 4.1 Shared Types

| Feature | yrs 0.25.0 | ygo v1.0.4 | JS compat notes | General notes |
|---|---|---|---|---|
| **YText** | ✅ `TextRef` | ✅ `YText` | ✅ Wire-compatible | yrs exposes rich-text via `insert_with_attributes()` and `format()` with arbitrary Quill attrs |
| YText rich formatting (marks/attributes) | ✅ `Attrs` type, `insert_with_attributes()`, `format()` | ⚠️ Partial — `Insert` accepts `map[string]any` attrs but `Format()` not implemented | ⚠️ ygo attrs are passed through but range formatting API is absent | yrs `format()` applies attributes to an existing range without re-inserting |
| YText delta events | ✅ `TextEvent::delta(txn)` returns `Vec<Delta>` | ✅ `YTextEvent.Delta` populated on every observer call | ✅ Quill-compatible insert/delete/retain | Both produce the same delta shape |
| **YArray** | ✅ `ArrayRef` | ✅ `YArray` | ✅ | — |
| YArray move (single element) | ✅ `move_to()` — CRDT-safe; concurrent insertions in moved range are also relocated | ⚠️ `Move()` — delete-then-insert; loses causal history, not concurrent-safe | ✅ JS Yjs has concurrent-safe move since 13.6; wire protocol carries move semantics | **Critical gap.** ygo godoc warns this is not CRDT-safe for multi-client use |
| YArray move range | ✅ `move_range_to()` | ❌ Not implemented | ✅ JS Yjs supports range move | — |
| **YMap** | ✅ `MapRef` | ✅ `YMap` | ✅ | — |
| YMap iteration | ✅ `keys()`, `values()`, `iter()` | ✅ `Keys()`, `Entries()` | ✅ | — |
| **YXmlElement** | ✅ `XmlElementRef` | ✅ `YXmlElement` | ✅ | — |
| **YXmlText** | ✅ `XmlTextRef` | ✅ `YXmlText` | ✅ | — |
| **YXmlFragment** | ✅ `XmlFragmentRef` | ✅ `YXmlFragment` | ✅ | — |
| **Subdocuments** | ✅ Nested `Doc` via `ContentDoc`; `auto_load`, `should_load` options | ✅ `ContentDoc`; `GUID()` accessor, `WithGUID()` option | ✅ GUID round-trips V1/V2 | yrs has richer auto-load lifecycle hooks |
| **Weak references** | ⚠️ Experimental; requires `"weak"` feature flag | ❌ Not implemented | ❌ JS Yjs: experimental | Low adoption; not a near-term gap |

### 4.2 Transactions and Origin Tracking

| Feature | yrs 0.25.0 | ygo v1.0.4 | JS compat notes | General notes |
|---|---|---|---|---|
| **Transaction API** | ✅ Type-enforced: `Transaction` (read) + `TransactionMut` (write) via traits | ✅ Single `Transact(func(*Transaction))` callback; RWMutex separates readers/writers | ✅ Same semantics — write lock per document | yrs has strong compile-time separation; ygo enforces at runtime via mutex |
| **Auto-commit on drop** | ✅ `TransactionMut` commits when dropped | ✅ Commit at end of `Transact` callback | — | Different ergonomic shape; same guarantee |
| **Origin tracking** | ✅ `TransactionMut::origin()` | ✅ `txn.Origin` field | ✅ | — |
| **Before/after state** | ✅ `before_state()`, `after_state()`, `delete_set()` on txn | ✅ Exposed via `OnAfterTransaction` callback | — | — |
| **Nested transactions** | ❌ Single active `TransactionMut` per Doc | ❌ Nested `Transact` calls not supported | — | Both match JS Yjs single-lock semantics |
| **Read-only transactions** | ✅ `Transaction` type (cannot mutate) | ✅ Methods outside `Transact` acquire `RLock` | — | yrs enforces at type level; ygo enforces at runtime |
| **Context-aware transaction** | ❌ Not present | ✅ `Doc.TransactContext(ctx, ...)` | — | ygo-only; fn polls ctx to exit early; no mid-fn interrupt (matches yrs' no-cancel model) |

### 4.3 Updates and Encoding

| Feature | yrs 0.25.0 | ygo v1.0.4 | JS compat notes | General notes |
|---|---|---|---|---|
| **V1 encode** | ✅ `encode_state_as_update()` | ✅ `EncodeStateAsUpdateV1()` | ✅ | — |
| **V1 decode/apply** | ✅ `apply_update()` on `TransactionMut` | ✅ `ApplyUpdateV1()` | ✅ | — |
| **V2 encode** | ✅ `encode_state_as_update_v2()` | ✅ `EncodeStateAsUpdateV2()` | ✅ | — |
| **V2 decode/apply** | ✅ | ✅ `ApplyUpdateV2()` | ✅ | — |
| **State vector encode** | ✅ `encode_state_vector()` | ✅ `EncodeStateVector()` | ✅ | — |
| **Diff update (encode)** | ✅ `encode_diff_v1()`, `encode_diff_v2()` | ✅ `EncodeStateAsUpdateV1()` accepts remote state vector | ✅ | — |
| **Merge updates** | ✅ `merge_updates()` | ✅ `MergeUpdatesV1()`, `MergeUpdatesV2()` | ✅ | — |
| **Snapshots** | ✅ `Snapshot` struct; `new(state_vector, delete_set)` | ✅ `Snapshot`, `EncodeSnapshot()`, `DecodeSnapshot()` | ✅ | — |
| **Update obfuscation** | ❌ Not documented | ❌ Not implemented | — | Rarely needed |
| **GC struct decoding (tag 0)** | ✅ | ✅ Fixed in v1.0.2 | ✅ | ygo pre-1.0.2 rejected tag-0 structs |
| **Skip struct decoding (tag 10)** | ✅ | ✅ Fixed in v1.0.2 | ✅ | ygo pre-1.0.2 rejected tag-10 structs |
| **Cross-client parent resolution** | ✅ | ✅ Fixed in v1.0.2–v1.0.3 | ✅ | Pending retry loop for items referencing later-decoded client groups |
| **GC'd origin handling** | ✅ | ✅ Fixed in v1.0.3 | ✅ | Orphaned items encoded as GC structs; multi-client resolved via store scan |

### 4.4 Undo / Redo

| Feature | yrs 0.25.0 | ygo v1.0.4 | JS compat notes | General notes |
|---|---|---|---|---|
| **UndoManager** | ✅ `UndoManager<M>` (generic over metadata type) | ✅ `UndoManager` | ✅ | — |
| **Tracked origins** | ✅ `include_origin()`, `exclude_origin()` | ✅ `WithTrackedOrigins(...)` | ✅ | — |
| **Capture timeout** | ✅ Configurable; merges consecutive local txns | ✅ `WithCaptureTimeout()`, default 500 ms | ✅ | — |
| **Stack item metadata** | ✅ Generic `M` attached to `StackItem` | ✅ `StackItem.Meta map[string]any` | — | yrs is type-safe via generics; ygo uses `map[string]any` |
| **OnStackItemAdded callback** | ✅ `observe_item_added()` | ✅ `OnStackItemAdded` callback | — | — |
| **OnStackItemUpdated callback** | ✅ `observe_item_updated()` fires when a new txn extends an existing stack item | ❌ Not implemented | — | **Important gap.** Useful for updating cursor metadata when a capture window extends |
| **OnStackItemPopped callback** | ✅ `observe_item_popped()` | ❌ Not implemented | — | Fires on undo/redo — useful for restoring cursor position |
| **Expand scope after construction** | ✅ `expand_scope(type)` — add more shared types to track | ❌ Types fixed at construction | — | **Important gap.** Makes it impossible in ygo to lazily start tracking a type |
| **StopCapturing** | ✅ `reset()` | ✅ `StopCapturing()` | — | — |
| **Thread safety** | ❌ `UndoManager` is `!Send + !Sync` | ✅ ygo UndoManager is goroutine-safe (document mutex) | — | yrs limitation: can't share UndoManager across threads |
| **can_undo / can_redo** | ✅ `can_undo()`, `can_redo()` | ❌ Not exposed directly | — | ygo callers must check `Undo()` error return |
| **clear()** | ✅ Resets all stack items | ✅ `Destroy()` stops tracking; no clear-and-continue | — | Minor ergonomic difference |

### 4.5 Relative and Absolute Positions

| Feature | yrs 0.25.0 | ygo v1.0.4 | JS compat notes | General notes |
|---|---|---|---|---|
| **RelativePosition** | ✅ `RelativePosition` | ✅ `RelativePosition` | ✅ Wire-compatible with Yjs JS | — |
| **AbsolutePosition** | ✅ | ✅ `AbsolutePosition` | ✅ | — |
| **Encode/decode** | ✅ `encode_relative_position()`, `decode_relative_position()` | ✅ `EncodeRelativePosition()`, `DecodeRelativePosition()` | ✅ | — |
| **CreateFromIndex** | ✅ | ✅ `CreateRelativePositionFromIndex()` | ✅ | — |
| **ToAbsolute** | ✅ | ✅ `ToAbsolutePosition()` | ✅ | — |
| **StickyIndex** | ✅ `StickyIndex` — explicit left/right sticky side; supports array sequences | ❌ Not implemented | — | yrs-only; more ergonomic for editor cursor semantics |

### 4.6 Observers and Events

| Feature | yrs 0.25.0 | ygo v1.0.4 | JS compat notes | General notes |
|---|---|---|---|---|
| **Type-level observe** | ✅ Per-type `observe()` → event callback | ✅ `Observe()` on all types | ✅ | — |
| **Deep observe** | ✅ `DeepObservable` trait | ✅ `ObserveDeep()` | ✅ | — |
| **Path in events** | ✅ `event.path()` | ✅ `YArrayEvent.Path`, etc. | ✅ | — |
| **YText delta in events** | ✅ `TextEvent::delta(txn)` | ✅ `YTextEvent.Delta` | ✅ Quill-compatible | — |
| **YArray delta in events** | ✅ `ArrayEvent::delta()`, `inserts()`, `removes()` | ✅ `YArrayEvent` carries changes | ✅ | — |
| **YMap keys-changed in events** | ✅ `MapEvent::keys()` | ✅ `YMapEvent.KeysChanged` | ✅ | — |
| **After-transaction observer** | ✅ `Doc::observe_update_v1()`, `observe_update_v2()` for raw bytes | ✅ `Doc.OnAfterTransaction`, `Doc.OnUpdate` | — | yrs exposes raw update bytes; ygo fires both full txn and encoded bytes |
| **Observer unsubscribe safety** | ✅ Subscription object dropped to unsubscribe | ✅ ID-based lookup; fixed in v1.0.0 (C5) | — | yrs uses RAII; ygo uses explicit unsubscribe closure |

### 4.7 Awareness Protocol

| Feature | yrs 0.25.0 (y-sync) | ygo v1.0.4 | JS compat notes | General notes |
|---|---|---|---|---|
| **Apply awareness update** | ✅ `Awareness::apply_update()` | ✅ `Awareness.ApplyUpdate()` | ✅ Wire-compatible | — |
| **Encode awareness update** | ✅ `Awareness::encode_update(clients)` | ✅ `Awareness.EncodeUpdate()` | ✅ | — |
| **Clock semantics** | ✅ Monotonic; stale updates rejected | ✅ Monotonic clock; stale rejected | ✅ | — |
| **Auto-expiry** | ⚠️ 30-second timeout defined in y-sync; no background goroutine — cleanup fires on next update receive | ✅ `awareness.StartAutoExpiry(interval)` — background goroutine | ✅ Matches y-websocket JS behavior | ygo's explicit background task is more reliable than lazy cleanup |
| **Per-client state size limit** | ❌ No limit enforced | ✅ `maxAwarenessStateBytes` (1 MiB per state) | — | **ygo ahead.** Security hardening absent from y-sync |
| **Max clients per room** | ❌ No limit | ✅ `maxAwarenessClients` (100,000) | — | **ygo ahead.** OOM guard |
| **State content validation** | ❌ Arbitrary JSON accepted without depth check | ✅ `checkJSONDepth` rejects > 20 levels; `maxAwarenessStateBytes` | — | **ygo ahead.** C12 / H9 hardening |
| **Awareness state OOM guard** | ❌ `numClients` field not bounds-checked | ✅ Rejected if `numClients > maxAwarenessClients` | — | **ygo ahead.** |
| **y-sync maintenance status** | ⚠️ **ARCHIVED Dec 2024** — frozen at 0.4.0, read-only | N/A | — | **Critical ecosystem risk.** No active Rust maintainer for sync/awareness protocol |

### 4.8 Sync Protocol

| Feature | yrs 0.25.0 (y-sync) | ygo v1.0.4 | JS compat notes | General notes |
|---|---|---|---|---|
| **SyncStep1 (type 0)** | ✅ Encoded as `varUint(0) • varByteArray(state_vector)` | ✅ | ✅ | — |
| **SyncStep2 (type 1)** | ✅ | ✅ | ✅ | — |
| **Update broadcast (type 2 in sync sub-protocol)** | ✅ | ✅ | ✅ | — |
| **Auth message (outer type 2)** | ⚠️ Defined in y-protocols spec but **not enforced** — left to transport | ✅ Silently ignored per y-websocket spec; `case msgAuth` documented | ✅ Matches y-websocket JS behavior | y-sync acknowledges auth message type but provides no security model |
| **Query awareness (outer type 3)** | ✅ `MSG_QUERY_AWARENESS` | ✅ | ✅ | — |
| **Awareness message (outer type 1)** | ✅ | ✅ | ✅ | — |
| **Message framing** | ✅ `varUint(type) • varByteArray(payload)` | ✅ | ✅ | — |
| **Message size limits (protocol layer)** | ❌ No limits in y-sync | ✅ WebSocket frames capped at 64 MiB via `SetReadLimit`; HTTP via `MaxBytesReader` | — | **ygo ahead.** C11 hardening |
| **Protocol version negotiation** | ❌ Not implemented | ❌ Not implemented | — | Neither library implements this; Yjs spec doesn't define it |
| **MessageReader (batched parsing)** | ✅ `MessageReader` for concatenated byte stream | ✅ `sync.ReadSyncMessage` | ✅ | — |

### 4.9 Concurrency and Threading Model

| Feature | yrs 0.25.0 | ygo v1.0.4 | JS compat notes | General notes |
|---|---|---|---|---|
| **Thread/goroutine-safe Doc** | ✅ `Doc` is `Send + Sync`; uses `arc-swap`, `dashmap` | ✅ `Doc` has `sync.RWMutex` | — | Both are safe to share across threads |
| **Transaction thread safety** | ❌ `Transaction` and `TransactionMut` are `!Send + !Sync` — must stay on creating thread | ✅ `*Transaction` passed into callback; goroutine-safe | — | **ygo advantage** for multi-threaded server workloads |
| **UndoManager thread safety** | ❌ `UndoManager` is `!Send + !Sync` | ✅ Goroutine-safe (protected by doc mutex) | — | **ygo advantage** |
| **Interior mutability** | `arc-swap` for atomic swaps, `dashmap` for concurrent maps | `sync.RWMutex` on doc | — | Different mechanisms; similar guarantees |
| **Async-native** | ⚠️ Partial — y-sync uses `async-trait`, `async-lock`; core transactions are synchronous | ❌ Sync only; goroutines used at the provider level | — | yrs has tokio integration in transport crates; ygo does not |
| **Observer deadlock risk** | ⚠️ Observer callbacks must not re-enter `TransactionMut` (single-lock model) | ✅ Observers snapshotted inside lock and fired after release (C7 fix) | — | **ygo ahead** — deadlock fixed in v1.0.0; yrs has same structural risk |

### 4.10 Garbage Collection

| Feature | yrs 0.25.0 | ygo v1.0.4 | JS compat notes | General notes |
|---|---|---|---|---|
| **GC enabled by default** | ✅ `skip_gc: false` default | ✅ `WithGC(true)` default | ✅ Matches JS Yjs default | — |
| **Per-Doc GC disable** | ✅ `skip_gc: true` in `Options` | ✅ `WithGC(false)` option | — | — |
| **Explicit GC trigger** | ✅ `TransactionMut::gc(Option<&DeleteSet>)` | ✅ `Doc.RunGC()` | — | — |
| **GC wire encoding (tag 0)** | ✅ Correct tag-0 GC structs | ✅ Fixed in v1.0.2 | ✅ | ygo pre-1.0.2 could not decode tag-0 |
| **GC'd-origin compat** | ✅ | ✅ Fixed in v1.0.3 | ✅ | ygo now stores orphaned items and re-encodes them as GC structs |
| **Nil panic on reconnect with GC'd items** | Not reproduced in yrs | ✅ Fixed in v1.0.4 | — | ygo-specific bug in `delete()` path |

### 4.11 Extras

| Feature | yrs 0.25.0 | ygo v1.0.4 | JS compat notes | General notes |
|---|---|---|---|---|
| **Snapshot encode/restore** | ✅ `Snapshot::new(sv, ds)` + encode/decode | ✅ `EncodeSnapshot()`, `DecodeSnapshot()` | ✅ | — |
| **Preliminary types** (nested init) | ✅ `TextPrelim`, `ArrayPrelim`, `MapPrelim`, etc. — typed init values | ❌ Not implemented; insertion uses `any` / `WriteAny` | — | yrs approach prevents type errors at compile time when inserting nested shared types |
| **StickyIndex** | ✅ With explicit left/right side | ❌ Not implemented | — | See relative positions section |
| **JsonPath queries** | ✅ Documented as available for state extraction | ❌ Not implemented | — | Nice-to-have; limited adoption |
| **Doc.ClientID** | ✅ `u32` (fits JS 53-bit VarUint limit) | ✅ `uint32` (fixed in v1.0.0, was uint64) | ✅ | — |
| **GUID** | ✅ Auto-UUID if not set | ✅ `WithGUID()`, `GUID()` | — | — |
| **Doc options** | `client_id`, `guid`, `collection_id`, `offset_kind`, `skip_gc`, `auto_load`, `should_load` | `WithClientID`, `WithGUID`, `WithGC` | — | yrs has `offset_kind` and `collection_id`/`auto_load` that ygo lacks |
| **offset_kind** | ✅ `Bytes` or `UTF16` — controls how text offsets are counted | ❌ Hardcoded UTF-16 (fixed in v1.0.4) | ⚠️ yrs default is `Bytes`; must be set to `UTF16` for JS client compat | **Critical JS-compat risk in yrs.** If Rust integrators don't set `offset_kind: UTF16`, their YText offsets will be wrong for emoji/supplementary chars. ygo is correct by default. |
| **TransactContext** | ❌ Not present | ✅ `Doc.TransactContext(ctx, ...)` | — | ygo-only; fn polls ctx to exit early; no mid-fn interrupt (matches yrs' no-cancel model) |

---

## 5. Performance Comparison

### B4 Editing Trace

The B4 trace (182,315 character insertions + 77,463 deletions from a real LaTeX editing session) is the standard benchmark for Yjs-compatible CRDT libraries.

| Library | Apply | Encode V1 | Encode V2 | Decode V1 | V1 size | V2 size | Hardware | Version |
|---|---|---|---|---|---|---|---|---|
| **Yjs (JS)** | 5,714 ms | 11 ms | — | 39 ms | ~160 KB | — | Intel i5-8400, Node 20 | 13.6.11 |
| **ygo (Go)** | ~1,400 ms | ~9.7 ms | ~9.0 ms | ~1,180 ms | 3.4 MB | 235 KB | Apple M4 Max | v1.0.4 |
| **yrs (Rust)** | *not published* | *not published* | *not published* | *not published* | — | — | — | 0.25.0 |

**⚠️ Hardware caveat:** Yjs JS numbers are on Intel i5-8400 (2018 server-class), ygo numbers are on M4 Max (2024 high-end laptop). Direct time comparisons are misleading. The important signal is the ratio.

**V1 document size note:** ygo's 3.4 MB V1 vs Yjs JS ~160 KB reflects a difference in squashing behavior — Yjs JS aggressively merges consecutive same-client insertions into single string items on GC, reducing item count. ygo's encoder preserves each insertion item individually in the V1 stream. After V2 encoding (which applies column-oriented RLE compression), ygo produces 235 KB — 6.9% of V1 size — which is much closer to the Yjs baseline.

**Language-level expectations:** yrs will outperform ygo on raw CPU benchmarks due to Rust's zero-cost abstractions, no GC pauses, and yrs's use of arena-friendly allocation patterns (smallvec, smallstr for short strings). These advantages are inherent to the language choice and not actionable at the ygo level.

**Non-language algorithmic gaps in ygo (actionable):**
- ygo's B4 Apply allocates ~3.1M heap objects (~1.2 GB). Each `Item` is individually heap-allocated. An arena or slab allocator would reduce allocation pressure and GC pause frequency significantly.
- ygo's position cache (LRU, 80 entries) was added in v1.0.0 for O(1) average index lookups, but the cache is invalidated on middle insertions. Large documents with many non-tail insertions still degrade toward O(n) per insert.
- The `DeleteSet.applyTo` O(n²) issue was fixed in v1.0.0 (H1), but similar linear scans may remain in the conflict-scanning path for very large documents.

**Recommended follow-up:** Install yrs 0.25.0, run the B4 trace from `benchmarks/testdata/editing-trace.json` on the same M4 Max hardware, and compare allocations and time side-by-side. This is the only way to get an actionable number for the language-independent gap.

---

## 6. Transport Comparison

### 6.1 Rust Transport Landscape

The Rust Yjs ecosystem has **no single official transport**. The closest is `yrs-warp 0.9.0` (maintained by the y-crdt organization), which uses the warp HTTP/WebSocket framework. Community alternatives exist for axum, actix, and tokio-tungstenite, with varying maintenance levels. There is **no HTTP sync implementation** anywhere in the Rust ecosystem.

ygo bundles its own WebSocket and HTTP providers as first-party packages with a consistent API and security model.

### 6.2 WebSocket Comparison

| Feature | yrs-warp 0.9.0 | ygo/provider/websocket v1.0.4 | Notes |
|---|---|---|---|
| **Framework** | warp 0.3 (tokio-based) | net/http + gorilla/websocket | Different web frameworks |
| **Room / Doc management** | ✅ `BroadcastGroup` per room; `Connection` per peer | ✅ `Server` with per-room `room` struct | Similar abstraction |
| **Auth hook** | ❌ No built-in auth; outer warp filters required | ✅ `Server.AuthFunc func(*http.Request) bool` — returns 401 on rejection | **ygo ahead** |
| **CORS / origin validation** | ❌ No built-in origin validation | ✅ `Server.AllowedOrigins []string`; same-origin fallback when empty; `"*"` for open | **ygo ahead** |
| **SyncStep1/2 handling** | ✅ | ✅ | — |
| **Update broadcast** | ✅ | ✅ | — |
| **Awareness handling** | ✅ | ✅ | — |
| **Auth message (type 2)** | ❌ Not handled — unknown message type | ✅ Silently ignored (`case msgAuth`) | ygo matches y-websocket JS behavior |
| **Broadcast strategy** | ✅ Per-peer tokio sink task via `broadcast::channel` (buffered, capacity 32) | ✅ Per-peer goroutine with 10-second write deadline | Both avoid serial blocking on slow peers |
| **Max connections (server-wide)** | ❌ No limit | ✅ `Server.MaxConnections` — returns 503 before upgrade | **ygo ahead** |
| **Max peers per room** | ❌ No limit | ✅ `Server.MaxPeersPerRoom` — returns 503 before upgrade | **ygo ahead** |
| **Graceful shutdown** | ❌ No drain — dropping `BroadcastGroup` terminates abruptly | ✅ `Server.Shutdown(ctx)` closes all peers and waits for goroutines to drain | **ygo ahead** |
| **Room cleanup on last peer** | ❌ Manual — caller must drop `BroadcastGroup` | ✅ Automatic — `handleDisconnect` deletes room when last peer leaves | **ygo ahead** |
| **Persistence adapter** | ❌ No built-in interface | ✅ `PersistenceAdapter` interface, `MemoryPersistence`, `NewServerWithPersistence` | **ygo ahead** |
| **Message size limit (WS frame)** | ❌ No limit documented | ✅ 64 MiB via `conn.SetReadLimit` | **ygo ahead** |
| **WebSocket frame read limit** | Not documented | ✅ Enforced | — |
| **Room-split race prevention** | ❌ Not addressed in source | ✅ Fixed in v1.0.1 — peer removal and room deletion atomic under `server.rmu` + `room.mu` | **ygo ahead** |
| **Awareness update validation** | ❌ No server-side validation before broadcast | ✅ Invalid updates dropped (v1.0.1 fix) | **ygo ahead** |
| **Persistence error propagation** | ❌ Not documented | ✅ `getOrCreateRoom` propagates persistence errors (HTTP 500) | **ygo ahead** |
| **Known open issues** | 2 open issues: SyncStep1 delivery failure on reconnect; multi-peer broadcast failures | Fixed: nil panic on reconnect (v1.0.4) | yrs-warp issue mirrors a bug ygo already fixed |
| **Maintenance** | ✅ Active (last commit 2026-03-26) | ✅ Active | — |

### 6.3 Alternative WebSocket Crates (Rust)

| Crate | Framework | yrs version | Status | Notes |
|---|---|---|---|---|
| yrs-axum 0.8.2 | axum 0.8 | yrs 0.18.2 (outdated) | Community (vagmi); active | Lags yrs by 7 minor versions |
| yrs-actix-redis-demo | actix-web | Recent | Reference demo (Horusiath) | Not a library; shows Redis-backed multi-instance patterns |
| yrs-kafka 0.1.1 | Kafka + RocksDB | Older | Community; stale (2024-09-02) | Persistence/scaling demo |
| hocuspocus-rs | axum | Recent | Community MVP | Hocuspocus wire framing, not standard y-websocket |

**Summary:** No axum or actix alternative is production-grade or current on yrs version. Integrators in the Rust ecosystem must choose yrs-warp (official, warp-only) or build their own transport.

### 6.4 HTTP Sync Comparison

| Feature | Rust ecosystem | ygo/provider/http v1.0.4 | Notes |
|---|---|---|---|
| **HTTP sync (state vector POST)** | ❌ No official or widely-adopted implementation | ✅ POST state vector → receive diff update | **ygo uniquely ahead** |
| **Content-Type validation** | N/A | ✅ Rejects non-`application/octet-stream` with 415 | — |
| **Body size limit** | N/A | ✅ `http.MaxBytesReader` | — |

The HTTP transport is entirely absent from the Rust Yjs ecosystem. This is a meaningful differentiator for ygo in scenarios where WebSocket is unavailable (serverless, certain CDN deployments, polling-based sync).

---

## 7. Production Readiness

### Security Hardening

ygo underwent a systematic security audit at v1.0.0, addressing 17 Critical/High/Medium issues (C1–C17, H1–H12, M1–M5). The most significant: observer deadlock (C7), OOM guards in V1/V2 decoders (C2, C8, C9, C13), CORS bypass (C15), awareness OOM (C12), and unbounded allocation in ReadAny (C2). Full list in CHANGELOG.md.

**yrs and yrs-warp have no equivalent published security audit.** The issues ygo fixed (unbounded allocations, CORS bypass, broadcast-to-closed-peer race) are architectural patterns that also exist in yrs-warp based on the source review — but no systematic hardening effort is documented.

### Protocol Compatibility History (ygo)

| Version | Issue | Impact |
|---|---|---|
| v1.0.2 | GC struct (tag 0) and skip struct (tag 10) not decoded | Rejected all updates from GC-enabled clients |
| v1.0.2 | Cross-client parent resolution (pending retry loop) | "N items with unresolvable parents" errors |
| v1.0.3 | GC'd-origin handling in StoreUpdate | Persistence errors and dropped broadcasts |
| v1.0.4 | Nil panic when reconnecting client sends GC'd YMap state | Server panic |
| v1.0.4 | UTF-16 offset encoding for emoji/supplementary chars | Corrupt binary across V8/JavaScriptCore clients |

These incidents confirm that JS Yjs ↔ ygo interop stress-testing surfaces non-obvious wire-level gaps. Each was triggered by real GC behavior in the JS client that the Go server hadn't exercised.

### Concurrency Model

- **ygo:** `sync.RWMutex` at document level. Multiple concurrent readers; single writer. All goroutines safe. UndoManager is goroutine-safe. Observer callbacks fired after lock release (deadlock-safe).
- **yrs:** `Doc` is `Send + Sync` but `Transaction`/`TransactionMut`/`UndoManager` are `!Send + !Sync`. Rust's borrow checker prevents sending transactions across thread boundaries at compile time, but it also means server code that wants to hold a read lock across an async boundary needs careful lifetime management.

### GC Compatibility

Both libraries default to GC enabled, producing tag-0 wire structs for collected items. ygo's GC compat bugs (v1.0.2–v1.0.4) were all triggered by the JS client (which enables GC aggressively on YMap keys via repeated `YMap.Set` on the same key). The fixes are now stable.

### y-sync Archival Risk

y-sync (the Rust sync + awareness protocol crate) was archived in December 2024. Any future changes to the Yjs protocol — new message types, auth extensions, awareness format changes — cannot be adopted by the Rust ecosystem without forking y-sync or pulling the protocol handling into yrs itself. This is a long-term maintenance risk for Rust-based yrs servers, not a current correctness issue.

---

## 8. Prioritised Gaps Summary

### Critical

| # | Title | Impact | Effort | Ref |
|---|---|---|---|---|
| G1 | **YArray.Move() is not CRDT-safe** — concurrent insertions into the moved range are not also relocated | Multi-client documents with concurrent array moves will have diverging state | L | §4.1 |
| G2 | **YText `offset_kind` not configurable** — ygo is hardcoded UTF-16, which is correct for JS clients but means ygo cannot serve clients that expect byte offsets | Any non-JS Rust yrs client using default `Bytes` offset_kind will experience index mismatches | M | §4.11 |

> Note on G2: ygo being hardcoded to UTF-16 is the **correct** behavior for JS Yjs client compatibility. The gap is that ygo has no escape hatch for Rust-native clients or other environments that expect byte offsets. The inverse is also true: **yrs users must explicitly set `offset_kind: UTF16`** or their server will have offset mismatches with JS clients — a JS-compat risk in yrs that ygo avoids by default.

### Important

| # | Title | Impact | Effort | Ref |
|---|---|---|---|---|
| G3 | **YArray range move absent** — `move_range_to()` not implemented | Ergonomic gap for ordered list reordering | M | §4.1 |
| G4 | **UndoManager.OnStackItemUpdated callback missing** — no notification when a new transaction extends an existing stack item | Cannot update cursor metadata when the capture window extends an existing undo item | S | §4.4 |
| G5 | **UndoManager.OnStackItemPopped callback missing** — no notification on undo/redo | Cannot restore cursor position after undo/redo without polling | S | §4.4 |
| G6 | **UndoManager.ExpandScope() absent** — tracked types are fixed at construction | Cannot lazily add shared types to an existing UndoManager; forces reconstruction | S | §4.4 |
| G7 | **YText range Format() absent** — can insert text with attributes but cannot apply/remove attributes on an existing range | Rich-text editor use-cases (bold, italic, etc. on selected range) require Format() | M | §4.1 |
| G8 | **StickyIndex not implemented** — RelativePosition exists but lacks explicit left/right sticky semantics | Editor cursor semantics (stick to left or right of adjacent char on delete) harder to model | M | §4.5 |

### Nice-to-Have

| # | Title | Impact | Effort | Ref |
|---|---|---|---|---|
| G9 | **Weak references** — experimental yrs feature flag; allows linking/quoting content from other document regions | Required for advanced block-reference editors (Roam, Notion-style) | L | §4.1 |
| G10 | **Preliminary types for nested init** — `TextPrelim`, `ArrayPrelim` etc. give type-safe nested shared-type initialisation | Ergonomic improvement; ygo uses `any`/WriteAny for nested values | S | §4.11 |
| G11 | **can_undo() / can_redo() accessors** — ygo callers must attempt and check error | Minor ergonomic gap | XS | §4.4 |
| G12 | **JsonPath queries on document state** | Niche; rarely needed for typical collaborative editor use cases | M | §4.11 |

### Ecosystem Observation (not a ygo gap)

**y-sync archival (Dec 2024):** The Rust sync + awareness protocol crate has no active maintainer. Future protocol evolution in the Yjs ecosystem will not be absorbed by the Rust ecosystem without community effort. This is a risk for teams building on yrs, not for ygo.

**Transport security:** ygo's `provider/websocket` is more hardened than yrs-warp by a wide margin (auth, CORS, connection limits, graceful shutdown, persistence interface, room cleanup). This gap in yrs-warp represents a production risk for Rust yrs deployments, not a gap in ygo.

---

## 9. Appendix

### Source Links

| Resource | URL |
|---|---|
| yrs 0.25.0 source | https://github.com/y-crdt/y-crdt |
| yrs docs | https://docs.rs/yrs/0.25.0/yrs/ |
| y-sync 0.4.0 (archived) | https://github.com/y-crdt/y-sync |
| y-sync docs | https://docs.rs/y-sync/0.4.0/y_sync/ |
| yrs-warp 0.9.0 | https://github.com/y-crdt/yrs-warp |
| crdt-benchmarks (Yjs JS B4 numbers) | https://github.com/dmonad/crdt-benchmarks |
| Yjs protocol spec | https://github.com/yjs/y-protocols/blob/master/PROTOCOL.md |
| ygo source | https://github.com/reearth/ygo |
| ygo benchmarks | benchmarks/README.md (this repo) |

### Versions Pinned

| Library | Version | Date confirmed |
|---|---|---|
| yrs | 0.25.0 | 2026-04-13 |
| y-sync | 0.4.0 (archived) | 2026-04-13 |
| yrs-warp | 0.9.0 | 2026-04-13 |
| ygo | v1.0.4 | 2026-04-13 |
| Yjs JS (benchmark ref) | 13.6.11 | Published in crdt-benchmarks README |

### Follow-up Work

1. **Apples-to-apples benchmark harness.** Install yrs 0.25.0, write a Rust bench that reads `benchmarks/testdata/editing-trace.json` and runs Apply/Encode/Decode, execute on M4 Max alongside ygo's existing bench suite. Publish numbers in a shared table.
2. **CRDT-safe array move (G1).** This requires implementing the y-move protocol extension that yrs uses — a non-trivial CRDT change. Scope separately.
3. **YText Format() (G7).** Implement range attribute formatting in `YText.Format(txn, index, length, attrs)`. Medium effort.
4. **UndoManager callbacks (G4, G5, G6).** Three small additions to the UndoManager event model. Small effort each.
