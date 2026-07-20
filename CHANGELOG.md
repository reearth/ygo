# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [1.35.0] — 2026-07-20

### Fixed

- **Head-of-line blocking in the websocket broadcast writer**
  ([#172](https://github.com/reearth/ygo/issues/172)): `runWriter` held the
  per-peer write mutex (`wmu`) across the blocking `conn.WriteMessage`. Because
  `broadcast()` takes the same `wmu` to enqueue frames for *other* peers, a
  single slow or stalled peer could block the broadcasting peer for up to
  `writeTimeout`, and the write-queue-overflow branch could never be reached
  while a write was in flight. The write path now holds `wmu` only long enough to
  read the `closed` flag, then runs `SetWriteDeadline` + `WriteMessage` without
  it — safe because `runWriter` is the sole goroutine that calls
  `conn.WriteMessage`, and gorilla/websocket permits `conn.Close()` concurrently
  with a write. This also fixes a false-positive slow-peer test that only ever
  passed on its own client read-deadline expiring.

### Added

- **`SlowPeerResync` policy for graceful slow-peer recovery**
  ([#172](https://github.com/reearth/ygo/issues/172)): new
  `provider/websocket.Server.SlowPeerPolicy` field of type `SlowPeerPolicy`.
  `SlowPeerDisconnect` (default) is unchanged — it closes a peer whose bounded
  broadcast queue (`PeerWriteQueueSize`) overflows, forcing reconnect-and-resync.
  `SlowPeerResync` instead keeps the connection open: it drops the now-stale
  delta, flags the peer, and has `runWriter` send a full-state `SyncStep2` (plus
  current awareness) once the write queue drains, so a transiently-slow peer (a
  brief GC pause, a busy tab, a burst of large updates) converges in place
  without reconnect churn. Set it before the server starts serving.

### Changed

- **Default `PeerWriteQueueSize` bumped 256 → 512**: a larger per-peer broadcast
  queue gives transiently-slow peers more slack before overflowing (matching the
  yrs shared broadcast ring of 512), so the slow-peer path is hit less often;
  the `SlowPeerResync` policy then handles the overflows that still occur.
  Callers can still override via `Server.PeerWriteQueueSize`.

## [1.34.0] — 2026-07-18

### Added

- **On-device editing for the mobile bindings**
  ([#118](https://github.com/reearth/ygo/issues/118)): `mobile.Doc` gains
  gomobile-safe mutators — `InsertText`, `InsertTextWithAttributes`, `DeleteText`,
  `FormatText`, `InsertArray`, `DeleteArray`, `SetMap`, `DeleteMapKey` — each
  validated and transaction-wrapped, returning an error (never panicking) on bad
  input. A Swift/Kotlin app is now a full editor, not just a viewer.
- **Push change-notifications for the mobile bindings**
  ([#119](https://github.com/reearth/ygo/issues/119)): `Doc.Observe` delivers the
  V1 update bytes plus a `local` flag after each committed transaction;
  `Awareness.Observe` delivers `{added,updated,removed}` client-id sets. Delivery
  is on a background goroutine; `Subscription.Close()` unsubscribes and all
  observers detach on `Doc`/`Awareness` `Close`.
- **`Awareness.PurgeTombstones(grace)` reclaims aged removal tombstones**
  ([#166](https://github.com/reearth/ygo/issues/166)). When a client is removed
  or expires, its entry is kept as a null-state tombstone so its clock can still
  encode removals and reject stale re-adds. These tombstones were never
  reclaimed: because they count toward `SetMaxClients` (deliberately, to bound a
  peer inventing null-state client IDs), a high-churn room's entry count grew
  monotonically and could eventually **refuse new, legitimate clients** — and
  every full `EncodeUpdate(nil)` kept re-broadcasting them. `PurgeTombstones`
  drops tombstones older than `grace` (a non-positive `grace` is a no-op; the
  local client is never purged; no observer event fires). `StartAutoExpiry` now
  runs it automatically each tick as a second stage — `RemoveExpired(timeout)`
  then `PurgeTombstones(2*timeout)` — so `grace` outlives normal update
  reordering before a tombstone's clock is forgotten. Callers driving expiry
  manually should pair `RemoveExpired` with `PurgeTombstones`.

### Changed

- **Idiomatic Yjs JSON from the mobile read accessors**
  ([#109](https://github.com/reearth/ygo/issues/109)): `Doc.GetTextJSON` now emits
  idiomatic single-op Yjs delta (`[{"insert":"hi","attributes":{...}}]`) and
  `Awareness.StatesJSON` emits `{"<clientID>": <state>}` without the internal
  clock. These reshape two mobile read accessors whose output was pre-stable
  (`GetTextJSON` was explicitly documented as unstable); no core-library change.

## [1.33.0] — 2026-07-18

### Fixed

- **XML wire conformance with yjs (`Y.XmlFragment`/`Y.XmlElement`/`Y.XmlText`).**
  Three fixes make ygo's XML types byte-conformant with yjs@13.6 over both V1
  and V2 updates (the y-prosemirror shapes: element attributes, nested
  elements, text marks, concurrent merges):
  - Detached XML subtrees and detached `YText` now **buffer** mutations
    (Yjs `_prelimContent`/`_prelimAttrs`/`_pending` parity) and materialise
    items only when the subtree attaches, top-down — children first, then
    attributes. Building a tree bottom-up previously assigned child clocks
    below the container's, emitting same-client forward parent references
    that crash `Y.applyUpdate` in yjs (`findIndexSS` TypeError) and that
    ygo itself parked forever on re-apply (fragment emptied). A detached
    fragment/element **reflects its buffered prelim content** to readers
    (`Len`, `Children`, attribute getters, `ToXML`), matching yjs's
    `_prelimContent`-aware `length`/`toArray()` — so `Insert(txn, Len(),
    node)` appends rather than prepending, and iteration sees what was
    inserted before attach. Nested `YXmlText` content stays opaque until
    attach (`Len` 0, empty `ToString`), mirroring yjs. (Reflecting prelim
    *attributes* on read is intentionally more complete than yjs, which
    hides `_prelimAttrs` until integrate; ygo keeps detached reads
    consistent with the attached view. Wire bytes are unaffected either way.)
  - Non-string XML attribute values (e.g. a ProseMirror heading's
    `level=1`, a number) are no longer dropped by the attribute getters.
  - The V2 encoder no longer deduplicates keys, matching the yjs reference
    encoder exactly (yjs ships `writeKey` with dedup deliberately disabled;
    older yjs clients cannot read deduped updates). The decoder still
    accepts both shapes.

### Added

- **Typed XML attribute accessors** — `YXmlElement.SetAttributeValue`,
  `GetAttributeValue`, `GetAttributeValues` preserve the attribute's wire
  value type end-to-end. The string-typed `GetAttribute`/`GetAttributes`
  now render non-string scalars best-effort instead of dropping them.
- **XML JS-fixture conformance suite** — `crdt/yxml_yjs_conformance_test.go`
  applies genuine yjs@13.6 update bytes (decode, byte-identical re-encode in
  V1 and V2, byte-identical authoring against pinned clientIDs,
  diff-vs-state-vector, concurrent merge in both orders), mirroring the
  existing YMap/YText conformance suites.

### Changed

- **V2 updates can be larger for XML-heavy documents**: with key dedup
  disabled on encode (yjs parity, see above), every repeated key —
  `ContentFormat` keys like `strong`, repeated element node names like
  `paragraph` — is re-emitted rather than back-referenced. This is the size
  cost of byte-compatibility with the yjs reference encoder.

## [1.32.0] — 2026-07-17

### Added

- **Transaction-scoped root accessors** — `Transaction.GetText`, `GetMap`,
  `GetArray`, `GetXmlFragment` resolve (and create on first use) root types from
  inside a `Transact` callback without re-locking, reusing the lock the
  transaction already holds. Previously the natural `doc.GetText(...)` call
  inside a callback self-deadlocked the non-reentrant document lock, silently
  and permanently ([#138](https://github.com/reearth/ygo/issues/138)). The
  accessors are valid only inside the callback that received the transaction and
  **panic once the transaction has committed** (e.g. from an
  `OnAfterTransaction` observer or a retained `*Transaction`) instead of
  silently touching document state without the lock.
- **Randomized convergence fuzz framework
  ([#70](https://github.com/reearth/ygo/issues/70)).** `testutil/fuzz`
  generates seed-reproducible multi-peer scenarios (random ops across
  Text/Array/Map/XML, nested containers, delivery via Apply/Merge/Diff on V1 and
  V2, plus GC), asserts all ygo peers converge, and — when Node + `yjs` are
  present — checks ygo against real Yjs both logically and by round-trip
  (`TestFuzzCrossImpl`). Failures print a seed (`FUZZ_SEED=<n>` to replay) and
  shrink to a minimal scenario (`fuzz.Shrink`); a regression corpus
  (`testutil/fuzz/corpus`) locks in known cases. CI runs a fixed 1,000-seed set;
  `FUZZ_ITER=50000` drives heavier local soak runs. This framework found the
  YArray.Push (#158) and YText tombstone-anchoring + cache-invalidation (#160)
  divergences fixed in v1.31.5 and v1.31.6.
- **Cross-language Yjs conformance is now enforced in CI
  ([#99](https://github.com/reearth/ygo/issues/99)).** Cross-impl tests hard-fail
  (no silent skip) under `YGO_REQUIRE_NODE=1`; a symmetric Map/Array/Text/XML
  fixture matrix (V1+V2) decodes genuine `yjs@13.6.30` bytes node-free; and a
  fixture-drift CI job regenerates from the pinned yjs and fails on divergence.

### Changed

- **Documented the locking contract honestly** on `Transact` and the
  Doc-level accessors: anything that takes the document lock —
  `Doc.GetText`/`GetMap`/`GetArray`/`GetXmlFragment`, `Doc.Load`,
  observer registration, and nested `Transact` on the same `Doc` — still
  deadlocks silently when called inside a `Transact` callback (Go exposes
  no goroutine identity with which to detect it). Use the transaction
  accessors or resolve handles before the transaction.

## [1.31.6] — 2026-07-13

CRDT convergence + correctness patch. No API changes. Found by ygo's
cross-implementation convergence fuzzer (#70), which compares ygo's public API
against live Yjs. Three related, tombstone-triggered fixes
([#160](https://github.com/reearth/ygo/issues/160)).

### Fixed

- **`YText.Insert` anchored a run before an adjacent tombstone, diverging from
  Yjs (the text-path mirror of #158).** `Insert`/`InsertEmbed` anchored a new
  run relative to the last **live** item, leaving `originRight` pointing at an
  adjacent tombstone, so the run landed *before* it. Yjs's text path advances
  its cursor past deleted items before anchoring, landing *after* them; two
  peers inserting next to the same tombstone therefore ordered their runs
  differently. `Insert`/`InsertEmbed` now advance the anchor past adjacent
  deleted items. (Only text was affected — Yjs's array path doesn't skip
  tombstones, so arrays/maps/XML already converged.)
- **Stale `firstLiveCache` after inserting past leading tombstones.** Inserting
  a live run after leading tombstones made it the first live item without it
  becoming the list head, so the only insert-time `firstLiveCache` invalidation
  (the new-head branch) never fired. A later `Delete` then walked from a stale
  cached head and tombstoned the wrong indices (it could silently no-op).
  `integrate` now also invalidates when a live item lands after a deleted left
  neighbour. (Exposed by the fix above.)
- **Stale `posCache` after a remote delete-only apply (pre-existing).** Remote
  applies run with `Local==true`, so `item.delete`'s position-cache
  invalidation was skipped and the remote delete-set path never cleared it. A
  remote update that only tombstoned an item left cached `(index → item)`
  entries for still-live items after it with an index that was too high, so the
  next local positioned insert resolved the wrong neighbour. The remote
  delete-set path now invalidates the position cache after deleting a countable
  item.

## [1.31.5] — 2026-07-13

CRDT convergence patch. No API changes. Found by ygo's cross-implementation
convergence fuzzer (#70), which compares ygo's public API against live Yjs.

### Fixed

- **`YArray.Push` diverged from Yjs's `push` when the array's tail was a
  tombstone ([#158](https://github.com/reearth/ygo/issues/158)).** `Push`
  delegated to `Insert(Len())`, which anchors the new element after the last
  **live** element (the live-index walk skips tombstones). Yjs's `push`
  (`typeListPushGenerics`) anchors after the last **physical** item, tombstones
  included. When the tail was a deleted element the two produced different YATA
  anchors (`origin=nil, rightOrigin=tombstone` vs `origin=tombstone,
  rightOrigin=null`), so a ygo peer and a Yjs peer that both pushed concurrently
  onto such an array ordered the merged result differently. `Push` now appends
  after the physical tail, matching Yjs. `Insert` at an explicit index is
  unchanged — it already matched Yjs's `insert`.

## [1.31.4] — 2026-07-13

CRDT convergence patch. No API changes. Found by the randomized convergence
fuzzer (#70) once nested containers were exercised, and confirmed to converge
in real Yjs where ygo diverged.

### Fixed

- **A deleted-and-GC'd container's orphaned attribute could be mis-grafted onto
  an unrelated root map, diverging peers
  ([#156](https://github.com/reearth/ygo/issues/156)).** When an XML element (or
  other nested container) is deleted, auto-GC (default `gc:true`) replaces its
  `ContentType` with a `ContentDeleted` tombstone, so a concurrent attribute
  write on that element arrives as a keyed item whose parent can no longer be
  resolved. A store-wide fallback (`findParentForMapEntry`) then attached that
  orphan to the **first** map-type parent it happened to find while scanning the
  store — a choice that depends on integration order and Go map iteration, so
  two peers that saw the same updates in different orders ended up with a
  spurious key (e.g. `"id"`) on the root map on one side only. The fallback is
  removed at all three resolve sites (V1 within-update, V1 doc-level drain, and
  V2); such orphans now drop on every peer, matching Yjs, which integrates an
  item with an unresolved parent as a no-op. The divergence was pre-existing and
  had been masked by the pre-#154 hard abort; it hit 3 of the first 1000 fuzzer
  seeds, and all 1000 now converge.

## [1.31.3] — 2026-07-13

CRDT convergence patch. No API changes. Found by the new randomized convergence
fuzzer (#70) once nested containers were exercised, and confirmed against real
Yjs.

### Fixed

- **Deleting a GC'd nested container aborted a concurrent child-merge
  ([#154](https://github.com/reearth/ygo/issues/154)).** When a nested container
  (e.g. an XML element) is deleted, auto-GC (default `gc:true`) replaces its
  `ContentType` with a `ContentDeleted` tombstone. A concurrent remote update
  that still referenced that container by parent-ID then failed a
  `ContentType` type-assertion and returned a hard "parent item is not a
  ContentType" error, aborting the **entire** update/merge instead of dropping
  the one orphaned child. All four sites (V1/V2 decode and within-update
  resolve) now treat a non-ContentType parent-by-ID as an orphan
  (`parent = nil`) and continue, matching Yjs. This also reverses the
  overly-strict resolve-pass error introduced in v1.31.2.

## [1.31.2] — 2026-07-13

CRDT convergence patch. No API changes. Both fixes were found by ygo's new
randomized convergence fuzzer (#70) and confirmed by independent harness-free
isolation.

### Fixed

- **`ApplyUpdateV2` mis-positioned an item whose `OriginRight` was a
  not-yet-integrated clock ([#151](https://github.com/reearth/ygo/issues/151)).**
  V2 decodes client groups in descending client order, so a higher-clientID
  item could reach integration before its lower-clientID right neighbour
  existed and land at the wrong position, diverging from `ApplyUpdateV1` for an
  identical logical update. `applyV2Txn` now defers such an item (first-pass
  `OriginRight`-clock guard plus a retry-loop `itemFutureDep` check), matching
  the V1 apply path — the C-2 `rightOrigin`-parking fix (#65/#68) that had never
  been ported to V2.
- **`MergeUpdates`/`DiffUpdate` stripped a real item's origin when its parent
  was outside the input set, on both V1 and V2
  ([#152](https://github.com/reearth/ygo/issues/152)).** The re-encoders cleared
  `Origin`/`OriginRight` whenever the referenced neighbour's `Parent` was nil —
  but a `Parent` is also nil when its anchor lives in the receiver's base rather
  than in the merged update. Stripping detached the item, so the receiver
  integrated it at the type head (reorder/loss). The encoders now strip only
  when the neighbour is a genuine GC orphan (no `Parent` and no
  `Origin`/`OriginRight`/`parentID`); genuine GC-origin handling (#125) is
  preserved.

## [1.31.1] — 2026-07-11

Correctness and performance patch. No API changes. Several of these fixes were
surfaced by ygo's new randomized convergence fuzzer (#70).

### Fixed

- **YMap key silently lost on `ApplyUpdateV1` ([#149](https://github.com/reearth/ygo/issues/149)).**
  When a map-keyed item's origin was authored by a higher-clientID peer, the
  item decoded before its origin's client group and took the V1 deferred-parent
  retry path, which resolved the parent but did not inherit `ParentSub`. The
  item integrated keyless and vanished from the map — a single document's own
  full-state encode could fail to round-trip, dropping a live key
  non-deterministically by arrival order. The V1 within-update resolver now
  inherits `ParentSub` like the sibling sites and the V2 decoder already do.
- **`ApplyUpdateV2` hard-failed on a deferred parent-by-ID child
  ([#146](https://github.com/reearth/ygo/issues/146)).** When a nested
  container's first child was authored by a higher-clientID peer, V2's
  descending client-group order decoded the child before its parent. The V2
  decoder now defers and resolves it (parity with the V1 `#140` fix) instead of
  returning `parent item not found`.
- **V2 struct-level merge/diff dropped a deferred parent-by-ID child.**
  `MergeUpdatesV2`/`DiffUpdateV2` re-encoded such a child as a GC placeholder,
  losing its content and parent link. `encodeItemV2` now excludes `parentID`
  from the GC-orphan path and re-emits the explicit parent-by-ID, and both the
  V1 and V2 resolve passes return a consistent error on a genuinely corrupt
  (non-container) parent-by-ID rather than silently orphaning.
- **`examples/peer-sync` deadlocked ([#138](https://github.com/reearth/ygo/issues/138)).**
  The example called `GetText`/`GetArray`/`GetMap` inside a `Transact` callback
  (a re-entrant document-lock hang); the shared-type handles are now resolved
  before the transaction, matching `examples/http-sync`.

### Performance

- **V2 string-column decode is now O(n), not O(n²).** `ApplyUpdateV2` on
  string-heavy documents drops roughly 6×, on par with `ApplyUpdateV1`.
## [1.31.0] — 2026-07-09

### Added

- **Subdocument lifecycle (#63).** A `Doc` can now embed another `Doc` as a
  subdocument — its own clock space and GUID, nested inside a parent doc's
  `YMap` — mirroring Yjs's subdocuments feature. Embedding uses the existing
  `YMap.Set(txn, key, value)`: pass a `*crdt.Doc` as the value and it is
  wired up as a `ContentDoc`; `YMap.Get` returns the `*crdt.Doc` back out. A
  `Doc` may be embedded only once — embedding the same `*Doc` a second time
  panics with the new `crdt.ErrSubdocAlreadyIntegrated` (create a second
  `Doc` with the same GUID instead).
- **`Doc.GetSubdocs()` / `Doc.GetSubdocGUIDs()`** — the subdocuments
  currently resident on a doc, sorted by GUID. The registry reconciles on
  every committed transaction regardless of whether anything observes via
  `OnSubdocs`.
- **`Doc.OnSubdocs(func(crdt.SubdocsEvent)) func()`** — subscribes to
  subdocument lifecycle changes; returns an unsubscribe closure (same
  pattern as `Observe`). Fires at most once per transaction.
- **`crdt.SubdocsEvent{Added, Removed, Loaded []*Doc}`** — reports docs newly
  embedded, docs detached (their `YMap` entry deleted), and docs that should
  now be synced. Embedding and deleting the same doc within one transaction
  cancels out, so no event fires for a net no-op.
- **`Doc.Load()`** — signals that a subdocument's data should be synced.
  Flips `ShouldLoad()` to `true` and, for a doc already embedded in a
  parent, emits a `Loaded` event on the parent. Idempotent. Must not be
  called from inside a `Transact` closure — like `GetText`, it acquires a
  document lock itself (the parent's, not the callee's).
- **`crdt.WithAutoLoad(bool)`**, **`crdt.WithShouldLoad(bool)`**,
  **`crdt.WithCollectionID(string)`** — new `DocOption`s for subdocument
  configuration, plus matching accessors `Doc.AutoLoad()`, `Doc.ShouldLoad()`,
  and `Doc.CollectionID()`. A doc constructed directly defaults
  `ShouldLoad()` to `true`; a doc materialized from a decoded update starts
  `false` (derived from `autoLoad`, matching Yjs) until `Load()` is called.
- **`crdt.Example_subdocs`** — a runnable godoc example
  (`crdt/example_subdocs_test.go`) showing embed → `OnSubdocs` → `GetSubdocGUIDs`.
- **`ContentDoc` opts round-trip on both wire formats.** A subdocument's
  guid/gc/autoLoad/collectionId opts now survive V1 and V2 encode/decode
  (previously the V2 encoder wrote an empty opts object) and survive
  `MergeUpdatesV1`/`MergeUpdatesV2`. Byte-parity with real Yjs is verified by
  decoding a fixture captured from a genuine `yjs@13.6.30` `Y.Doc` that
  embeds a subdocument.

### Changed

- **`crdt.New()` now defaults a Doc's `guid` to a random uuidv4** (was `""`).
  This is Yjs parity (JS Yjs defaults `guid: uuidv4()`) and needed so every
  locally-created doc has a usable identifier for subdocument embedding. It
  is an observable change to `Doc.GUID()`: code that previously saw `""` from
  a `crdt.New()` doc (without `WithGUID`) now sees a random UUID string. Docs
  constructed with `crdt.WithGUID(...)` are unaffected.

Live cross-peer subdocument sync (a provider recognizing `Added`/`Loaded`
events and syncing the subdocument's contents over the wire) is a separate,
tracked follow-up — see [#142](https://github.com/reearth/ygo/issues/142).

## [1.30.1] — 2026-07-08

### Fixed

- **`MergeUpdatesV1`/`DiffUpdateV1` silently dropped a nested-type child whose
  parent is referenced by item-ID when the container is in a later client group**
  (the deferred parent-by-ID / C-3 case). The struct-level merge decodes structs
  without integrating them, so a deferred child kept `Parent == nil` with only
  `parentID` set; `encodeItem` then (1) matched the GC-orphan guard and re-encoded
  the child as a content-less GC placeholder, and (2) wrote an empty-named root
  type for the parent — either way detaching the child. A 37-byte full-state
  update round-tripped through `MergeUpdatesV1` came back 20 bytes with the child
  reduced to a GC placeholder, so `MergeUpdatesV1(u)` was not equivalent to
  apply-then-encode. Regression from the struct-level merge rewrite (#125),
  present v1.27.0–v1.30.0. Fixed by excluding deferred `parentID` items from the
  GC-orphan guard and re-emitting the explicit parent-by-ID when `Parent` is
  still unresolved. Consumers that reconstruct a document by folding a base
  snapshot plus tail updates through `MergeUpdatesV1` (e.g. loading from
  persistence) no longer lose nested-type children authored by a lower-clientID
  peer. (#140)

## [1.30.0] — 2026-07-03

This release bundles two additive features: read-only WebSocket connections
(#59) and the CRDT attribution API (#56).

### Added

- **`websocket.Server.Authorize`** — a richer alternative to `AuthFunc`:
  `func(*http.Request) (ConnectionConfig, bool)`. The bool accepts/rejects the
  connection (false → 401, as before); the returned `ConnectionConfig` describes
  the accepted connection. The first field is `ReadOnly`. When `Authorize` is set
  it takes precedence over `AuthFunc`; `AuthFunc` is unchanged and still grants
  read-write. This enables Hocuspocus-style read-only connections — public-read
  docs, viewer roles, monitoring connections (#59).
- **`websocket.ConnectionConfig`** — per-connection configuration returned by
  `Authorize`. Currently carries `ReadOnly bool`; extensible for future
  per-connection settings. (#59)
- **`crdt.IDSet`** (yjs-v14 `IdSet`) — a per-client set of item-ID runs
  (`(clock, len)`), with `Add`, `Has`/`HasID`, `Clients`, `Ranges`, `IsEmpty`,
  and `Clone`. Wire codec `EncodeIDSet`/`DecodeIDSet` is byte-compatible with
  published yjs v14's `writeIdSet`/`readIdSet` (pinned `npm:yjs@14.0.0-16`). (#56)
- **`crdt.IDMap`** (yjs-v14 `IdMap`) — `IDSet` plus attribution metadata per
  range; overlapping ranges split and their attributes join on read
  (`Add`, `Has`, `Clients`, `Ranges`, `Slice`, `IsEmpty`). Wire codec
  `EncodeIDMap`/`DecodeIDMap` is byte-compatible with published yjs v14's
  `writeIdMap`/`readIdMap` (same pin). (#56)
- **`crdt.ContentAttribute`** (yjs-v14 `ContentAttribute`) — one `{Name,
  Value}` attribution fact. `NewContentAttribute`/`MustContentAttribute`
  validate `Value` against the lib0 `any` domain so an unsupported value
  (`ErrUnsupportedAttributeValue`) fails at the call site instead of panicking
  inside the encoder. (#56)
- **`crdt.ContentIDs`** / **`crdt.ContentMap`** — the insert/delete pair of
  `IDSet`s or `IDMap`s for one update or doc. `EncodeContentIDs`/`DecodeContentIDs`
  and `EncodeContentMap`/`DecodeContentMap` encode inserts then deletes, following
  yjs-main's `writeContentIds`/`writeContentMap` composition; each half is
  byte-verified against the same yjs v14 pin, but no published yjs v14 rc
  exposes a top-level `ContentMap`/`encodeContentMap` export to pin the
  wrapper itself against (that API exists only on yjs's unreleased `main`). (#56)
- **Builders**: `ContentIDsFromUpdateV1`/`ContentIDsFromUpdateV2` (extract from a
  V1/V2 update without integrating it — the entry point for stamping an incoming
  update), `InsertSetFromDoc`/`DeleteSetFromDoc` (yjs
  `createInsertSetFromStructStore`/`createDeleteSetFromStructStore`),
  `CreateContentMapFromContentIDs` (yjs `createContentMapFromContentIds` — stamp
  insert/delete IDs with attributes). (#56)
- **Set algebra**: `MergeIDSets`/`MergeIDMaps`, `ExcludeIDSet`/`ExcludeIDMap`,
  `IntersectIDSets`/`IntersectIDMaps`, `FilterIDMap`, `IDSetFromIDMap`,
  `IDMapFromIDSet`, plus the `ContentMap`-level wrappers
  `MergeContentMaps`/`ExcludeContentMap`/`IntersectContentMaps`/`FilterContentMap`. (#56)
- **`crdt.Example_attribution`** — a runnable godoc example
  (`crdt/example_attribution_test.go`) showing the full stamp → encode → store
  → decode round trip. (#56)

### Changed

- **Read-only WebSocket peers drop inbound writes.** A peer accepted with
  `ConnectionConfig{ReadOnly: true}` still receives document and awareness
  broadcasts and can request state (its `SyncStep1` is answered with `SyncStep2`)
  and query awareness, but its inbound writes are dropped server-side: sync
  `SyncStep2`/`Update` messages, Hocuspocus `SyncReply` (tag 4), and awareness
  updates are not applied or broadcast. Stateless signals (tags 5/6) are not
  gated by read-only. This is additive — connections authorized via `AuthFunc`
  (or with no auth hook) remain read-write. (#59)

### Security

- **Decode ceilings on all four attribution decoders.** `DecodeIDSet`,
  `DecodeIDMap`, `DecodeContentIDs`, and `DecodeContentMap` bound every
  length-prefixed count (client count, range count, attribute count) against the
  decoder's remaining input before allocating, and `DecodeIDMap` additionally
  caps the per-range attribute pre-allocation (the pointer-slice analogue of the
  `encoding` `maxAnyElements` guard, N-C2), so a crafted/truncated payload cannot
  trigger an oversized allocation ahead of the eventual read failure. Errors are
  wrapped in `ErrAttributionDecode`. (#56)

### Docs

- New README **Attribution** section: the primitive/builder/algebra surface,
  a stamp→encode→store→decode code sample mirroring the godoc example, the
  y-redis (y/hub) positioning, the exact yjs v14 pin this release is verified
  against, the scope boundary between byte-verified `IDSet`/`IDMap` and the
  `ContentMap` wrapper (which follows yjs-main's composition but has no
  published function to pin against yet), and the GC caveat for rendering
  attributed history (retain updates, or create the doc `WithGC(false)`). (#56)

## [1.29.1] — 2026-06-29

### Fixed

- **WebSocket clustering deadlock**: `getOrCreateRoom` invoked the relay's
  `RoomActivated` callback while holding the server's rooms lock (`s.rmu`). A relay
  that replays stream history from within `RoomActivated` by calling `Sink.Inject`
  re-entered `getOrCreateRoom` and blocked on the same non-reentrant mutex,
  deadlocking the goroutine while it still held the lock and wedging the entire
  instance — every subsequent room create or serve blocked. This was reachable in
  normal multi-node operation: the second instance to activate a room whose cluster
  stream already had history ran a catch-up replay at activation time and
  deadlocked. `RoomActivated` now fires after the rooms lock is released and the
  room is published, so a re-entrant `Inject` finds the room and returns instead of
  deadlocking. This mirrors `RoomDeactivated`, which already fired off-lock. (#133)

## [1.29.0] — 2026-06-19

### Added

- **`http.Server.AuthFunc`** — an optional `func(*http.Request) bool` called before
  any document is read or mutated; returning false rejects the request with 401.
  The HTTP provider previously had no auth hook (any caller could GET full state or
  POST arbitrary updates), so this brings it to parity with the WebSocket provider's
  existing `AuthFunc` (security finding S-5, #50).
- **`http.Server.MaxUpdateBytes`** — configurable POST-body cap (bytes); oversize
  bodies are rejected with 413 before being buffered. Zero keeps the existing
  64 MiB default. (#50)
- **`websocket.Server.MessageRateLimit` / `MessageRateBurst`** — optional per-peer
  inbound message rate limit (token bucket, `golang.org/x/time/rate`). Each peer
  gets its own limiter; a peer that exceeds it is **disconnected** rather than
  having the offending message dropped (silently discarding a CRDT update would
  leave the peer permanently diverged). Zero (the default) is unlimited, preserving
  existing behaviour (security finding S-7, #51).

### Changed

- **The HTTP provider now validates room names**, rejecting empty, oversized,
  `.`/`..`, and control-character names with 400. This is a behaviour change:
  names that were previously accepted are now rejected. The rule matches the
  WebSocket provider's and is centralised in a shared internal validator so both
  providers enforce one definition. (#50)
- **`websocket.Server.AllowedOrigins` now supports `*` wildcards.** Each entry
  may contain one or more `*` wildcards, each matching any run of characters
  (e.g. `https://*.netlify.app`, `https://pr-*---web-*.run.app`), in addition to
  exact origins and the bare `*` allow-all. Literal segments are anchored to the
  start and end of the Origin, so a wildcard cannot spoof a different host
  (`https://*.example.com` does not match `https://x.example.com.evil`); a
  *trailing* `*` matches only an optional `:<port>` (`https://app.example.com*`
  matches `https://app.example.com:8443` but not `https://app.example.com.evil`).
  Matching is case-insensitive. Previously only exact matches (and a bare `*`)
  were honored, so wildcard entries silently never matched. (#129)

## [1.28.0] — 2026-06-18

### Added

- **`crdt.CreateDocFromSnapshot(src, snap)`** reconstructs the state a snapshot
  captured into a new, independent `Doc` — the Go equivalent of Yjs JS's
  `createDocFromSnapshot`. Items inserted after the snapshot are excluded and
  post-snapshot deletions are not applied, so a key/element deleted after the
  snapshot reappears in the reconstruction. The returned doc is non-GC, so it can
  be snapshotted or restored from again. (#58)
- **`crdt.ErrSnapshotSourceGCed`** is returned by `CreateDocFromSnapshot`,
  `RestoreDocument`, and `EncodeStateFromSnapshot` when the source doc has GC
  enabled: a GC-enabled doc replaces deleted items' content with length-only
  tombstones at transaction commit, so an item deleted after the snapshot no
  longer carries the content it had at snapshot time and reconstruction cannot be
  faithful. Create the source `WithGC(false)`.

### Changed

- **`RestoreDocument` and `EncodeStateFromSnapshot` now guard against a
  GC-enabled source.** They return `ErrSnapshotSourceGCed` rather than silently
  producing an incomplete document/update when the source had GC enabled
  (previously they returned wrong data with a nil error). `RestoreDocument`
  delegates to `CreateDocFromSnapshot`. Callers already following the documented
  `WithGC(false)` snapshot-history contract are unaffected. (#58)

### Performance

- **`Item.integrate` conflict scan no longer reallocates its conflict-tracking
  map on every conflict-group reset.** It now reuses the map via `clear()`
  (matching Yjs's `conflictingItems.clear()`) instead of allocating a fresh one,
  which had made conflict-scan allocation quadratic in the conflict-group size.
  Under high same-position contention (many peers inserting at the same index)
  this cuts convergence allocations sharply — about −92% allocs, −55% bytes and
  −29% time at 400 concurrent same-position inserts — with no measurable change to
  the common low-contention path (`BenchmarkTwoPeerConvergence` allocations
  unchanged). Closes the last open item (C) of #54; A shipped in v1.16.0 and B was
  measured and intentionally not adopted.
## [1.27.0] — 2026-06-16

### Fixed

- **`YText.Format` no longer strips formatting outside its range.** Re-applying
  or toggling a format over a sub-range of an already-formatted run deleted the
  closing marker that bounded content *after* `index+length`, stripping the
  surrounding run's formatting — most visibly on documents loaded via
  `ApplyUpdate` (un-split runs; cf. yjs#606), but also on freshly-typed text.
  `Format` is now a faithful port of the Yjs JS `formatText` algorithm
  (cursor-based, with `minimizeAttributeChanges` / negated-attribute
  restoration), so it opens markers only where the value changes, deletes only
  in-range overlapping markers, and restores the post-range state. Verified
  against `yjs@13.6.30` across fresh and loaded docs.
- **`UndoManager` undo of a deletion now propagates to peers (CRDT-sound).**
  Undo previously flipped `Deleted = false` in place, which produced no wire
  record: a peer that already had the tombstone never revived the content, and a
  back-sync re-deleted it locally — silently losing the undo. Undo now re-inserts
  a copy of the deleted content as a new item (Yjs `redoItem`), so the
  restoration is a normal insert that converges across peers. Works for YText,
  YArray, and YMap. (Undoing a deletion of items inside a *deleted nested type*
  remains a known gap.)
- **`MergeUpdatesV1` / `DiffUpdateV1` no longer drop non-integrable structs.**
  They applied the input to a temporary document and re-encoded its *integrated*
  state, so any struct whose dependency was missing (it parked in the pending
  queue) was silently dropped — corrupting the common partial-diff case. They now
  merge at the **struct level** (Yjs `mergeUpdates`/`diffUpdate` parity): decode
  every struct without integrating, merge per client (dedup/slice overlaps,
  preserve clock gaps as skip structs), union delete sets, and re-encode — so
  non-integrable structs survive verbatim. Verified against `yjs@13.6.30`.

### Added

- **`crdt.SharedType`** — the previously-unexported interface satisfied by
  `YText` / `YArray` / `YMap` / `YXml*` is now exported, so `NewUndoManager`'s
  scope can be named from outside the package (`crdt.NewUndoManager(doc,
  []crdt.SharedType{txt, arr})`). It was effectively unusable externally before.
  Its methods stay unexported, so only ygo's own types can satisfy it.
- **Struct-level update utilities** (closes #57): `MergeUpdatesV2`,
  `DiffUpdateV2`, and `EncodeStateVectorFromUpdate` / `EncodeStateVectorFromUpdateV2`
  — the V2 columnar equivalents of the merge/diff fixes above, plus extracting a
  state vector directly from an update without integrating it. All verified
  against `yjs@13.6.30`.

### Changed

- **`YText.ToDelta` coalesces adjacent inserts with equal attributes** into a
  single op, matching Yjs JS `toDelta`. Previously each backing Item emitted its
  own op, so a run split across Items (e.g. by a `Format` boundary) produced
  several adjacent ops with identical attributes. Consumers that relied on the
  one-op-per-Item shape will now see fewer, coalesced ops (the rendered text and
  attributes are unchanged).

## [1.26.0] — 2026-06-15

### Security

- **`cmd/ygo-server` is now secure by default.** The server wires no
  authentication of its own, so it now binds `127.0.0.1:1234` (loopback only) by
  default instead of all interfaces. A non-loopback bind still works but logs a
  prominent `SECURITY` warning, since any host that can reach the port could
  otherwise read and modify every document. Front a public deployment with an
  authenticating reverse proxy.

### Fixed

- **Large single fields no longer fail to sync below the message cap (N-12).** A
  single wire field (`VarBytes` / `VarString`) was capped at a fixed 16 MiB even
  though every message layer allows 64 MiB by default — so a document with one
  >16 MiB text node or binary embed was silently rejected inside an
  otherwise-valid message. A field is now bounded by the size of the message that
  carries it (policed by the provider's own `MaxMessageBytes`), removing the
  silent failure without weakening the out-of-memory guard: `ReadVarBytes`
  returns a sub-slice that aliases the buffer, so the buffer length is the real
  bound and a crafted oversized length prefix is still rejected without
  allocating.
- **`RelativePosition` resolution matches Yjs at the end of a type.** A position
  anchored to a root type (a null-item / tname anchor — the form
  `CreateRelativePositionFromIndex` produces for an end-of-type cursor) now
  resolves to the end of the type when `Assoc >= 0` (and to the start when
  `Assoc < 0`), matching `toAbsolutePosition` in the Yjs JS reference. Previously
  it always resolved to index 0, snapping an end-of-document cursor back to the
  start. This is a resolution-semantics fix; the wire format was already aligned
  in v1.23.1 (C-4).

### Added

- **Malformed inbound frames are logged.** The websocket server previously
  dropped unreadable or unappliable messages (bad framing, malformed awareness,
  un-appliable sync / stateless) silently. It now logs each discard at `Debug`
  level with the room and error, so an operator can diagnose why a peer's edits
  never land. The level is `Debug`, not `Warn`, because the rate is
  attacker-controlled — keeping this from becoming a log-flood vector.

### Compatibility

No breaking API changes. The `ygo-server` binary's default `-addr` changes from
`:1234` to `127.0.0.1:1234`; deployments that relied on the previous
all-interfaces default must now pass `-addr :1234` (or a specific address)
explicitly — and will then see the new security warning.

## [1.25.0] — 2026-06-11

### Security

- **Bounded awareness memory growth (DoS hardening).** A peer could exhaust a
  room's memory by sending awareness updates that invent unbounded client IDs —
  including null-state entries, which bypassed the existing per-room byte cap.
  `awareness.Awareness` now supports `SetMaxClients(n)`, a cap on the number of
  distinct tracked client entries (live presence plus removal tombstones); once
  reached, previously-unseen client IDs are dropped while existing clients keep
  updating. The websocket `Server` exposes this as `MaxAwarenessClientsPerRoom`.

### Added

- **Server-side awareness expiry.** `Server.AwarenessExpiry` (when > 0) starts a
  per-room background sweep that reclaims a remote client's presence after the
  configured idle duration — clearing "ghost" presence left by peers that died
  silently (mobile sleep, NAT timeout) without a clean disconnect. The sweep
  goroutine is stopped when the room is evicted.
- **`cmd/ygo-server` flags** `-max-awareness-clients` (default 10000) and
  `-awareness-expiry` (default 30s) wire the two protections on by default for
  the turnkey server.

## [1.24.0] — 2026-06-09

### Added

- **`mobile/` — gomobile bindings for native iOS/Android** (#101). A gomobile-safe
  façade (string/int64/[]byte/error only) over `crdt` + `awareness`: `Doc`
  (apply/encode/diff + read text/map/array JSON) and `Awareness`
  (set/clear/encode/apply/states), each with a `Close()` lifecycle. Pure-Go /
  CGo-free; `golang.org/x/mobile` is a build-time tool, not a dependency. v1 is
  sync + render (on-device editing is a planned follow-up).

## [1.23.1] — 2026-06-09

### Fixed

- **Concurrent `YMap`/attribute writes no longer lose a key by apply order.**
  Last-writer-wins decided the winner from the immediate linked-list right
  neighbour, so an unrelated-key write landing between two same-key writes made
  an item falsely consider itself rightmost; receivers diverged and a cross-sync
  then deleted the key outright. The winner is now the rightmost same-key item in
  YATA order (itself order-independent), with an `itemMap` fast path that keeps
  distinct-key population O(N).
- **Out-of-order updates with a missing `rightOrigin` now park instead of
  diverging.** An item whose right origin referenced a not-yet-integrated client
  was integrated at the wrong position (permanent text divergence) because the
  future-clock check was skipped for root types. It now defers and retries once
  the missing client arrives, matching Yjs `getMissing`.
- **A document no longer fails to decode its own full-state encode.** When a
  lower-clientID peer wrote into a higher-clientID peer's nested type (e.g. an
  XML attribute), the child's parent-by-ID reference decoded before its parent
  and hard-failed `ApplyUpdate` (breaking persistence reload and initial sync).
  Such references are now deferred and resolved once the container integrates,
  mirroring Yjs `pendingStructs`.
- **`RelativePosition` is now wire-compatible with Yjs.** The encoding used the
  wrong type tags (item/tname were `1`/`2` instead of Yjs's `0`/`1`), so every
  shared cursor exchanged with a JS peer mis-decoded or crashed lib0. Tags now
  match Yjs (`0`=item, `1`=tname, `2`=type), the type-anchored variant
  round-trips, and `assoc` is optional on decode. Verified by encoding in Go and
  resolving in `yjs@13.6.30`.
- **`Snapshot` encoding is now wire-compatible with Yjs.** ygo wrote the state
  vector first with per-block length prefixes; Yjs writes the delete set first,
  then the state vector, with none. The layout now matches `Y.encodeSnapshot` /
  `Y.decodeSnapshot`.

Found by an internal architecture review; each is covered by a new
order-independence / convergence regression test (`crdt/convergence_p0_test.go`),
verified against `yjs@13.6.30`.

## [1.23.0] — 2026-06-09

### Added

- **`persistence/sqlite` — pure-Go SQLite `VersionedPersistence` backend** (#98).
  CGo-free (`modernc.org/sqlite`), WAL-mode, with crash-safe two-phase prune and
  full `RunConformance` coverage. Drop-in durable store: `sqlite.Open("data.db")`.
- **`cmd/ygo-server` — ready-to-run WebSocket collaboration server** (#100).
  Flags for address, allowed origins, connection/room limits, max message size,
  optional Redis cluster relay (`-redis`), and SQLite persistence (`-store`),
  with graceful shutdown.

## [1.22.0] — 2026-06-06

Yjs wire-format conformance. Fixes a cluster of cross-language interop bugs in
which ygo decoded — and encoded — certain YMap entries and content types in a
way that round-tripped ygo↔ygo but diverged from genuine `yjs` bytes. Found by
diffing ygo's wire codec against the canonical Yjs source and reproducing each
with genuine `yjs@13.6.30` bytes; verified in **both** directions (ygo decodes
real Yjs output; real Yjs decodes ygo output).

> Released on top of v1.21.0 (`cluster/redis`). The two are independent —
> v1.22.0 touches only `crdt`.

### Fixed

- **Duplicate map keys (last-write-wins) corrupted updates** (#YMap-wire).
  Setting the same key more than once — i.e. overwriting any value, the most
  common map operation — produces a second item that carries an *origin* and so
  no `parentSub` on the wire (Yjs sets the BIT6 "has key" flag but writes no
  string in that case). ygo's decoder read a `parentSub` string on the BIT6
  flag regardless of origin, consuming content bytes and misaligning the stream:
  - **V1**: aborted with `unknown Any tag` / `unexpected end of input` and
    **rejected the whole update** (total data loss).
  - **V2**: silently dropped the overwritten key.
  The decoder now reads `parentSub` only when no origin is present, and inherits
  the key from the origin/left item during integration (first-pass decode *and*
  the within-update pending retry) so the item lands in the parent's key map.
  The encoder is fixed symmetrically: it writes the `parentSub` string only in
  the no-origin case, matching Yjs `Item.write`.
- **Empty-string map keys were dropped** (#YMap-wire). `m.Set("", v)` is valid
  in Yjs but ygo used `""` to mean "no key" (a sequence element). `ParentSub`
  is now `*string` (`nil` = sequence element, `&""` = genuine empty key), so
  empty-keyed entries survive encode/decode in both wire versions.

A follow-on source-level diff of the whole wire encode/decode surface against
the canonical Yjs reference (the same method used to find the YMap bugs) turned
up three more cross-library breaks, all reproduced with genuine `yjs@13.6.30`
bytes and fixed:

- **`YText` embeds didn't interop** (#wire-conformance). Yjs's `writeJSON`
  differs by wire version — V1 writes a JSON-text varstring, V2 writes a
  structured `writeAny`. ygo used `WriteAny` for both, so a Yjs **V1** embed
  (`InsertEmbed`) failed to decode (`unknown Any tag`) and ygo-encoded V1
  embeds were unreadable by Yjs. V1 now uses JSON text (V2 was already correct).
- **Subdocument (`ContentDoc`) `opts` field** (#wire-conformance). Yjs writes
  `guid` then `writeAny(opts)` (always an object). ygo's V1 omitted `opts`
  entirely (decode desynced the struct stream); its V2 wrote `null`, which
  makes genuine Yjs crash on `opts.shouldLoad`. V1 now reads/writes `opts`; V2
  writes `{}`.
- **`YXmlHook` (typeRef 5) desynced the V1 decoder** (#wire-conformance). ygo
  has no hook type, but Yjs writes a `hookName` string after the ref; the V1
  decoder left it unconsumed, corrupting the rest of the update. It now consumes
  the name and degrades to a placeholder (as the V2 decoder already did).
- **Splitting `YText` inside a surrogate pair diverged from Yjs**
  (#wire-conformance). When an index/length lands in the middle of a
  supplementary character (e.g. an emoji, which occupies 2 UTF-16 code units),
  Yjs slices the surrogate pair and replaces each lone half with U+FFFD
  (`"a😀c"`, insert `"X"` @2 → `"a�X�c"`). ygo instead rounded the boundary
  forward to the next whole rune, producing different content and item-clock
  boundaries than a JS peer. A shared `splitUTF16` helper now emits U+FFFD on
  both halves for the split and both encoder tail-slice paths (V1 + V2),
  matching Yjs (verified against `yjs@13.6.30`). Clean (between-character) splits
  are unchanged. Reachable only by indices interior to a surrogate pair, which
  conformant editors never emit — but now exact for fuzzers and hand-built indices.

The audit confirmed **no** divergence in the V1 struct header/framing, info-byte
layout, GC/Skip structs, delete-set body, content ref numbers,
String/Binary/Deleted/Format content, the shared-type ref table (0–6), or the
V2 multi-stream RLE format. Intentionally left as-is (decode-tolerant /
non-issues): ascending-vs-descending client ordering, the `ContentMove` (tag
11) ygo extension, the large-integer float32-vs-float64 `Any` tag (numerically
lossless), and the legacy `ContentJSON` (ref 2) per-value format that modern
Yjs never emits.

### Tests

- `crdt/testdata/ymap_yjs_fixtures.json` — 10 YMap scenarios captured from
  `yjs@13.6.30`, decoded as genuine reference bytes (JS→Go) with ygo→ygo
  re-encode stability checks (30 subtests).
- Go→JS interop (`testutil/verify_go_fixtures.js`) gains `ymap_lww` and
  `ymap_empty_key` fixtures, proving ygo's encoder output decodes correctly in
  real Yjs.
- Two `TestYjsCompat_GCdYMapOrigin` fixtures that had hand-crafted
  *non-conformant* bytes (a `parentSub` string written next to an origin —
  which real Yjs never emits) were rewritten to conformant byte shapes.

### ⚠️ Wire-format / persisted-data note (no code change required)

The public API is unchanged — no recompile or code change is needed. However,
the on-wire encoding of overwritten and empty-keyed map entries changed to match
Yjs. **V1/V2 snapshots persisted by ygo ≤ v1.20.0 that contain an overwritten or
empty map key will not decode correctly on this version.** Live-sync deployments
(peers upgrade together) are unaffected; only stored snapshots with those
specific patterns are. Such data was never readable by real Yjs anyway. If you
have affected snapshots, re-encode them once from a running ≤ v1.20.0 instance
(or simply re-sync). No opt-in legacy decoder is shipped given ygo's small
install base; one can be added later if needed.

## [1.21.0] — 2026-06-06

Production-ready Redis transport for the `cluster.Relay` abstraction shipped in v1.20.0. With this release a multi-process ygo deployment behind a load balancer can share one logical document per room via Redis pub/sub — the canonical Hocuspocus `extension-redis` / y-hub topology, in pure Go.

### Added

- **`cluster/redis` subpackage — Redis-backed `cluster.Relay`** (#62).
  - `redis.New(client *goredis.Client, redis.Config)` returns a relay that satisfies the `cluster.Relay` contract.
  - Per-room pub/sub: `RoomActivated` SUBSCRIBES, `RoomDeactivated` UNSUBSCRIBES — a node only receives traffic for rooms it actually hosts. Calls are reference-counted at the relay layer.
  - **Wire format**: `VarBytes(nodeID) + VarUint(kind) + VarString(room) + VarBytes(data)`. The nodeID is a per-relay 16-byte identifier used to suppress self-delivery (Redis pub/sub mirrors every publish back to the publisher's own subscription; the subscriber drops payloads whose nodeID matches its own). Origin is observer-local and intentionally never serialised, per the `cluster.Relay` package contract.
  - Bounded back-pressure: a configurable `OutboundBuffer` (default 256) decouples `Publish` from the actual Redis `PUBLISH` RPC so the CRDT transaction path never blocks on the network round trip. Publish surfaces a clean `ErrRelayClosed` if the bound start context is cancelled (e.g. `Server.Shutdown`), never hangs.
  - Configurable inbound channel size (`Config.ChannelSize`, default 1024) — go-redis silently drops messages when its inbound channel fills; for CRDT updates this would manifest as silent inter-node divergence, so size for the busiest expected room.
  - Single dispatcher goroutine pattern; subscriber + publisher goroutines exit cleanly on `Close`. Lifecycle (Start/Close) and room-membership (RoomActivated/RoomDeactivated) ops are serialised under one mutex so concurrent calls can never reorder the underlying Redis SUBSCRIBE/UNSUBSCRIBE RPCs.
  - **Delivery contract: fire-and-forget** — Redis pub/sub is at-most-once; a node that subscribes *after* a publish does not receive that publish. The intended deployment pattern pairs the relay with `VersionedPersistence` for catch-up state. Documented explicitly in `docs/CLUSTERING.md`.
  - Echo prevention remains entirely on the provider side (the sentinel pointer-identity guard in `provider/websocket/cluster.go`) — the Redis adapter additionally drops self-deliveries at the transport, so the local node never pays the decode + Inject + observer round trip for its own writes.
  - **Test coverage**: 20+ unit and integration tests against `miniredis` (no docker dependency in CI), exercising nil-client / nil-sink / start-after-close / sink-mismatch error contracts, idempotent Close, cross-node round-trip for both `KindSync` and `KindAwareness`, self-delivery suppression, per-room subscription isolation, RoomDeactivated stops delivery, reference-counted room activation, wire-format encode/decode round-trip with truncated-input safety, deterministic `Publish` back-pressure under buffer-full + ctx-deadline + done-close + startCtx-cancel arms, concurrent-publisher stress, concurrent Activate/Deactivate convergence, and Start/Close/Publish lifecycle stress.
  - **Two-server integration tests** (the issue's acceptance criterion): peer-A connected to `srvA` and peer-B connected to `srvB`, both servers sharing one Redis. Edits on A propagate to B (and vice versa) for both document sync and awareness.

### Dependencies

- `github.com/redis/go-redis/v9` — first-party Redis client.
- `github.com/alicebob/miniredis/v2` (test-only) — in-process Redis for `cluster/redis` tests so CI doesn't need a docker side-car.

### Documentation

- `docs/CLUSTERING.md` gains a dedicated "Redis adapter" section covering setup, config, the fire-and-forget delivery contract, the catch-up-via-persistence pattern for late joiners, and what is intentionally NOT in this adapter (distributed locking / writer election, Redis Streams).

### What's not in this release (tracked separately)

- **Redlock / distributed writer election** for persistence write coordination. Belongs in the persistence layer, not the relay.
- **Redis Streams** as an at-least-once alternative to pub/sub. Worth its own design pass given the consumer-group + last-read-id model is a meaningful architectural shift.
- **Redis cluster mode** (multi-shard pub/sub). go-redis supports it but pub/sub semantics differ; this adapter targets single-node / Sentinel deployments.

## [1.20.0] — 2026-06-01

Horizontal-scale release. Adds two independent building blocks for running ygo
across multiple processes: a **cluster relay** that mirrors document updates
*and* awareness between server nodes, and a **versioned persistence** layer with
history, snapshots, and crash-safe pruning on top of the existing
`PersistenceAdapter` primitive. Both ship with reference implementations and are
purely additive — no breaking changes.

### Added

- **`cluster` package — cross-node relay**. A first-class abstraction for
  sharing one logical document across multiple `websocket.Server` instances,
  carrying **both CRDT document updates and awareness (presence)** — superseding
  the doc-sample clustered-adapter pattern that relayed documents only and
  punted on awareness.
  - `cluster.Relay` interface (`Publish`, `Start`, `RoomActivated`,
    `RoomDeactivated`, `Close`) and `cluster.Sink` interface (`Inject`, `Rooms`,
    `GetAwareness`, `GetDoc`). `*websocket.Server` satisfies `cluster.Sink`
    directly (compile-time asserted).
  - `cluster.Outbound` / `cluster.Inbound` events tagged `KindSync` /
    `KindAwareness`.
  - `cluster.MemRelay` — channel-backed in-process reference implementation
    (`NewMemRelay`, `WithBufferSize`), ideal for tests and single-process
    multi-server simulations.
  - **`(*websocket.Server).AttachRelay(cluster.Relay) error`** — wires
    `doc.OnUpdate` + `awareness.OnChange` per room to `Publish` local changes,
    and injects remote changes via `Inject` (sync → `ApplyUpdateV1` +
    `BroadcastUpdate`; awareness → `ApplyUpdate` + peer fan-out). Started with a
    context cancelled on `Server.Shutdown`, which also closes the relay.
  - **Echo guard**: relay-injected changes are applied with a process-local
    origin sentinel; the per-room observers drop sentinel-origin changes by
    pointer identity (the same trick `Server.Apply` uses), so a change crosses
    the cluster exactly once and never loops. The sentinel never crosses the
    wire.
  - New `Server` accessors backing the `Sink` contract:
    **`GetAwareness(room) (*awareness.Awareness, bool)`** and
    **`Rooms() []string`** (both thread-safe over the room map).
  - See [docs/CLUSTERING.md](docs/CLUSTERING.md).

- **`persistence` package — versioned persistence**. An append-only,
  versioned store keyed by room, layered on the low-level `PersistenceAdapter`
  primitive.
  - `persistence.VersionedPersistence` interface: `Load`, `AppendUpdate`,
    `ListVersions` (newest-first, non-cumulative), `GetUpdate`, `MaterializeAt`
    (point-in-time rebuild via `MergeUpdatesV1`), `CaptureSnapshot` /
    `RestoreSnapshot` (named V1 head blobs), `PruneAfter`, `Compact`, `Delete`.
    Standardised on lib0 **V1** internally.
  - **Crash-safe `PruneAfter`** (snapshot-before-delete): writes a checkpoint
    (`target` ceiling + rolled-back head) *before* deleting newer updates, and
    clamps every read to the checkpoint — a crash mid-prune can never resurrect
    a "future" version on reopen.
  - Reference implementations: `NewMemoryPersistence()` (in-process) and
    `NewFilePersistence(dir)` (directory-backed, atomic temp+rename writes,
    `Reopen()` restart modelling).
  - **Exported conformance suite** `persistence.RunConformance(t, factory)` so
    external adapters (e.g. a GCS store) verify themselves with one call. Covers
    append/list/get, materialise, prune (incl. the **mid-prune crash**
    regression), compact, and snapshot round-trip. Optional
    `CrashInjector` / `Reopener` interfaces unlock the crash-safety subtest;
    both reference impls implement them.
  - **`persistence.LegacyAdapter`** (`NewLegacyAdapter` /
    `NewLegacyAdapterContext`) maps `Load`/`AppendUpdate` onto the provider's
    `LoadDoc`/`StoreUpdate` (and the optional `StoreUpdateContext`), so a
    `VersionedPersistence` plugs straight into
    `websocket.NewServerWithPersistence` — every committed transaction becomes
    one version.
  - See [docs/PERSISTENCE.md](docs/PERSISTENCE.md#versioned-persistence-the-persistence-package).

### Documentation

- New [docs/CLUSTERING.md](docs/CLUSTERING.md) documenting the relay, `MemRelay`,
  `AttachRelay`, the origin-sentinel echo guard, and implementing a custom
  broker-backed `Relay`.
- [docs/PERSISTENCE.md](docs/PERSISTENCE.md) gains a versioned-persistence
  section and now frames `PersistenceAdapter` as the low-level primitive the
  versioned layer builds on; the multi-node section cross-links to the relay for
  cluster-wide awareness.

## [1.19.0] — 2026-05-28

Second Hocuspocus-compatibility release. Adds the application-level extension points that production deployments need on top of v1.18.0's wire-protocol message types: per-room lifecycle hooks on the WebSocket server, and a new optional `provider/webhook` subpackage for forwarding events to external HTTP endpoints.

### Added

- **`provider/websocket` lifecycle hooks** (#60). Four new optional hook fields on `Server`:
  - `OnLoadDocument func(ctx context.Context, room string, doc *crdt.Doc) error` — fires once per room after the persistence adapter has bootstrapped the doc, before any peer interacts. Returning a non-nil error fails room creation and propagates to the caller. **Runs while the server room-map lock is held**, so implementations must return promptly; defer heavy I/O to a goroutine if needed. This mirrors `PersistenceAdapter.LoadDoc` which also runs under the same lock.
  - `OnUnloadDocument func(ctx context.Context, room string)` — fires when a room is evicted from the server map (last-peer-leaves or `CloseRoom`). Runs after all server locks are released; safe to block on I/O.
  - `OnFirstPeer func(ctx context.Context, room string)` — fires on the 0→1 peer transition; useful for warm-up tasks. Runs after locks released. `ctx` is the WebSocket request context.
  - `OnLastPeer func(ctx context.Context, room string)` — fires on the 1→0 peer transition; useful for cool-down tasks. Runs after locks released. Fires before `OnUnloadDocument` when both apply.

  All four hooks are panic-safe: a `recover()` wraps each invocation and logs the panic + stack at `Error` level via the server logger — a misbehaving hook can no longer crash the connection-handling or disposal goroutine.

- **`provider/webhook` subpackage** (#61). New optional package that POSTs ygo events to a configurable HTTP endpoint:
  - `webhook.Config` with `URL`, `Secret`, `Debounce`, `MaxRetries`, `BackoffBase`, `MaxBackoff`, `MaxBodyBytes`, `MaxConcurrentDeliveries`, `HTTPClient`, `Logger`.
  - `webhook.New` / `webhook.Webhook.Enqueue` / `webhook.Webhook.Close`.
  - **`webhook.AttachTo(srv, wh) func()`** convenience that wires every relevant `Server` hook (`OnLoadDocument` → `EventLoad` + per-doc `OnUpdate` → `EventUpdate`; `OnUnloadDocument` → `EventUnload`; `OnFirstPeer` → `EventConnect`; `OnLastPeer` → `EventDisconnect`) in a single call. Returns an idempotent detach func that restores the previous hook values. Composes with any hooks the caller has already set.
  - HMAC-SHA256 request signing on every body, emitted as `X-YGo-Signature-256: sha256=<hex>`. `webhook.VerifySignature` for receivers; constant-time comparison.
  - Debounce / coalescing window (default 1s, capped at 10s) — rapid same-`(Room, Type)` events collapse into a single POST. Different event types for the same room never collapse into each other.
  - Retry with exponential backoff (default 5 attempts, 250ms base, **capped at `MaxBackoff` (default 30s)**) on transport errors and 5xx responses. **±20% jitter** added to each retry sleep to defeat thundering-herd retry alignment when many concurrent webhooks fail against the same receiver. 4xx drops immediately.
  - **Bounded delivery concurrency** via `MaxConcurrentDeliveries` (default 8) — a slow / dead receiver no longer accumulates unbounded delivery goroutines under burst load.
  - **Single dispatcher goroutine** owns the debounce timer (replaces the previous `time.AfterFunc` + `Timer.Reset` pattern that allowed a queued firing to escape `Close`). Both `Close` and the dispatcher cooperate on a single `closed` channel for clean teardown.
  - `webhook.Event` shape with type / room / update bytes (base64 on the wire) / timestamp.
  - `webhook.Close` drains pending events before returning; events enqueued after Close are silently dropped.

### Internal

- `Server.getOrCreateRoom` now takes a `context.Context` so `OnLoadDocument` receives a request-scoped ctx. Internal API change; no public callers affected.

## [1.18.0] — 2026-05-28

First Hocuspocus compatibility release. `provider/websocket` now accepts the seven additional message types Hocuspocus extends y-protocols with, so Hocuspocus-aware clients (Tiptap stateless extensions, custom liveness pings, application close signals) no longer have their frames silently dropped.

### Added

- **Hocuspocus message types 4-10 in `provider/websocket`** (#55):
  - **`SyncReply`** (tag 4) — applied to the local doc and broadcast to other peers, but never echoed back to the sender (breaks the SyncStep1 ping-pong loop on noisy links).
  - **`Stateless`** (tag 5) — surfaced to the application via the new `Server.OnStateless` hook with `IsBroadcast: false`. Not broadcast to other peers.
  - **`BroadcastStateless`** (tag 6) — fanned out to all other peers in the room as plain `Stateless` (tag 5) frames; `OnStateless` fires on the server with `IsBroadcast: true`.
  - **`CLOSE`** (tag 7) — closes the underlying WebSocket connection; logs the optional reason if present.
  - **`SyncStatus`** (tag 8) — server→client ack; silently consumed if a client sends it.
  - **`Ping`** (tag 9) — replied to with a single-byte `Pong` (tag 10) frame.
  - **`Pong`** (tag 10) — silently consumed.

- **`Server.OnStateless StatelessHook`** and **`StatelessInfo` struct** — new public types in `provider/websocket`. The hook is invoked on the peer's read goroutine after any broadcast fan-out has already happened; long-running work should be dispatched to a separate goroutine.

### Framing limitation

Hocuspocus's full client framing prepends a `VarString(docName)` to every frame so a single connection can multiplex multiple documents. ygo's framing remains the y-websocket layout (tag + payload), one document per WebSocket. This release adds the Hocuspocus message **types** on the existing y-websocket framing — so Hocuspocus-aware **handlers** can be used by clients that speak y-websocket, but Hocuspocus's multi-doc multiplex is a separate architectural change not in scope here.

### Tests

- 7 new integration tests in `provider/websocket/hocuspocus_test.go` exercise each new tag end-to-end (real WebSocket peers via `httptest`): `SyncReply` apply+broadcast+no-echo, `Stateless` hook fires + no broadcast, `BroadcastStateless` fan-out as tag 5 + hook fires with `IsBroadcast: true`, `Ping` → `Pong`, `Pong` silently consumed, `CLOSE` closes the connection, `SyncStatus` silently consumed.

## [1.17.0] — 2026-05-27

Wire-framing performance pass. Focused on the encoder allocation churn on the WebSocket send path and the redundant copies on the awareness JSON decode path. No public API breaks; one new helper and one decoder-method rename.

### Added

- **`encoding.GetEncoder` / `encoding.PutEncoder` / `encoding.EncodeBytes`** (#52). A package-level `sync.Pool` for `*Encoder`. `EncodeBytes(fn)` is the recommended wrapper for wire-framing call sites: it gets an encoder from the pool, runs `fn`, copies the resulting bytes into an independent allocation, and returns the encoder to the pool. The returned slice is safe to hand to write channels or other long-lived consumers.

- **`encoding.Decoder.RemainingBytesCopy`** (#53 A). Returns an independently-allocated copy of the unread buffer portion — the previous behaviour of `RemainingBytes`. Use when callers need to retain the bytes across decoder buffer mutations.

### Performance

- **`Decoder.RemainingBytes` is now zero-copy** (#53 A). Returns a sub-slice of the underlying buffer instead of allocating a fresh copy. The documented contract is now: callers must treat the result as read-only and copy if they need a slice with an independent lifetime. The only non-test caller in this repo (`provider/websocket/peer.go:47`) hands the bytes straight to `ApplySyncMessage` and `broadcastSync`, both of which only read or copy.

- **`Awareness.ApplyUpdate` zero-copy JSON decode** (#53 B). The per-entry JSON payload is now held as `[]byte` end-to-end (decoded via `ReadVarBytes`, which already returns a sub-slice of the decoder buffer) and passed directly to `json.Unmarshal`. Pre-fix, the bytes were converted to `string` by `ReadVarString` (one copy) and back to `[]byte` by `json.Unmarshal([]byte(s), ...)` (a second copy). On `BenchmarkApplyUpdate_Many` (100 entries): **-15.97% allocs/op (626 → 526), -9.93% sec/op on `_Single`**.

- **WebSocket send/broadcast paths use the pooled encoder** (#52). All six `encoding.NewEncoder()` call sites in `provider/websocket/peer.go` (`sendSync`, `sendAwareness`, `broadcastSync`, `broadcastAwareness`, `broadcastAwarenessFromRoom`, `encodeAwarenessRemoval`) now go through `EncodeBytes`. `crdt/update.go`'s `encodeV1Locked` and `EncodeStateVectorV1` also switched. The pool keeps the underlying buffer warm across calls, eliminating the growth allocations that occurred on each `WriteVarUint`/`WriteRaw` past the 64-byte initial capacity.

Benchstat n=5 (awareness package geomean): **-7.97% sec/op, -3.58% allocs/op**. Encoding package geomean: -1.81% sec/op, neutral allocs.

### Internal refactor

- `awareness.checkJSONDepth` signature changed from `string` to `[]byte` so it can scan the decoder sub-slice without an intermediate string conversion. Private — no external impact.

## [1.16.0] — 2026-05-26

First post-audit release. Focused exclusively on `crdt/` internal performance — no public API changes.

### Performance

- **`YText.Delete` O(N²) → O(N) for sequential head-deletes** (#86). Two complementary fixes drive the win, both inspired by a cross-reference comparison against Yjs JS and yrs:
  1. **`hasFormatting` gating** (mirrors Yjs's `_hasFormatting` flag). `abstractType` now flips a `hasFormatting` bit the first time a `ContentFormat` item is integrated. `YText.Delete` skips the `cleanupDanglingFormatsInRegion` walk entirely on YText types that have never had `Format()` called — the dominant cost on plain-text head-delete workloads.
  2. **`firstLiveCache` extended to `deleteRange`**. `abstractType` memoises the first live (non-deleted) item from `t.start`; both `deleteRange` and `cleanupDanglingFormatsInRegion` now resume from that pointer instead of re-walking accumulated leading tombstones on every call. The cache advances lazily (tombstoning is monotonic) and is invalidated only when a new item is integrated as the new `t.start`.

  Benchstat n=5 on `BenchmarkYText_Delete`: **-46.77% (2.87ms → 1.53ms per 1000-char delete loop)**. Geomean across the hot-path suite: **-8.09% sec/op**, **0.00% B/op**, **0.00% allocs/op**.

- **`Transaction.changed` pre-sized to 4** (#54 A). Most transactions touch 1-3 types; the zero-hint allocation forced immediate rehashing on the first append.

Two follow-ups from #54 were measured and reverted because they net-regressed other benchmarks:
- Pre-sizing `Transaction.newItems` added one allocation per transaction even when no `ContentString` was inserted, hurting array/map-only workloads more than it helped text.
- Reusing the YATA conflict-scan maps via `clear()` slowed `TwoPeerConvergence` (small maps make `clear()` walk slower than a fresh allocation). Conflict-scan map pooling remains a candidate for a future PR once the cost model justifies it.

### Internal refactor

- `abstractType` gains `firstLiveCache *Item` and `hasFormatting bool`, plus helpers `firstLiveFromStart` and `invalidateFirstLiveCache`. All private — no public API surface change.
- `item.integrate` flips `Parent.hasFormatting = true` whenever the integrated item carries a `ContentFormat`, matching Yjs's `_hasFormatting` once-true-always-true semantics.

### Why not a full Yjs structural port

A cross-reference comparison against Yjs JS revealed that the Yjs `cleanupContextlessFormattingGap` model has different cleanup semantics — it only removes duplicate-key markers in a contiguous gap, not orphan opener/closer pairs whose effect zone has no live content. ygo's existing `cleanupDanglingFormatsInRegion` is intentionally more aggressive and is what closes #71 vector A4. We kept ygo's richer cleanup logic and adopted only Yjs's `_hasFormatting` gating + the `firstLiveCache` optimisation, which together give the YText_Delete win without weakening cleanup semantics.

## [1.15.0] — 2026-05-26

Closes the cross-reference audit (issues #71-#79). Final correctness-focused minor release of the audit cycle.

### Added

- **`YMapEvent.Keys`** (#74 vector D2, MEDIUM). New `map[string]KeyChange` field on `YMapEvent` surfaces per-key change actions (`KeyAdded` / `KeyUpdated` / `KeyDeleted`) plus `OldValue` for updates and deletes. The legacy `KeysChanged` set is still populated for backwards compatibility, but new code should prefer `Keys` for richer event metadata. Mirrors Yjs JS's `YMapEvent.keys`.

- **`YArrayEvent.Delta`** (#74 vector D1, MEDIUM). New `[]Delta` field on `YArrayEvent` carries Quill-style insert / retain / delete operations with values, matching the existing `YTextEvent.Delta` shape. Trailing retains are elided per Quill convention. Pre-fix, array observers received only the `Target` and `Txn` and had to recompute the diff themselves.

### Fixed

- **Auto-GC at transaction commit** (#78 vector H1, MEDIUM). When `WithGC(true)` (the default), the content of items tombstoned during a transaction is now replaced with a length-only `ContentDeleted` placeholder at commit time, rather than waiting for a manual `RunGC` call. Long-running collaborative sessions no longer retain full content for items that have been deleted and will never be observable again. The replacement happens *after* the observer-delta computation, so subscribers still see the original content in the Delete delta. Auto-GC is suppressed while an `UndoManager` is attached so undo / redo can still restore deleted items by flipping the `Deleted` flag.

- **Transient-split re-merge at transaction commit** (#78 vector H2, MEDIUM). When `splitItem` produces a right half and no item is integrated between the two halves before the transaction commits, the halves are reunited. This prevents linked-list fragmentation in long edit sessions where item boundaries get split (e.g. during partial deletions or `ContentMove` resolution) but no foreign insertion ever needed the boundary. Mirrors Yjs JS's `_mergeStructs` / `tryToMergeWithLeft`.

### Internal refactor

- `Transaction` gains a `mergeStructs []*Item` field (private) that `splitItem` appends to and `tryMergeWithLefts` walks at commit. Adds one slice header per transaction; allocation is amortised across splits.
- `Doc` gains a private `undoManagerCount` field, incremented by `NewUndoManager` and decremented by `UndoManager.Destroy`. Used to gate transaction-commit auto-GC.

## [1.14.0] — 2026-05-25

### Fixed

- **`YArray.ToSlice` / `YArray.ToJSON` / `YMap.Entries` / `YMap.ToJSON` recursively unwrap nested shared types** (#75, HIGH). Pre-fix, items wrapping a nested `*YArray`, `*YMap`, `*YText`, or `*YXml*` via `ContentType` were silently dropped from the output — `json.Marshal` of a YArray containing a nested YMap produced an array with the nested map missing entirely. Now: nested YArray → `[]any`, nested YMap → `map[string]any`, nested YText → `string`, nested YXml* → XML string serialisation. Arbitrarily-deep nesting (array → map → array → …) recurses cleanly. Matches Yjs JS's `toJSON` convention.

- **`YTextEvent.Delta` emits insert/delete/retain ops for `ContentEmbed` and `ContentType` items** (#74 vector D3, MEDIUM). Pre-fix, `computeDelta` only handled `ContentString` and `ContentFormat`, so observers received delta events that silently omitted embeds (images, formulas, custom inline objects). Now: embed inserts surface as `Delta{Op: Insert, Insert: embedValue}`, embed deletes as `Delta{Op: Delete, Delete: 1}`, and retains across embeds advance by 1 (matching Yjs's UTF-16 length convention).

### Internal refactor

- Factored `toSliceLocked` / `entriesLocked` / `toStringLocked` helpers in YArray / YMap / YText so the recursive `toJSONValue` (used by #75) can traverse nested types from within a held doc lock without re-entering. No public API change.

### Documentation

- **README refresh.** Bumped the version reference from v1.7.0 to v1.14.0 (five months stale). Extended the post-v1.0 hardening section through v1.14.0: security hardening (v1.8.x), lib0 wire-format parity (v1.8.0/v1.10.0), cross-reference audit (v1.9.0–v1.14.0), sync read-loop resilience (v1.9.0), awareness heartbeat (v1.11.0). Added a callout for the `gaps` label tracking the audit work.

## [1.13.0] — 2026-05-21

### Fixed

- **`YText.Insert` with `currentAttributes` diff** (#71 vectors A2 + A3, HIGH). Closes #71 entirely. Companion to v1.12.0's A1 + A4 fixes.
  - **A2 — inheritance**: when caller passes `nil`/empty attrs, the new text inherits whatever `ContentFormat` markers are in effect at the cursor. `Insert` now computes `currentAttributes` by walking from `txt.start` to the insertion anchor, then uses it to decide whether any new markers are needed (none, in the inheritance case).
  - **A3 — no rightward bleed**: when caller passes explicit attrs, a diff against `currentAttributes` drives marker emission: opening markers for keys that need to change, and negating closing markers after the text to revert to the pre-insert state. Pre-fix, only openers were emitted, so formatting bled rightward through subsequent retained text. Matches Yjs JS `insertText` byte-for-byte.
  - **Incidental fix**: the closer's `Origin` was being set to the first clock of the wrapped text item, not the last. On a fresh peer applying the update via `ApplyUpdateV1`, YATA integration placed the closer mid-text instead of after the full text. Now uses `item.ID.Clock + item.Content.Len() - 1`.

## [1.12.0] — 2026-05-20

### Added

- **`YText.InsertEmbed(txn, index, embed, attrs)`** (#76, HIGH): public API for inserting embedded objects (images, formulas, videos, any inline non-text payload) into the rich-text stream. Each embed counts as one UTF-16 code unit in document length, matching Yjs JS. Optional `attrs` argument wraps the embed in opening + closing ContentFormat markers so attributes apply only to the embed itself, not to subsequent content. The wire format (`ContentEmbed`, tag 5) already supported embeds — this closes the missing public-API gap.
- **`ToDelta` now emits embeds** as their own Delta entries with the embed value carried in `Insert` (rather than a string). Pre-fix, `ContentEmbed` items in the linked list were silently dropped from `ToDelta` output.

### Fixed

- **`YText.Format` cleans up overlapping same-key markers in the target range** (#71 vector A1, HIGH): pre-fix, every call to `Format` inserted opening + closing markers without checking for existing markers in the range. Repeated formatting toggles (bold on/off, applied multiple times to the same range) left dead pairs in the linked list that accumulated without bound. `Format` now walks the range and tombstones any pre-existing `ContentFormat` items whose key is being set or cleared before inserting the new markers. Matches Yjs JS `YText.formatText`.

- **`YText.Delete` cleans up dangling format markers** (#71 vector A4, MEDIUM): after deleting a range, `ContentFormat` markers whose effect zone now contains no live countable content are tombstoned. Two redundancy categories handled: openers with no live content in scope (until the next same-key marker), and closers with no live opener for the same key preceding them. Matches Yjs JS `cleanupFormattingGap`. **Perf note:** the cleanup adds a bounded local walk after each `Delete` call — `BenchmarkYText_Delete` (1000 single-char deletes from position 0) shows a ~53% increase from 1.55µs to 2.39µs per delete, all within sub-µs absolute latency. Tracked for future optimisation; correctness takes priority.

- **`computeDelta` correctly handles markers deleted in the current transaction** (incidental fix surfaced by #71/A1): a pre-existing format marker tombstoned during the transaction updated `oldAttrs` mid-walk, producing a phantom attribute diff on the preceding retain. `flushRetain` is now called before the `oldAttrs` update so the diff is computed against the correct pre-transaction state.

### Deferred to follow-up

- **YText.Insert with attribute inheritance / negation** (#71 vectors A2 + A3): the structural rewrite of `Insert` to compute `currentAttributes` at the cursor and diff against caller-supplied attrs is scoped to a separate PR (#71 stays open with a follow-up note).

## [1.11.1] — 2026-05-20

### Fixed

- **`crdt`: `Item.delete` now cascades into `ContentType` children (#72 vector B1, HIGH)**. Deleting a container item (e.g. a `YArray` entry holding a nested `YMap`) previously tombstoned only the outer item; the nested children stayed live in the store and the delete-set on the wire omitted their clocks. Peers that held the same nested type saw inconsistent state — inner items appeared live after the outer container was deleted. `Item.delete` now walks `ContentType.Type` head-to-tail and recursively tombstones each child, matching Yjs JS `Item.delete` and yrs `Block::delete`. Cascade depth is unbounded (arbitrarily-nested structures fully clean up).

- **`crdt`: `DeleteSet.applyToPartial` splits items at range boundaries before tombstoning (#72 vector B2, HIGH)**. A delete-set entry that only partially overlapped a locally-squashed run previously tombstoned the entire item, wiping content outside the declared range. Cross-peer trigger: one side squashes runs of text the sender saw as multiple items, then receives a partial-range delete-set entry from the sender. `applyToPartial` now calls `getItemCleanStart` at both range boundaries so each affected item lies entirely inside `[r.Clock, r.Clock+r.Len)` before being deleted, matching Yjs JS `iterateDeletedStructs` and yrs `Update::integrate`.

## [1.11.0] — 2026-05-20

### Added

- **`Awareness.Heartbeat()`** (#73 vector C5): re-emits the local client's current state with an incremented clock so peers learn we're still alive even when the state hasn't changed. Designed to pair with `StartAutoExpiry` on the peer side — they expire clients that go quiet; we keep ourselves visible by heartbeating periodically. Observers are not fired (state itself didn't change, only the clock advanced). Matches Yjs JS's constructor interval which re-emits local state every `outdatedTimeout/2`.

### Fixed

- **Awareness: remote peers can no longer wipe local state (#73 vector C1, HIGH)**. A remote `(self.clientID, ?, "null")` entry previously cleared the local awareness state — meaning any peer could deauthenticate any other peer by broadcasting a null for their clientID. `ApplyUpdate` now detects this case, bumps the local clock past the incoming, and re-emits the current local state so peers learn the new clock. Matches yrs `apply_update_internal` (`awareness.rs:414-419`) and Yjs JS `applyAwarenessUpdate`.

- **Awareness: equal-clock null removals are honored for active clients (#73 vector C2, HIGH)**. The strict `e.clock <= current.Clock` gate dropped legitimate "client X has gone offline at the clock you already know" messages, leaving phantom presence indicators. The gate now uses `e.clock < current.Clock` for the stale-drop path, and additionally accepts `e.clock == current.Clock` when the entry is null AND the client is currently active. Strictly-older clocks and equal-clock non-null updates are still dropped (no new information).

- **Awareness: local clock now reconciles with remote echoes (#73 vector C3, MEDIUM)**. `SetLocalState` previously did a plain `a.clock++`, which could regress if a remote peer had echoed our clientID at a higher clock (e.g. another browser tab also acting as this clientID). It now does `a.clock = max(a.clock, states[a.clientID].Clock) + 1` so the emitted clock is always greater than anything peers have already observed for us.

- **Awareness: `RemoveExpired` exempts the local client (#73 vector C4, MEDIUM)**. Previously the sweep walked every entry in `meta`, including our own — meaning the local client could self-expire in a quiet room. Now skipped. Matches y-protocols / yrs: the local peer can't tell whether *it* has gone silent, so it's responsible for refreshing presence via `SetLocalState` or `Heartbeat`.

## [1.10.0] — 2026-05-19

### Added

- **`WriteAny` now accepts Go unsigned integer types and the narrower signed types** (#77): `uint`, `uint8`, `uint16`, `uint32`, `uint64`, `int8`, `int16`, `int32` previously panicked. They are now promoted to `int64` and dispatched through the same magnitude-based tag logic as `int` / `int64`. `uint64` values exceeding `math.MaxInt64` fall back to tag 123 (float64) with documented precision loss, matching lib0 JS's behavior for very-large `Number`s.
- **`encoding.ErrInvalidUTF8`** (#77): returned by `ReadVarString` when the byte payload is not valid UTF-8.

### Fixed

- **`WriteAny` integer dispatch now matches lib0 byte-for-byte** (#77): ygo previously emitted tag 125 (int + VarInt) for any `int`/`int64` up to 2^55-1, regardless of magnitude. lib0 only uses tag 125 for int32-range values; larger integers go to tag 123 (float64) or tag 122 (BigInt). ygo now uses the same dispatch:

  | Range | Tag | Wire format |
  |-------|-----|-------------|
  | `[-2^31, 2^31)` | 125 | VarInt |
  | `[-2^53, 2^53)` (outside int32) | 123 | float64 |
  | beyond float64 safe-int | 122 | BigInt |

  **Compatibility note:** ygo-encoded updates produced before this fix used tag 125 for ints in the 2^31..2^54 range; Yjs JS still decodes those correctly (tag 125 → VarInt → JS Number). After the fix, ygo's emitted bytes for these values match Yjs JS byte-for-byte. **Round-trip type change for Go callers:** a Go `int64(2^35)` previously round-tripped as `int64`; it now round-trips as `float64` (tag 123). A Go `int64(2^55)` previously panicked in `WriteVarInt`; it now round-trips as `encoding.BigInt`. Callers who depend on int64 type fidelity for values outside int32 range should use `encoding.BigInt` explicitly.

- **`WriteAny(float64)` narrows to float32 when lossless** (#77): lib0 emits tag 124 (4 bytes) when `float32(v) == v`; ygo previously always emitted tag 123 (8 bytes). Now matches lib0. Halves the wire size for values like `1.5`, `-0`, and small integer-valued floats.

- **`ReadVarString` rejects invalid UTF-8** (#77): previously `string(b)` silently accepted any byte sequence, producing strings that downstream consumers (JSON serializers, JS clients, persistence) couldn't handle correctly. Now returns `ErrInvalidUTF8`, matching lib0's `TextDecoder('utf-8', { fatal: true })`. **Perf note:** `ReadAny` of a string is ~4ns slower per call (utf8.Valid scan). Negligible at typical workloads; correctness is the priority.

## [1.9.0] — 2026-05-19

### Added

- **`sync.WithErrorHandler(fn func(error))` option for `ApplySyncMessage` (#79)**: when set, the dispatcher routes `ApplyUpdateV1` errors to the caller-supplied handler and returns `(nil, nil)` instead of propagating the error out. Lets a transport read loop continue across a single malformed update from a peer rather than tearing down the connection. Without the option, the existing return-the-error behavior is preserved (back-compat). Decoding errors on the message header itself (truncated frames, unknown message types) are still returned regardless. Matches y-protocols `readSyncMessage(..., errorHandler)`.

## [1.8.1] — 2026-05-18

### Added

- **`awareness.Awareness.SetMaxBytes(n int64)`** (#48): caps the cumulative byte size of awareness state held in one Awareness instance across all remote clients merged via `ApplyUpdate`. A value of 0 (the default) is unlimited. Incoming entries that would push the total past the cap are silently dropped (treated as null), matching the existing pattern for oversized-state handling.
- **`provider/websocket.Server.MaxAwarenessBytesPerRoom int64`** (#48): server-level config that forwards `SetMaxBytes` to each room's `Awareness` at room creation. Caps total awareness state per room.

### Fixed

- **`crdt`: `Item.integrate` now resolves `Right` from `OriginRight` (#65, #68)**: the YATA conflict-scan loop in `Item.integrate` terminates on `o != item.Right`, but `item.Right` was never populated from `item.OriginRight` before the loop ran. When an incoming item declared a right boundary via `OriginRight`, the scan had no upper bound and placed the item past concurrent items that share the same `Origin`. This caused divergence with Yjs JS and yrs on any update whose items used `OriginRight` — including local inserts in the middle of same-client runs and remote inserts that should respect a known right neighbour. Mirrors Yjs JS's `getItemCleanStart(this.rightOrigin)` call at the top of `Item.integrate`.
  - Adds a new `StructStore.getItemCleanStart(txn, id)` helper that mirrors the existing `getItemCleanEnd`, splitting an existing item so the returned item starts exactly at the given clock.
  - Regression coverage: 5 new tests in `crdt/yata_origin_right_test.go` covering basic right-boundary placement, conflicting right-origins, split-boundary right-origins, three-replica convergence, and the deleted-right-neighbour case. No measurable perf regression on `BenchmarkApplyUpdateV1`, `BenchmarkYText_Insert`, or `BenchmarkTwoPeerConvergence` (benchstat over n=5; differences within noise).
- **`awareness`: cap on per-state key count (#48 vector A)**: a state JSON object with thousands of small keys (e.g. `{"k1":1,...,"k65535":1}`) passed the existing 1 MiB byte cap (~10 bytes per entry × 65k entries ≈ 650 KiB) but materialised into a multi-MB `map[string]any`. After `json.Unmarshal`, states with more than 1,000 top-level keys are now dropped silently (treated as null).

## [1.8.0] — 2026-05-15

### Added

- **`encoding.BigInt` type + `Any` tag 122 support.** lib0's BigInt tag (used by Yjs JS to encode int64 values that don't fit in JS `Number.MAX_SAFE_INTEGER`) is now decoded into a new `encoding.BigInt` named type, and `WriteAny` can encode that type with tag 122. Previously, ygo failed to decode updates containing JS BigInt values. Adds `Encoder.WriteBigInt64` and `Decoder.ReadBigInt64` helpers.
- **`crdt.WithMaxPendingItems(n int)`** (#46): doc option to configure the pending-items cap.
- **`provider/websocket.Server.MaxPendingItems`** and **`provider/http.Server.MaxPendingItems`** (#46): server-level config that forwards the cap to `crdt.New(...)` for all rooms.
- **`provider/websocket.Server.HandshakeTimeout`** (#47, default 30s): caps how long a peer may stay connected without sending any message after handshake.

### Fixed

- **Float byte order now matches lib0 and yrs (big-endian).** ygo previously encoded float32 and float64 in little-endian, which silently broke binary compatibility with Yjs JS and yrs for any document containing float values. The README has always claimed binary compatibility; this fixes the gap. The wire format for float `Any` values (tags 123 and 124) is now big-endian, matching [lib0's `writeFloat32`/`writeFloat64`](https://github.com/dmonad/lib0/blob/main/src/encoding.js).

  **Compatibility note**: previously persisted ygo updates containing float values will read different float values after upgrading. Cross-implementation use was already broken before this fix; the breakage was in the previous behavior, not this one. If your deployment stores ygo update bytes long-term and uses raw float values (rather than integers encoded via VarInt), plan for a re-encode pass.
- **`crdt`: cap on pending-items queue depth (#46)**: items whose dependencies have not yet arrived are parked in `StructStore.pending.items`. Previously this queue was unbounded — a malicious peer could craft a single max-size update full of items referencing far-future clocks and park multi-GB of items, OOM'ing the server. Now capped at 100,000 by default (configurable via the new `crdt.WithMaxPendingItems` option or `provider/websocket.Server.MaxPendingItems` / `provider/http.Server.MaxPendingItems`). Updates that would exceed the cap return `ErrInvalidUpdate`.
- **`provider/websocket`: initial read deadline on connections (#47)**: the WebSocket read loop had no read deadline before the first message. With `MaxConnections == 0` (the default), an attacker could complete handshakes on many connections and then send nothing, holding goroutines + buffers indefinitely (slow-loris). Now an initial `HandshakeTimeout` (default 30s, configurable via `Server.HandshakeTimeout`) closes any connection that doesn't send a first message in time. The deadline is cleared after the first successful read.

### Documentation

- **CSWSH warning on `Server.AllowedOrigins` (#49)**: godoc on the `AllowedOrigins` field now explicitly warns about Cross-Site WebSocket Hijacking when set to `"*"`. `SECURITY.md` adds a CSWSH entry to the threat model with mitigation guidance.

## [1.7.1] — 2026-05-14

### Documentation

- **Documentation refresh.** README updated to reflect the v1.7.0 reality: extended Features list covering panic safety, cooperative cancellation, error-returning variants, out-of-order delta convergence, WebSocket hardening, observability, semaphore-backed resource limits, `crypto/rand` ClientID, and context-aware persistence. New top-level sections: Persistence, Running in production. Security section rewritten to cover the threat model alongside vulnerability-reporting.
- **`docs/ROADMAP.md` retired.** Replaced with `docs/HISTORY.md`, which covers what was built, the post-1.0 arc, and the upstream-alignment design value. The pre-1.0 phased plan no longer applied.
- **`docs/PERSISTENCE.md` documents `PersistenceAdapterContext`** (added in v1.7.0) with a Postgres-style example using `db.ExecContext`.
- **`docs/ARCHITECTURE.md` and `docs/INTERNALS.md`** signpost v1.x additions and link to CHANGELOG. ARCHITECTURE adds a paragraph on the pending-structs queue.
- **`CONTRIBUTING.md` documents PR conventions** that we settled in practice: Conventional Commits prefixes, the auto-close keyword convention (one `Closes #N` per issue), branch naming, and benchstat discipline for hot-path changes.
- **`.github/PULL_REQUEST_TEMPLATE.md` aligned** with actual practice — CHANGELOG entries go into the next version's heading (no `[Unreleased]` section).

## [1.7.0] — 2026-05-14

### Added

- **`encoding.Encoder.WriteVarIntE(v int64) error` (#26)**: error-returning sibling for `WriteVarInt`. Returns `ErrVarIntOutOfRange` when v's magnitude exceeds the lib0 55-bit limit instead of panicking. Preferred over `WriteVarInt` for callers that wrap user input or untrusted data. Successful output is byte-identical to `WriteVarInt`. Pattern matches v1.3.0's `TransactE`. Existing `WriteVarInt` unchanged (now a thin wrapper).
- **`provider/websocket.PersistenceAdapterContext` (#35)**: optional extension interface that lets persistence adapters receive a context cancelled when `Server.Shutdown` begins. The persistence worker type-asserts at runtime and prefers `StoreUpdateContext` when available, falling back to `StoreUpdate` for existing adapters. Pattern matches `io.WriterTo` / `database/sql/driver.QueryerContext`. Existing adapters work unchanged.

## [1.6.1] — 2026-05-12

### Changed

- **`crdt`: `applyV1Txn` refactored into three helpers (#29)**: the V1 update decoder grew to 277 lines across three releases (#11 pending-structs, #10 cooperative cancellation, v1.4.1 panic-safety). Extracted into `decodeAndPark`, `resolveWithinUpdatePending`, and `drainPending`. Pure refactor — zero behavior change, all existing tests pass without modification.

## [1.6.0] — 2026-05-12

### Added

- **Context-aware sibling methods for Awareness and UndoManager (#27)**: four new methods mirroring the v1.1.2/v1.3.0 TransactContext pattern.
  - `Awareness.SetLocalStateContext(ctx, state) error`
  - `Awareness.ApplyUpdateContext(ctx, update, origin) error`
  - `UndoManager.UndoContext(ctx) (bool, error)`
  - `UndoManager.RedoContext(ctx) (bool, error)`

  Pre-cancelled ctx returns `ctx.Err()` without invoking the operation. Mid-call cancellation is cooperative (matches the existing TransactContext contract and upstream Yjs JS / yrs semantics). Existing methods unchanged.

### Documentation

- **Runnable Examples + quick-start snippets + stability statement (#39)**: 8 new `Example*` functions across `crdt`, `awareness`, `provider/websocket`, `provider/http` test files. Each renders as a runnable, copy-pasteable code block on pkg.go.dev. Each public package's `doc.go` now leads with a `Quick start` snippet and includes a `Stability` section documenting the v1.x compatibility promise.

### Changed

- **`provider/websocket`: split `server.go` into focused files (#21)**: the 956-line `server.go` mixed five concerns (HTTP upgrade, peer lifecycle, sync dispatch, awareness broadcast, persistence). Now organized as `server.go` (Server lifecycle), `peer.go` (peer connection lifecycle), and `persistence.go` (persistence worker). Pure refactor — zero behavior change, no API change.

## [1.5.0] — 2026-04-24

### Added

- **`Doc.PendingStats()` for pending-queue observability (#24)**: returns a snapshot of the pending-structs machinery shipped in v1.2.0 — how many items are parked, how many delete-set ranges are queued, which clients we're blocked on. Cheap (one RLock + small copy). Intended for operational monitoring of out-of-order delta convergence: detecting adversarial peers or persistent convergence gaps from misbehaving clients.
- **`crdt.NewClientID()` public helper**: exposes the same ClientID generator used internally so callers can produce reproducible test setups or coordinate IDs externally.

### Changed

- **`provider/websocket`: hard-cap connections via `semaphore.Weighted` (#23)**: replaces the previous optimistic atomic-counter + rollback for `MaxConnections` and `MaxPeersPerRoom`. The atomic pattern had a race window where N+ε simultaneous connections could briefly exist past the cap before any were rejected. Semaphores provide a hard guarantee under any burst pattern. Adds `golang.org/x/sync` as a direct dependency.
- **`crdt`, `provider/http`: ClientID generation now uses `crypto/rand` (#28)**: replaces `math/rand` at both sites. ClientIDs aren't authentication tokens, but predictable IDs in multi-tenant deployments are a footgun. `SECURITY.md` updated with explicit ClientID semantics.

### Documentation

- **godoc and invariant comment polish (#30)** — contributed by @Jah-yee. Adds an `Origin Tags` section to the `crdt` package doc explaining the `Origin any` convention, struct-level godoc for `provider/websocket.InjectInfo`, godoc for `YArray.prepareFire`, and an invariant comment on `DeleteSet.applyToPartial` documenting the per-client clock-contiguity assumption.

## [1.4.1] — 2026-04-24

### Fixed

- **`provider/websocket`: runWriter goroutine leak on room-membership TOCTOU loss (#33)**: regression introduced in v1.4.0. When the room was deleted between `getOrCreateRoom` and peer registration (rare but reachable during peer churn), the per-peer `runWriter` goroutine was spawned before the TOCTOU check and never cleaned up. Moved the spawn to after the check passes and after the peer is registered with the room.
- **`awareness`: `StartAutoExpiry` leaked goroutine on double-call (#34)**: calling `StartAutoExpiry` more than once on the same `Awareness` orphaned the first goroutine because `a.stopExpiry` was overwritten without calling the previous stop. Now the previous goroutine is stopped before spawning the new one. Returned `stop` is also `sync.Once`-protected against double-close panics.

### Changed

- **`provider/websocket`: dropped stale per-peer goroutine spawn in inject paths**. Three `go p.write(data)` call sites in `BroadcastUpdate` and `Apply` were leftover from before v1.4.0 when `peer.write()` was synchronous. After v1.4.0, `peer.write()` is non-blocking; the `go` prefix added churn and scrambled write ordering. Now direct calls.

## [1.4.0] — 2026-04-24

### Added

- **`provider/websocket.Server.MaxMessageBytes` (#20)**: per-message size cap on the read path is now configurable. Default is 64 MiB (matching Rust yrs-warp's underlying warp default; conservative relative to Yjs y-websocket's implicit 100 MiB inheritance from `ws`). Lower this for stricter limits in untrusted deployments.
- **`provider/websocket.Server.Logger` (#18)**: structured logging via `*slog.Logger`. Defaults to `slog.Default()`. Surfaces previously-silent failures (slow-peer write errors, malformed sync messages, awareness update errors) at `Warn` level with `room` and `peer` context.
- **`provider/websocket.Server.PeerWriteQueueSize` (#19)**: bounded per-peer broadcast queue. Default 256. When a peer's queue fills (slow peer / dead connection / lagged receiver), the peer is disconnected. The CRDT pending-structs machinery (v1.2.0) handles reconnect-and-resync cleanly.

### Changed

- **`provider/websocket` broadcast model (#19)**: replaced "spawn one goroutine per peer per broadcast" with a persistent per-peer writer goroutine draining a buffered channel. Matches `yrs-warp`'s bounded-broadcast pattern. Eliminates unbounded goroutine churn under high broadcast cardinality. Slow peers are disconnected (forcing reconnect-and-resync) rather than silently accumulating un-delivered messages.

## [1.3.0] — 2026-04-24

### Added

- **`Doc.TransactE` and `Doc.TransactContextE` (#14)**: error-returning sibling methods for `Transact` and `TransactContext`. Callers can now signal logical errors from inside a transaction body without resorting to panic (too heavy) or out-of-band channels (clumsy). `fn`'s returned error becomes the method return value. For `TransactContextE`, `ctx.Err()` wins when both a ctx cancellation and an fn error fire. Mutations commit regardless of error (no rollback — matches Yjs JS's `doc.transact(f)` and yrs' RAII semantics); observers fire BEFORE the error returns, matching Yjs JS's `cleanupTransactions`-in-`finally` pattern.

### Changed

- **`transactInternal` refactored** to take `func(*Transaction) error` and return `error`. The existing `Transact` and `TransactContext` are thin wrappers and keep their original signatures — strictly additive for public callers.

## [1.2.0] — 2026-04-23

### Fixed

- **Cross-update Origin dependencies on out-of-order delivery (#11)**: when a peer received independent delta updates from concurrent producers out of dependency order (e.g. delta B arrived before delta A, and B's items referenced A's items via `Origin` / `OriginRight`), B's items were silently orphaned in the struct store and never integrated into the linked list, producing permanent convergence gaps that only a fresh sync step 1/2 exchange could repair. Items whose dependencies have not yet been integrated are now parked in a doc-level pending queue and retried automatically on each subsequent `ApplyUpdateV1` / `ApplyUpdateV2`.
- **Same-client clock gaps silently mis-integrated (#11, adjacent)**: if a peer received clocks 4 and 5 from client X without first receiving clock 3, the items were inserted at the head of the parent list with a `nil` origin lookup. These now park in the same pending queue and drain when the missing predecessor arrives.
- **Delete-set entries targeting not-yet-integrated items** were silently dropped. Unresolvable entries now accumulate in a `pendingDs` and retry each time pending items make progress, mirroring Yjs JS's `pendingDs` and yrs' `pending_ds`.

### Changed

- **Convergence semantics match Yjs JS and yrs.** The pending-structs machinery is semantically equivalent to the upstream implementations (`StructStore.pendingStructs` in Yjs JS, `Store.pending` in yrs). One mechanical deviation: retry is inline rather than recursive, because Go's `sync.Mutex` is not reentrant.

## [1.1.2] — 2026-04-22

### Added

- **`Transaction.Ctx()` accessor (#10)**: fn running inside `Transact` or `TransactContext` can now call `txn.Ctx().Err()` or `<-txn.Ctx().Done()` to cooperatively detect cancellation and return early. Mutations made before the early return commit; those that would have happened after do not. `Transact` populates the ctx with `context.Background()` so bare callers see a non-cancellable context.

### Changed

- **`TransactContext` godoc rewritten** to document the cooperative-polling contract explicitly. Behavior is unchanged for existing callers: the entry-guard check still runs, fn still executes to completion if it does not poll, and ctx.Err() is still returned as a "cancellation happened" signal. The new godoc clarifies that Go cannot safely interrupt arbitrary fn code (same constraint as Yjs JS and yrs).

## [1.1.1] — 2026-04-21

### Fixed

- **`Doc.Transact` lock leak on panic (#9)**: if `fn` (or any Phase 1 work) panicked, `d.mu` remained held forever, wedging the document. Any subsequent operation that needed the lock — `GetMap`, `GetText`, `ApplyUpdateV1`, a further `Transact`, an `OnUpdate` subscribe/unsubscribe — deadlocked. Transact now wraps its body in a deferred `recover()` that releases `d.mu` on every exit path.

### Changed

- **`Doc.Transact` panic semantics are now explicit.** On panic: observers fire with whatever partial state `fn` committed (matching Yjs JS and `yrs`), then the original panic is re-raised. Rollback is not provided — callers needing atomicity should recover and reconcile via sync or recreate the doc from persistence. Previously behavior was undefined (the caller deadlocked before any observer could run).
- **`websocket.Server.Apply` godoc** updated: a panicking `fn` no longer wedges the room. The caveat is softened accordingly; partial-state broadcasts are now the documented behavior.

## [1.1.0] — 2026-04-20

### Added

- **Server-side document injection** for AI agents and backend APIs. Three new methods on `*websocket.Server` let server-side Go code push changes into a live room without simulating a WebSocket peer (issue #8):
  - `BroadcastUpdate(ctx, room, update)` — fan a pre-encoded V1 update out to all connected peers. Does not mutate the server's doc; callers pair it with `crdt.ApplyUpdateV1` (or use `Apply`) to keep server state in sync. Validates ctx, room name, update size, and bytes before dispatch.
  - `Apply(ctx, room, fn)` — run a callback that mutates the doc via a bound `transact` helper, capture the delta with an origin-scoped `OnUpdate` subscription, and broadcast it. Auto-creates the room if needed; persistence flows through the existing `OnUpdate` hook.
  - `CloseRoom(name, force)` — explicit teardown for rooms that have no peers (typically ones created by `Apply`).
- **Access-control hook:** `Server.OnInject func(ctx context.Context, info InjectInfo) error` gates both `BroadcastUpdate` and `Apply`. `InjectInfo.Op` (`OpBroadcastUpdate` | `OpApply`) and `InjectInfo.UpdateSize` let policy differ per path and per size. Refusals are wrapped with the new `ErrInjectRefused` sentinel.
- **Resource caps:** `Server.MaxUpdateBytes` (per-update; default 64 MiB matching peer frame limit) and `Server.MaxRooms` (total rooms; default unlimited). `MaxRooms` applies uniformly to peer upgrades (HTTP 503) and `Apply` (`ErrTooManyRooms`).
- **Error sentinels:** `ErrServerShutdown`, `ErrInvalidRoomName`, `ErrRoomNotFound`, `ErrRoomHasPeers`, `ErrInvalidUpdate`, `ErrUpdateTooLarge`, `ErrTooManyRooms`, `ErrNoChanges`, `ErrInjectRefused` — all comparable with `errors.Is`.

### Changed

- Peer upgrades past `MaxRooms` now return HTTP 503 (previously, unbounded room creation was only capped indirectly by `MaxConnections`).
- Persistence goroutines now exit cleanly on `Server.Shutdown` even when their room has never had a connected peer. Previously this combination (reachable via `Apply` + persistence with no peers) hung `Shutdown`.

### Security

- Every server-side write path validates the room name via `isValidRoomName` — primary defense against path traversal in persistence adapters that key on room name.
- `BroadcastUpdate` validates update bytes at the server boundary via a throwaway `ApplyUpdateV1`, rejecting malformed input with `ErrInvalidUpdate` before any peer sees it.
- `MaxUpdateBytes` and `MaxRooms` close two DoS vectors enabled by the new API (oversized updates fanned out to all peers; unbounded room creation exhausting memory and persistence-backend connections).

## [1.0.5] — 2026-04-13

### Added

- **CRDT-safe array Move**: `YArray.Move()` now creates a `ContentMove` marker item instead of deleting and reinserting. The moved element preserves causal history; concurrent moves of different elements both apply; concurrent moves of the same element converge to the lower-ClientID winner. `ContentMove` is included in V1 and V2 wire encoding (`wireMove = 11`).
- **XML insert API**: `YXmlFragment.InsertElement`, `YXmlFragment.InsertText`, `YXmlElement.InsertElement`, and `YXmlElement.InsertText` are now exported, allowing external packages to build XML documents programmatically without reflection.

### Fixed

- **YText Format observer delta**: `YText.Format()` now emits an accurate `retain N + attributes` delta to observers. Previously the delta was missing or malformed, causing collaborative editors to show stale formatting to connected peers.

## [1.0.4] — 2026-04-10

### Fixed

- **Nil panic on reconnect with GC'd YMap items**: `delete()` on orphaned GC placeholder items (Parent==nil) dereferenced `item.Parent` for length adjustment and `addChanged()`, adding nil to `txn.changed` and causing a panic in Transact's observer loop. Also fixed nil check in YATA conflict scanning when `store.Find()` returns nil for GC'd origins.
- **Cross-browser sync corruption with emoji/supplementary characters**: ContentString encoding with offset used `[]rune` indexing (Unicode code points) but the offset is in UTF-16 code units. For emoji and supplementary characters (2 UTF-16 units each), the encoder produced corrupt binary that Yjs clients couldn't decode. Fixed in both V1 and V2 encoders to use `utf16ByteOffset()`.

## [1.0.3] — 2026-04-09

### Fixed

- **GC'd YMap origins crash `StoreUpdate` and break real-time sync**: When a Yjs client sends updates containing GC structs from repeated `YMap.Set` on the same key, the decoder errored with "N items with unresolvable parents" and rejected the entire update — crashing persistence and dropping broadcasts for the whole room. Fixed: the decoder now stores orphaned items gracefully (matching y-websocket JS server behavior), and the encoder re-encodes them as GC structs for valid clock accounting. Multi-client documents resolve the parent from other clients' items when available.
- **Encoder wrote corrupt parent info for items with GC'd origins**: `EncodeStateAsUpdateV1`/V2 wrote origin references pointing to parentless GC placeholders, which receivers couldn't decode. Fixed: the encoder detects GC'd origins and falls back to explicit parent info (named root type or container item ID).

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

[1.8.0]: https://github.com/reearth/ygo/releases/tag/v1.8.0
[1.7.1]: https://github.com/reearth/ygo/releases/tag/v1.7.1
[1.7.0]: https://github.com/reearth/ygo/releases/tag/v1.7.0
[1.6.1]: https://github.com/reearth/ygo/releases/tag/v1.6.1
[1.6.0]: https://github.com/reearth/ygo/releases/tag/v1.6.0
[1.5.0]: https://github.com/reearth/ygo/releases/tag/v1.5.0
[1.4.1]: https://github.com/reearth/ygo/releases/tag/v1.4.1
[1.4.0]: https://github.com/reearth/ygo/releases/tag/v1.4.0
[1.3.0]: https://github.com/reearth/ygo/releases/tag/v1.3.0
[1.2.0]: https://github.com/reearth/ygo/releases/tag/v1.2.0
[1.1.2]: https://github.com/reearth/ygo/releases/tag/v1.1.2
[1.1.1]: https://github.com/reearth/ygo/releases/tag/v1.1.1
[1.1.0]: https://github.com/reearth/ygo/releases/tag/v1.1.0
[1.0.5]: https://github.com/reearth/ygo/releases/tag/v1.0.5
[1.0.4]: https://github.com/reearth/ygo/releases/tag/v1.0.4
[1.0.3]: https://github.com/reearth/ygo/releases/tag/v1.0.3
[1.0.2]: https://github.com/reearth/ygo/releases/tag/v1.0.2
[1.0.1]: https://github.com/reearth/ygo/releases/tag/v1.0.1
[1.0.0]: https://github.com/reearth/ygo/releases/tag/v1.0.0
