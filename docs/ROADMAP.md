# Development Roadmap

This document tracks the phased implementation plan for ygo. Each phase has a clear goal, exit criteria, and a list of tasks. Phases are designed to build on each other — do not start a phase until its dependencies are complete.

---

## Phase 1 — lib0 Binary Codec

**Package:** `encoding/`
**Depends on:** nothing
**Goal:** Implement the complete lib0 variable-length binary encoding format, which is the foundation for all wire compatibility with Yjs.

### Tasks

- [ ] `Encoder` type with `WriteVarUint`, `WriteVarInt`, `WriteVarString`, `WriteVarBytes`, `WriteFloat32`, `WriteFloat64`, `WriteAny`, `WriteUint8`
- [ ] `Decoder` type with corresponding `Read*` methods
- [ ] Sentinel errors: `ErrUnexpectedEOF`, `ErrOverflow`
- [ ] Unit tests: all primitives round-trip, multi-write sequential reads, Unicode strings, empty byte slices, all `Any` variants
- [ ] Error tests: truncated input returns `ErrUnexpectedEOF`, VarUint > 53 bits returns `ErrOverflow`
- [ ] Fuzz target: `FuzzDecodeVarUint` — arbitrary input must never panic
- [ ] Compatibility test: byte-for-byte match against a lib0-encoded golden fixture

### Exit criteria

All unit tests pass. Fuzz target runs 1 M iterations without panic. Golden fixture matches exactly.

---

## Phase 2 — CRDT Core Engine

**Package:** `crdt/`
**Depends on:** Phase 1
**Goal:** Implement the core data model and the YATA integration algorithm. This is the most critical phase — everything else builds on it.

### Tasks

- [ ] `ID`, `ClientID`, `StateVector` (`id.go`)
- [ ] `Content` interface and all nine content types: `ContentString`, `ContentBinary`, `ContentAny`, `ContentEmbed`, `ContentFormat`, `ContentDeleted`, `ContentType`, `ContentDoc`, `ContentJSON` (`content.go`)
- [ ] `Item` struct with `Integrate(txn, offset)`, `Split(txn, at)`, `Delete(txn)`, `GC()` (`item.go`)
- [ ] YATA integration algorithm in `Item.Integrate` — correct handling of concurrent inserts at the same position, deleted origins, out-of-order delivery
- [ ] `StructStore` — per-client item slices, binary-search ID lookup, state vector computation (`store.go`)
- [ ] `DeleteSet` — range tracking, `IsDeleted`, `Merge`, encode/decode (`delete_set.go`)
- [ ] `Transaction` — batch operations, squash consecutive same-client items, observer lifecycle (`transaction.go`)
- [ ] `Doc` — root object, named root types, `Transact`, `ApplyUpdate`, `EncodeStateAsUpdate`, `EncodeStateVector`, `OnUpdate`, `Destroy` (`doc.go`)

### Tests

- [ ] `TestUnit_Item_Integrate_Sequential`
- [ ] `TestUnit_Item_Integrate_Concurrent_SamePosition`
- [ ] `TestUnit_Item_Integrate_Concurrent_Deterministic` — lower ClientID always placed first
- [ ] `TestUnit_Item_Integrate_DeletedOrigin`
- [ ] `TestUnit_Item_Integrate_OutOfOrder`
- [ ] `TestUnit_Item_Integrate_Idempotent`
- [ ] `TestUnit_StructStore_FindByID`
- [ ] `TestUnit_DeleteSet_IsDeleted`
- [ ] `TestUnit_Transaction_ObserverFiresOnce`
- [ ] `TestInteg_TwoPeer_Convergence` — two docs diverge and merge to identical state

### Exit criteria

All unit tests pass. Two-peer convergence test passes for ≥ 1 000 random operation sequences.

---

## Phase 3a — YArray and YMap

**Package:** `crdt/types/`
**Depends on:** Phase 2
**Goal:** Implement the two most-used shared types backed by the CRDT core.

### Tasks

**YArray**
- [ ] `Insert(txn, index, content ...any)`
- [ ] `Delete(txn, index, length int)`
- [ ] `Get(index int) any`
- [ ] `Len() int`
- [ ] `Slice(start, end int) []any`
- [ ] `ForEach(fn func(int, any))`
- [ ] `Observe` / `ObserveDeep`

**YMap**
- [ ] `Set(txn, key, value)`
- [ ] `Delete(txn, key)`
- [ ] `Get(key) (any, bool)`
- [ ] `Has(key) bool`
- [ ] `Keys() []string`, `Entries() map[string]any`
- [ ] `Observe` / `ObserveDeep`

### Tests

- [ ] `TestUnit_YArray_InsertDelete`
- [ ] `TestUnit_YArray_NestedTypes`
- [ ] `TestUnit_YArray_ObserverBatching`
- [ ] `TestInteg_YArray_ConcurrentInsert_Convergence`
- [ ] `TestUnit_YMap_SetGet`
- [ ] `TestUnit_YMap_Concurrent_LastWriteWins`
- [ ] `TestUnit_YMap_ObserverFires`
- [ ] `TestInteg_YMap_ConcurrentSet_Convergence`

### Exit criteria

All tests pass. Multi-peer convergence verified for both types.

---

## Phase 3b — YText

**Package:** `crdt/types/`
**Depends on:** Phase 3a
**Goal:** Implement collaborative text with rich-text formatting.

### Tasks

- [ ] `Insert(txn, index, text string, attrs ...Attributes)`
- [ ] `Delete(txn, index, length int)`
- [ ] `Format(txn, index, length int, attrs Attributes)`
- [ ] `Len() int`
- [ ] `String() string`
- [ ] `ToDelta() []Delta` — Quill-compatible delta output
- [ ] Run-length item squashing (consecutive same-client characters → single item)
- [ ] `Observe` / `ObserveDeep`

### Tests

- [ ] `TestUnit_YText_InsertDelete`
- [ ] `TestUnit_YText_Format_Bold`
- [ ] `TestUnit_YText_ToDelta_WithFormatting`
- [ ] `TestUnit_YText_RunLengthSquashing` — typing 5 chars creates 1 item not 5
- [ ] `TestUnit_YText_Unicode_Multibyte`
- [ ] `TestInteg_YText_ConcurrentFormat_Convergence`

### Exit criteria

All tests pass. `ToDelta` output matches Yjs reference for `ytext_bold.bin` fixture.

---

## Phase 4 — Update Encoding (V1 and V2)

**Package:** `crdt/`
**Depends on:** Phase 3a, 3b
**Goal:** Implement the full binary update format in both versions. This is the wire-compatibility layer.

### Tasks

- [ ] `EncodeStateAsUpdateV1(doc, sv StateVector) []byte`
- [ ] `ApplyUpdateV1(doc, update []byte, origin any) error`
- [ ] `EncodeStateAsUpdateV2(doc, sv StateVector) []byte`
- [ ] `ApplyUpdateV2(doc, update []byte, origin any) error`
- [ ] `UpdateV1ToV2(v1 []byte) ([]byte, error)`
- [ ] `UpdateV2ToV1(v2 []byte) ([]byte, error)`
- [ ] `MergeUpdatesV1(updates ...[]byte) ([]byte, error)`
- [ ] `DiffUpdateV1(update []byte, sv StateVector) ([]byte, error)`

### Tests

- [ ] `TestUnit_UpdateV1_RoundTrip_EmptyDoc`
- [ ] `TestUnit_UpdateV1_RoundTrip_TextInsert`
- [ ] `TestUnit_UpdateV1_RoundTrip_WithDeletes`
- [ ] `TestUnit_UpdateV2_SmallerThanV1`
- [ ] `TestUnit_V1toV2_Roundtrip`
- [ ] `TestUnit_MergeUpdates_OrderIndependent`
- [ ] `TestUnit_DiffUpdate_OnlyMissing`
- [ ] `TestUnit_ApplyUpdate_Idempotent`
- [ ] `TestUnit_ApplyUpdate_OutOfOrder`
- [ ] `TestCompat_ApplyJSUpdate_YText` — load `ytext_hello.bin`, verify state
- [ ] `TestCompat_ApplyJSUpdate_YTextBold` — load `ytext_bold.bin`, verify delta
- [ ] `TestCompat_ApplyJSUpdate_YArray`
- [ ] `TestCompat_ApplyJSUpdate_YMap`
- [ ] `TestCompat_ApplyJSUpdate_ConcurrentMerge`
- [ ] `FuzzApplyUpdateV1` — never panic on arbitrary input
- [ ] `FuzzApplyUpdateV2`

### Exit criteria

All compatibility tests pass with byte-for-byte correctness. Fuzz runs 1 M iterations without panic. V2 is measurably smaller than V1 for non-trivial documents.

---

## Phase 5 — Sync and Awareness Protocol

**Package:** `sync/`, `awareness/`
**Depends on:** Phase 4
**Goal:** Implement the y-protocols message layer and the ephemeral awareness protocol.

### Sync tasks

- [ ] `EncodeSyncStep1(doc) []byte`
- [ ] `EncodeSyncStep2(doc, step1 []byte) ([]byte, error)`
- [ ] `EncodeUpdate(update []byte) []byte`
- [ ] `ReadSyncMessage(msg []byte) (msgType int, payload []byte, err error)`
- [ ] `ApplySyncMessage(doc, msgType int, payload []byte, origin any) error`

### Awareness tasks

- [ ] `Awareness` type with `SetLocalState`, `GetLocalState`, `GetStates`
- [ ] `ApplyUpdate([]byte) error`
- [ ] `EncodeUpdate(clients ...ClientID) []byte`
- [ ] `OnChange` subscription
- [ ] State expiry after 30 s inactivity

### Tests

- [ ] `TestInteg_Sync_TwoPeerHandshake` — SyncStep1 → SyncStep2 → both docs identical
- [ ] `TestInteg_Sync_IncrementalUpdate`
- [ ] `TestInteg_Sync_ThreePeers_Convergence`
- [ ] `TestUnit_Awareness_SetGet`
- [ ] `TestUnit_Awareness_ConcurrentUpdate_HigherClockWins`
- [ ] `TestUnit_Awareness_Expiry`
- [ ] `FuzzApplySyncMessage` — never panic on arbitrary input

### Exit criteria

Two-peer handshake integration test passes end-to-end. Three-peer convergence passes. Fuzz runs cleanly.

---

## Phase 6 — Snapshots and Garbage Collection

**Package:** `crdt/`
**Depends on:** Phase 4
**Goal:** Point-in-time document history and memory management for long-running documents.

### Tasks

- [ ] `Snapshot` type: `StateVector` + `DeleteSet`
- [ ] `TakeSnapshot(doc) Snapshot`
- [ ] `EncodeSnapshot(Snapshot) []byte`
- [ ] `DecodeSnapshot([]byte) (Snapshot, error)`
- [ ] `RestoreDocument(update []byte, snapshot Snapshot) (*Doc, error)`
- [ ] GC walk in `gc.go`: replace deleted content with `ContentDeleted`, merge adjacent tombstones
- [ ] Respect `doc.GC = false` to preserve history

### Tests

- [ ] `TestUnit_Snapshot_RoundTrip`
- [ ] `TestUnit_RestoreDocument_MatchesSnapshot`
- [ ] `TestUnit_GC_ReducesMemory`
- [ ] `TestUnit_GC_DisabledPreservesContent`
- [ ] `TestUnit_GC_NoBreakLiveReferences`

### Exit criteria

Snapshot round-trip is lossless. GC does not break active linked-list references. `doc.GC = false` keeps all item content.

---

## Phase 3c — XML Types

**Package:** `crdt/types/`
**Depends on:** Phase 3b
**Goal:** XML-compatible shared types for rich document structures.

### Tasks

- [ ] `YXmlFragment` — ordered child nodes, `Insert`, `Delete`, `ToXML`
- [ ] `YXmlElement` — extends YXmlFragment with `NodeName`, `SetAttribute`, `GetAttribute`, `GetAttributes`
- [ ] `YXmlText` — extends YText for text nodes inside XML trees
- [ ] `Observe` / `ObserveDeep` for all three types

### Tests

- [ ] `TestUnit_YXmlFragment_InsertDelete`
- [ ] `TestUnit_YXmlElement_Attributes`
- [ ] `TestUnit_YXmlElement_ToXML`
- [ ] `TestInteg_YXml_ConcurrentEdit_Convergence`

### Exit criteria

`ToXML` output matches Yjs reference. Concurrent XML edits converge.

---

## Phase 7 — WebSocket and HTTP Providers

**Package:** `provider/websocket/`, `provider/http/`
**Depends on:** Phase 5
**Goal:** Production-ready transport handlers that plug into standard `net/http`.

### Tasks

**WebSocket**
- [ ] `Server` type with `ServeHTTP`
- [ ] `Room` — one `Doc` + `Awareness` per named room, peer fan-out
- [ ] `PersistenceAdapter` interface: `LoadDoc(room) ([]byte, error)`, `StoreUpdate(room, []byte) error`
- [ ] In-memory adapter (default)
- [ ] Graceful peer disconnect and awareness cleanup

**HTTP**
- [ ] `GET /doc/{room}?sv=<base64>` — return binary update diff
- [ ] `POST /doc/{room}` — apply binary update body
- [ ] Content-Type: `application/octet-stream`

### Tests

- [ ] `TestInteg_WebSocket_TwoPeerHandshake`
- [ ] `TestInteg_WebSocket_BroadcastToRoom`
- [ ] `TestInteg_WebSocket_PersistenceAdapter`
- [ ] `TestInteg_HTTP_GetDiff`
- [ ] `TestInteg_HTTP_PostUpdate`

### Exit criteria

End-to-end test: two goroutines connect to the WebSocket handler over an in-memory net.Pipe, make concurrent edits, and reach identical final state.

---

## Phase 8 — Benchmarks and Performance

**Depends on:** Phase 7
**Goal:** Establish performance baselines, identify bottlenecks, and ensure no regressions ship.

### Tasks

- [ ] Implement the [B4 editing trace benchmark](https://github.com/dmonad/crdt-benchmarks) (260 k real-world text edits)
- [ ] `BenchmarkB4_Apply` — apply the full trace from scratch
- [ ] `BenchmarkB4_Encode` — encode resulting document as V1
- [ ] `BenchmarkB4_EncodeV2` — encode as V2
- [ ] `BenchmarkB4_Decode` — decode V1 from bytes
- [ ] Register all benchmarks in `make bench` and CI benchmark workflow
- [ ] Profile with `pprof` and resolve top-3 hotspots
- [ ] LRU position cache (80 entries) for O(1) average-case index lookup
- [ ] Document baseline numbers in `benchmarks/README.md`

### Targets

| Benchmark | Target |
|-----------|--------|
| B4 Apply  | < 2 s  |
| B4 Encode V1 | < 200 ms |
| B4 Encode V2 | < 300 ms |
| B4 Decode | < 200 ms |

### Exit criteria

All benchmarks within targets. No benchmark more than 20% slower than the previous release. Results committed to `benchmarks/README.md`.

---

## Dependency chart

```
Phase 1 (encoding)
    └── Phase 2 (crdt core)
            ├── Phase 3a (YArray, YMap)
            │       └── Phase 3b (YText)
            │               └── Phase 3c (XML types)
            └── Phase 4 (update encoding)
                    └── Phase 5 (sync + awareness)
                            └── Phase 7 (providers)
                                    └── Phase 8 (benchmarks)
            └── Phase 6 (snapshots + GC)   [parallel with 5]
```

Phases 3a/3b can be developed in parallel with Phase 4. Phase 3c can follow 3b independently. Phase 6 can run in parallel with Phase 5.
