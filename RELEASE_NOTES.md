## v1.44.0

**Who is affected:** only callers who put non-UTF-8 bytes into a document —
a Go `string` holding an invalid UTF-8 byte sequence, passed to `YMap.Set`,
`YArray.Insert`/`Push`, `YText.Insert`/`ApplyDelta`/`InsertEmbed`/`Format`, a
root-type accessor, an XML node-name/attribute setter, or `WithGUID`. Most
callers are not doing this — Go string literals and anything read from JSON,
HTTP bodies, or ordinary text sources are valid UTF-8 already. If you build
strings from raw byte slices, non-UTF-8-safe truncation, or another encoding
without converting, you may be affected.

**What changes:** those calls now **panic**, naming the offending input, where
previously they succeeded and produced an update that encoded without
complaint but that neither ygo nor Yjs could decode — a room that silently
stopped converging and a stored update that could never be reloaded. A panic
on write is a large behavior change but it replaces silent, hard-to-diagnose
corruption with an immediate, attributable failure at the point the bad
string was introduced, instead of downstream at decode time (or never, if the
divergence went unnoticed).

**How to prepare:**
- **Pre-check** with `utf8.ValidString` (standard library `unicode/utf8`)
  before passing a string into any of the calls above, if you can't already
  guarantee the string is valid UTF-8.
- **Recover**, if you'd rather catch the panic at a call boundary than
  pre-check every string (e.g., wrapping a batch-import path that processes
  many documents and should skip/report a bad one rather than crash the
  whole batch).
- **Use `Encoder.WriteVarStringE`** if you're working at the encoding layer
  directly (not through the CRDT mutator API): it returns `ErrInvalidUTF8`
  instead of panicking, for callers encoding untrusted input themselves.

**The one place ygo repairs instead of rejecting:** the WebSocket provider's
auth reply path (`encodeAuthMessage` in `provider/websocket/peer.go`) coerces
an app-supplied diagnostic string — an `OnTokenAuth` hook's error text, or the
`"read-write"`/`"readonly"` scope label — with `strings.ToValidUTF8` rather
than panicking. That string runs on a live connection goroutine at a point
where crashing the goroutine over a malformed error message from the
application layer would be worse than the alternative: replacing invalid runs
with U+FFFD and keeping the connection and the diagnostic readable. Nowhere
else in this change coerces; this is the sole, deliberate exception, guarding
a live goroutine against an application's own string rather than a wire or
relay identifier.

**Performance:** encoding is slower, because `Encoder.WriteVarString` now runs
a `utf8.ValidString` pass over every string on every encode. The committed,
CI-gating benchmark, `BenchmarkEncodeStateAsUpdateV1`, measured **+8.63%**
(n=10). That benchmark is a worst case rather than a typical one:
`buildTextDoc(1000)` performs 1000 one-character inserts, each in its own
transaction, so the encoded document is 1000 items each holding a one-byte
string — maximum validation call count, with no string length at all to
amortize each call's fixed overhead over. `BenchmarkEncodeStateAsUpdateV1_Bulk`,
also committed, encodes the same total byte count built as a single bulk
insert instead of 1000 tiny ones — the shape most real documents actually
have — and measured **+2.86%** (n=10). Allocations are unchanged in both
cases. `ApplyUpdateV1` (decode), the heavier path at roughly 115µs, moves
only about +1.2%, since the decode side already validated UTF-8 before this
release. This is not rounded down to "negligible": it is a real, measured
cost, worst on documents built from many tiny string items.

**Why we're paying it:** most of this cost buys defence-in-depth rather than
closing a real gap. Every string in a document tree already passes either a
mutator boundary check (the new validation added in this release) or the
validating decoder (`ReadVarString` has always rejected invalid UTF-8 on the
read side). The encode-time check added here uniquely covers strings
assigned directly to exported struct fields that bypass the mutator API. For
`RelativePosition.Tname` this release adds a targeted check at
`EncodeRelativePosition`, so a caller building `RelativePosition{Tname: bad}`
by hand — there is no constructor to intercept it — gets a panic naming the
field rather than the encoder's generic one. `YXmlElement.NodeName` and
`ContentAttribute.Name` are the fields left relying on the generic
`Encoder.WriteVarString` backstop alone: both are exported and directly
assignable, bypassing `NewYXmlElement`/`NewContentAttribute`. We are
knowingly paying the encode-time cost so that none of these three paths can
silently produce an update no decoder will accept.

**This change deliberately reverses a documented decision.** Commit
`c3ba5ff` (2026-05-19, issue #77) added
`TestUnit_VarString_AsymmetricUTF8Contract`, codifying "write trusts, read
validates" — deliberately asymmetric, on the reasoning that adding write-side
validation would break callers passing pre-encoded data through the varstring
path. That asymmetry is now reversed: `WriteVarString` validates. The
reasoning that changed is this — a caller with genuinely pre-encoded binary
data was never using the varstring wire format correctly in the first place;
`WriteVarBytes` is the correct call for that, because a varstring is UTF-8 by
wire definition, not an arbitrary-bytes container. What such a caller
produces today, by routing binary through `WriteVarString`, is an update that
no decoder — ygo's own `ReadVarString`, or Yjs's — will accept. Validating on
write closes a contract that was already broken in practice; it does not
newly break a working pattern.

### Changed

- **Invalid UTF-8 is now rejected where it enters the document**, across
  `YMap`, `YArray`, `YText`, the XML types, `RelativePosition`, and
  `Doc.WithGUID`. See the upgrade impact above. (#209)
- **Room names must be valid UTF-8.** `internal/roomname.Valid` previously
  accepted invalid UTF-8, because ranging over a Go string yields
  `utf8.RuneError` for bad bytes rather than failing outright. Affects both
  the HTTP and WebSocket providers. (#209)

### Added

- `Encoder.WriteVarStringE` — the non-panicking, error-returning variant of
  `WriteVarString`, for callers who want to handle invalid UTF-8 rather than
  have it panic. (#209)

## v1.43.0

ygo could decode and materialise nested Y types but offered no way to construct
one: `abstractType` is unexported, `YArray.Insert` batches plain values into a
single `ContentAny` item, and `YMap.Get` returned `(nil, false)` for a key
holding a nested type despite it being visible through `ToJSON`. This release
adds the public surface — so a shape like a Jupyter notebook cell, a `Y.Map`
holding a `Y.Text`, can be built from outside the package — and makes detached
types behave the way the reference implementation does. No breaking API changes.

### Added

- **Public prelim constructors: `NewMapPrelim`, `NewTextPrelim`,
  `NewArrayPrelim`, and `YArray.PushType`.** Each returns a detached type that
  is inert until attached, at which point its content materialises with
  parent-first clocks — the ordering genuine Yjs produces and can decode.
  `PushType` gives a nested type its own item rather than batching it into a
  `ContentAny`, the same reason `YXmlFragment` exposes
  `InsertElement`/`InsertText`. This generalises to the core types what #147 and
  #170 established for `YXml`.

- **`YMap` and `YArray` stage their content while detached**, mirroring Yjs's
  `_prelimContent`. Mutations edit the staged content directly and the net
  result materialises once, when the container item integrates: a key set twice
  emits once, a key set then deleted emits not at all, and consecutive pushes
  coalesce into a single item. A detached build therefore emits the same structs
  Yjs emits rather than one per call. `YText` is unchanged — Yjs stages
  `Y.Text` as deferred operations (`_pending`), which the existing model already
  matches.

- **Detached reads answer from the staged content.** `Len`, `Get`, `Keys`,
  `Has`, `ToSlice`, `Entries`, `ForEach` and `ToJSON` now report what a detached
  `YMap` or `YArray` holds, recursively unwrapping staged nested types. This is
  for the core types what #170 did for detached XML nodes, rather than reversing
  that convention in a sibling API.

- **Conformance fixtures for prelim construction**
  (`testutil/gen_fixtures_prelim.js`). Yjs runs a scripted build with a pinned
  clientID and Go must replay the identical sequence and emit byte-identical V1
  bytes, in both directions, across eleven shapes — including the multi-call
  builds that distinguish staged content from replayed calls. Plus a fuzz target
  over nested prelim shapes.

### Changed

- **Shared types are rejected as plain values.** `YMap.Set` panics on an
  attached shared type, and `YArray.Insert`/`Push` panic on a shared type among
  `vals`, pointing at `PushType` instead. Previously these stored the type
  inside a `ContentAny`, which read back as an empty blob and then panicked the
  encoder at commit time — inside `Doc.Transact` when an `OnUpdate` hook is
  registered, which is every websocket deployment. The failure moves from a
  process-killing panic in the transaction machinery to a rejection at the call
  site.

### Fixed

- **`YMap.Get` returns nested types.** A key holding a `Y.Text`, `Y.Map` or
  `Y.Array` read back as `(nil, false)` because `Get` handled only `ContentDoc`
  and `ContentAny`, even though the type was fully materialised and reachable
  through `ToJSON`. It now mirrors `YArray.Get`.

- **`YArray.Move` on a detached array no longer emits `ContentMove`.** It
  reorders the staged content instead, so the attached result carries ordinary
  content that other implementations decode. Emitting the wire extension here
  was reachable through the prelim API and diverged silently: verified against
  pycrdt 0.13.1, the peer accepted the update without error and read the
  pre-move order while ygo read the moved one (#207).

## v1.42.0

Two contract changes for anyone with custom clustering code — nothing to do
here if you only use shipped components. **`cluster.Sink.Inject` may now be
called concurrently for distinct rooms** (same-room calls still stay
serialised and in publish order) — this is what a custom `Sink` must handle.
**`cluster.Relay.Publish` may now be called concurrently for distinct rooms,
and briefly twice for the SAME room** during a room's eviction/reload
handoff, with no per-room ordering guaranteed at all — this is what a custom
`Relay` must handle. `*websocket.Server` (the shipped `Sink`), `MemRelay`, and
`cluster/redis` (the shipped `Relay`s) are already safe for both; a
third-party `Sink` or `Relay` written against the old single-caller
assumption must confirm it is safe for concurrent use before upgrading.
Neither permission is a guarantee every relay exercises: `cluster/redis`
takes advantage of both, with a dedicated goroutine per room on each side,
so one slow room's `Inject`/`Publish` call can no longer stall delivery for
others (#187); `MemRelay`, the in-process reference relay, still delivers
every room from one goroutine per node and does not (yet) get that isolation
on either side.

Also fixed, and arguably the more consequential bug: a pre-existing defect,
shipped since v1.20.0 (the cluster relay's introduction), where `Server.Apply`
and the relay's echo-guard sentinel could both be satisfied from the same
address by Go's zero-size-allocation guarantee (`new(struct{})` twice), so the
two "distinct" origin markers compared equal. Two silent consequences: the
relay's echo guard mistook every `Apply`-driven write for its own echo and
dropped it before it ever reached the wire — any deployment calling
`Server.Apply` on a server with a relay attached has never actually
replicated those writes to other nodes — and separately, a concurrent
relay-injected update could bleed into the delta `Apply` returns to its own
caller. Fixed by giving each sentinel its own named, non-zero-size type.

The rest of the release is the performance/isolation half of the same
change set (#187): relay delivery — inbound in `cluster/redis`, outbound in
`provider/websocket` — used one shared queue for the whole node in each
direction, so one slow room's `Inject`/`Publish` call stalled delivery for
every other room. Each room now gets its own bounded lane and worker, so
that specific stall is gone in both directions. A saturated lane coalesces
its `KindSync` backlog (`crdt.MergeUpdatesV1`) instead of dropping it — a
saturated awareness slot instead replaces the queued blob with the newest one
(`AwarenessSuperseded`, not a drop, since awareness is idempotent heartbeat
state). That coalescing is not free, and not fully isolated either: on both
sides, once a room's backlog exceeds its cap, the merge runs synchronously,
on the goroutine that's doing the enqueueing — the Transact commit-path
goroutine outbound, `cluster/redis`'s single subscriber goroutine inbound.
Outbound this is a private cost to that one room's commit path (never a
block, just an occasional bounded merge). Inbound, because the subscriber
goroutine is shared across every room on the node, a wedged room's merge
attempt *can* still delay reading (and therefore delivering) other rooms'
messages for as long as the merge takes — bounded and amortized while merges
keep succeeding, unbounded while they keep failing (see #184 for the merge
cost itself). None of this changes what Redis pub/sub itself guarantees:
still at-most-once by Redis's own definition, with persistence healing a drop
only on the room's next reload, which a hot room never gets. What actually
changed is that a full lane can no longer arise from ordinary volume alone,
so a hard drop now signals a failing merge specifically, not exhausted
capacity — watch `Relay.Stats()` / `RelayStats()` as the health signal:
`Coalesced`/`AwarenessSuperseded`/inbound-only `RouterDrops` are routine,
alert on their rate; `HardDrops`/outbound-only `Dropped` should always be
zero, alert on presence.

One exception to that "should always be zero" rule: `Shutdown` cancels the
relay context before closing peer connections and before the persistence
drain, so peers can keep committing for the rest of shutdown — potentially
seconds — after outbound relay delivery has already stopped. Whatever is
still queued in a room's outbound lane at that point, or arrives afterward,
is discarded on `Shutdown` without incrementing `Dropped` or `HardDrops`, so
those counters reading zero does not mean nothing was lost across a
`Shutdown`. This is pre-existing behaviour, not new in this release — the
single shared outbound worker it replaces discarded the same way — but it is
newly *mis*-documented if left unstated, since this release is what
establishes `Dropped`/`HardDrops` as the "always zero, alert on presence"
signal. A fix (draining the outbound lanes, or reordering `relayCancel`
relative to connection-close/persistence-drain) is out of scope here — it
needs its own change, since reordering also delays inbound shutdown.

### Fixed

- **`Server.Apply` writes were silently never relayed to other nodes, since
  v1.20.0.** `Server.Apply` and `AttachRelay` each minted their origin
  sentinel with `new(struct{})`; Go's zero-size-allocation guarantee let the
  runtime satisfy both from the same `runtime.zerobase` address, so the two
  pointers compared equal even though they were meant to be distinct
  identities. The relay's echo guard misidentified every `Apply`-driven
  commit as a self-echo and dropped it before publish; separately, a
  concurrent relay-injected update could bleed into the delta `Apply` returns
  to its own caller. Fixed by giving each sentinel its own named, non-zero-
  size, unexported type.
- **Head-of-line blocking in relay delivery, both directions**
  ([#187](https://github.com/reearth/ygo/issues/187)). `cluster/redis`'s
  subscriber called `Sink.Inject` synchronously in its receive loop, so one
  slow room's `Inject` call stalled inbound delivery for every room on the
  node; `provider/websocket` had the same defect outbound, via one shared
  queue and worker for all rooms. Each room now gets its own bounded lane and
  worker in both directions, so a slow `Inject`/`Publish` call for one room no
  longer blocks any other room's delivery. `MemRelay` is not part of this fix
  — it still delivers every room from a single per-node goroutine.
- **Relay updates are coalesced instead of dropped under saturation.** A full
  lane merges its queued `KindSync` backlog via `crdt.MergeUpdatesV1` rather
  than dropping it (a full awareness slot instead replaces the queued blob,
  counted as `AwarenessSuperseded`, not a drop). The previous outbound drop
  was justified in-comment by "peers reconcile via sync step 1/2", which does
  not hold for a hot room — reconciliation needs a room reload, and a room
  with a connected client never reloads. Neither direction is merge-free:
  outbound, the commit path never blocks but does occasionally merge, on the
  caller's own goroutine; inbound, that same merge runs on `cluster/redis`'s
  single subscriber goroutine, shared across every room, so a wedged room's
  merge attempts can delay other rooms too (bounded/amortized while merges
  succeed, unbounded while they keep failing — see #184 for the merge cost
  itself). A hard drop remains possible on either side only when the merge
  keeps failing.

### Changed

- **`cluster.Sink.Inject` may now be called concurrently for distinct rooms.**
  Calls for the same room remain serialised and in publish order.
  `*websocket.Server` is already safe; third-party `Sink` implementations must
  confirm they are before relying on this. `cluster/redis` exercises the new
  permission; `MemRelay` does not yet.
- **`cluster.Relay.Publish` may now be called concurrently for distinct
  rooms, and two concurrent calls for the SAME room are possible.**
  `provider/websocket` drives `Publish` from one worker goroutine per room
  (not one per server), so distinct rooms overlap by design; across a room's
  eviction/reload handoff a predecessor lane's final drain can also briefly
  overlap with the successor lane's worker publishing for the same room
  name. Reviewed and accepted as benign — the `Relay` contract imposes no
  per-room ordering, `KindSync` blobs are commutative/idempotent, and stale
  awareness is dropped by the awareness clock gate — but a third-party
  `Relay` built on the old single-caller assumption must confirm it is safe
  for concurrent use, exactly as a custom `Sink` must. Both shipped relays
  already are: `MemRelay.Publish` snapshots its node list under its own
  mutex before sending; `cluster/redis`'s `Publish` takes no lock at all and
  uses atomics plus channels.
- `cluster/redis`'s package documentation states the at-most-once delivery
  reality explicitly, including that persistence heals a dropped update only
  on room reload and that idle residency (#183) lengthens that window rather
  than shortening it. It also corrects two overclaims the doc previously
  made: `Coalesced` non-zero is routine on any busy room (alert on its rate,
  not its presence — this was already stated correctly in `Stats`' own doc,
  just not in the package doc), and "one slow room never stalls delivery for
  another" is now scoped to the delivery work itself rather than the shared
  enqueue-time merge cost described above.
- `cluster.Relay`'s `RoomActivated`/`RoomDeactivated` contract now documents
  that implementers must tolerate a successor room activating a name before
  the predecessor's deactivate call for that name lands (both shipped relays
  already do, via reference-counting or no-op).

### Added

- `cluster/redis.Config.RoomQueueSize`, `cluster/redis.Relay.Stats()` (with
  `Coalesced`/`AwarenessSuperseded`/`HardDrops`/`RouterDrops`), and
  `websocket.Server.RelayStats()` (with `Coalesced`/`AwarenessSuperseded`/
  `HardDrops`/`Dropped`). `relayDropped` (now `RelayStats.Dropped`) was
  previously incremented and read nowhere, making outbound relay loss
  invisible; `RouterDrops` is new inbound instrumentation with no prior
  equivalent. Alert on `Coalesced`/`AwarenessSuperseded`/`RouterDrops` by
  rate (routine); alert on `HardDrops`/`Dropped` by presence (should always
  be zero). Monotonic for the life of the relay/server, but not guaranteed
  exact under a couple of documented, benign race windows (never an
  overcount, never a decrease).

## Install

```
go get github.com/reearth/ygo@v1.42.0
```

See [CHANGELOG.md](https://github.com/reearth/ygo/blob/main/CHANGELOG.md) for full details.

## v1.41.0

Three additions that together let an application build a real, bounded,
user-facing version history instead of hand-rolling one, plus a published
benchmark suite. Labelled snapshots can now be enumerated and individually
deleted, a store can report which rooms it holds, and the websocket server can
capture versions on its own on a throttled, change-driven cadence. All three are
optional extension interfaces, so no existing interface gained a method and
every third-party backend keeps compiling. Everything is off by default. No
breaking API changes.

### Added

- **`SnapshotStore`: enumerable, individually-deletable labelled snapshots.** The
  existing name-keyed `CaptureSnapshot`/`RestoreSnapshot` pair could be written
  and read by exact name but never enumerated, never individually deleted, and
  carried no metadata, so labelled snapshots were an unbounded, unreclaimable
  resource that only `Delete(room)` could clear. New interface:
  `SaveSnapshot`/`ListSnapshots`/`GetSnapshotState`/`DeleteSnapshot`, plus
  `SnapshotInfo{ID, Label, CreatedAt, Size}` and `SnapshotVersionedPersistence`.
  Snapshots are ID-keyed with non-unique labels, so repeated saves create distinct
  versions rather than overwriting. IDs are unique and monotonic within a room and
  never reused there, but are not globally unique. `ListSnapshots` is newest-first
  and never reads state blobs. The older name-keyed pair is unchanged and still
  supported, superseded for new code.

- **`RoomLister`: store-wide room enumeration.** `VersionedPersistence` could only
  be addressed one room at a time, so store-wide retention, cleanup, migration and
  reconciliation all needed an external index of room names with no way to detect
  drift against what was actually persisted. `ListRooms` reports every room
  holding at least one update or snapshot, so a snapshot-only room stays
  reclaimable, and returns the original room name rather than a backend's on-disk
  encoding.

- **Auto-versioning: `VersionableAdapter` + `Server.AutoVersionEvery`.** The
  server can now drive a version history itself. When the adapter implements
  `VersionableAdapter` and `AutoVersionEvery > 0`, it captures a labelled version
  (`AutoVersionLabel`, `"auto"`) at most once per interval per room **and only
  when the room actually changed**, plus one final version on room unload if it
  changed after the last one. A quiet room is never versioned. That pairing is the
  point: versioning per update is what makes a history panel unusable.
  `persistence.LegacyAdapter` implements it over `SnapshotStore` and gains
  `KeepSnapshots` to bound retained versions (0 = keep all, mirroring
  `KeepVersions`, which is the separate update-log axis). A store lacking
  `SnapshotStore` returns the new `ErrSnapshotsUnsupported` rather than silently
  discarding versions, so a misconfiguration is visible instead of appearing to
  work.

### Performance

- **`FilePersistence.ListSnapshots` no longer reads snapshot state blobs.** It
  read each record in full just to report metadata, making listing O(total
  snapshot bytes) and contradicting the `SnapshotStore` contract's own promise
  that listing stays cheap. It now reads only the 12-byte header plus the label
  and derives `Size` from the directory entry.

### Fixed

- **sqlite `Delete(room)` now also removes labelled snapshots.** The
  `snapshot_versions` rows for a room survived a room delete, contrary to the
  documented "removes all data for room" contract. The memory and file backends
  were already correct. Caught by the new conformance suite.

### Testing

- **Honest performance and scalability benchmark suite**
  ([#180](https://github.com/reearth/ygo/issues/180)): the full
  `dmonad/crdt-benchmarks` B1-B4 set plus cluster-relay, persistence and websocket
  scale benchmarks, a `benchmark` CI workflow, and `BENCHMARKS.md` publishing the
  results with their caveats rather than only the flattering numbers.

- Two new shared conformance suites, `RunSnapshotStoreConformance` and
  `RunRoomListerConformance`, run against all three backends so a third-party
  backend can self-verify the new contracts.

## Install

```
go get github.com/reearth/ygo@v1.41.0
```

See [CHANGELOG.md](https://github.com/reearth/ygo/blob/main/CHANGELOG.md) for full details.

## v1.40.0

Two convergence/lifecycle fixes plus new fuzz coverage. `YArray.Move` could
diverge across peers depending on merge order — a move that integrated
before its target item silently dropped its target claim; moves now defer
target arbitration until the target integrates, so all peers converge
regardless of order. On the websocket server, a room created for a
connection that fails before any peer registers (connection-limit denial or
WebSocket upgrade failure) is now reaped instead of leaking the room and its
persistence/awareness goroutines. No breaking API changes.

### Fixed

- **`YArray.Move` convergence across merge orders**
  ([#191](https://github.com/reearth/ygo/issues/191)): moves now defer
  target arbitration until the target item integrates, fixing a case where
  ascending-ClientID integration order caused peers to diverge.
- **Orphaned websocket rooms on early connection failure**
  ([#192](https://github.com/reearth/ygo/issues/192)): rooms are now reaped
  when a connection fails before any peer registers, instead of leaking the
  room and its background goroutines.

### Testing

- New Go-internal multi-peer move-convergence fuzzer sweep
  (`TestFuzzConvergenceMoves`, [#191](https://github.com/reearth/ygo/issues/191)).
  Moves are a ygo wire extension the yjs reference can't decode, so they're
  validated by internal convergence rather than the yjs cross-impl oracle.
- New yjs-interop guard tests for nested `Y.Map` (and `NaN`) values across
  V1/V2 round-trips ([#194](https://github.com/reearth/ygo/pull/194)),
  proving ygo preserves nested shared-type entries from genuine yjs bytes.

## v1.39.0

A minor release focused on performance and websocket-server room lifecycle.
`YText`/`YArray` positional access (`Get`/`Slice`, positional insert/delete,
`Format`, `ApplyDelta`) now runs through a Yjs-style bidirectional,
move-aware search-marker cache instead of the old forward-only position
cache — a purely internal change, no public API surface moved — cutting
100k-node positional operations by roughly two orders of magnitude. Along
the way, a real `YText.ApplyDelta` formatting bug was fixed: an `insert` op
with no attributes of its own no longer bleeds in a preceding retain's
formatting (Yjs/Quill-aligned); this is a **behaviour change** for code that
relied on the old bleed-through. On the websocket server, room load
(`LoadDoc`/decode/`OnLoadDocument`) no longer runs under the global
room-map lock, so concurrent connects to distinct rooms load in parallel
instead of serializing; and new `Server.RoomIdleTimeout` /
`Server.MaxResidentRooms` fields let a room stay warm in memory for a
bounded time (and bounded count) after its last peer leaves, so a quick
reconnect reuses the live doc instead of a full reload. No breaking API
changes; both new fields default to zero, which preserves prior-release
behaviour exactly.

### Performance

- **O(1) amortized positional access for `YText`/`YArray`**
  ([#181](https://github.com/reearth/ygo/issues/181)): a Yjs-style
  bidirectional, move-aware search-marker structure replaces the old
  forward-only position cache. Internal only — no public API change.
  Measured on a 100k-node document: random-position insert ~101× faster
  (632µs → 6.3µs), reverse insert ~916× faster (1.15ms → 1.26µs), random
  `Get` ~114× faster (481µs → 4.2µs).

### Fixed

- **`YText.ApplyDelta` format-bleed into a following insert**
  ([#181](https://github.com/reearth/ygo/issues/181)): an `insert` op with
  no `Attributes` of its own no longer inherits formatting from a preceding
  `{retain, attributes}` op, matching the Yjs/Quill rule. Behaviour change —
  see summary above. Also fixed: consecutive attribute-less inserts could
  integrate out of order.
- **Room load no longer serializes under the global room-map lock**
  ([#182](https://github.com/reearth/ygo/issues/182)): loading now runs
  off-lock behind a per-room `ready` barrier, so concurrent connects to
  distinct rooms load in parallel; a reentrant load (e.g. cluster relay
  `Inject`) waits on the same barrier instead of double-loading.

### Added

- **`Server.RoomIdleTimeout` and `Server.MaxResidentRooms`**
  ([#183](https://github.com/reearth/ygo/issues/183)): keep a room resident
  and warm for a bounded time after its last peer leaves (durable flush
  still happens immediately), with an LRU bound on how many idle rooms stay
  resident at once. Both default to zero, preserving the previous
  eager-evict behaviour.

## Install

```
go get github.com/reearth/ygo@v1.39.0
```

See [CHANGELOG.md](https://github.com/reearth/ygo/blob/main/CHANGELOG.md) for full details.

---

## v1.38.0

A minor, additive release adding two independent features. On the awareness
layer, a new `OnUpdate` event channel fires on every applied entry (including
heartbeats) alongside the content-only `OnChange`, plus a `Meta(clientID)`
accessor — matching Yjs `y-protocols`/yrs semantics. On the websocket provider,
opt-in Hocuspocus docName framing and an `OnTokenAuth` hook bring in-band token
authentication for the `@hocuspocus/provider` client ecosystem. No breaking
API changes, though `OnChange` is tightened (see Changed).

### Added

- **Awareness `OnUpdate` + `Meta(clientID)`**
  ([#105](https://github.com/reearth/ygo/issues/105)): `OnUpdate` fires on every
  applied awareness entry including heartbeats, distinct from the content-only
  `OnChange`. `Meta(clientID)` returns per-client `{Clock, LastUpdated}`,
  retained for tombstones to match the reference implementations.
- **Hocuspocus in-band token auth + docName framing**
  ([#104](https://github.com/reearth/ygo/issues/104)): opt-in
  `Server.HocuspocusFraming` reads/writes docName-prefixed frames for real
  `@hocuspocus/provider` interop; `Server.OnTokenAuth` validates the tag-2 Auth
  token and replies `Authenticated`/`PermissionDenied`, closing denied
  connections with WebSocket code `4401`. Composes with `AuthFunc`/`Authorize`;
  it is a handshake reply + optional read-only downgrade, not a
  document-confidentiality gate (use the HTTP-boundary auth for that).

### Changed

- **`OnChange` no longer fires on content-identical re-emits**
  ([#105](https://github.com/reearth/ygo/issues/105)): remote heartbeats (via
  `ApplyUpdate`) and local same-content `SetLocalState` now fire `OnUpdate`
  only. `Heartbeat()` now fires `OnUpdate` (previously silent). A reactivated
  client is classified `Updated` rather than `Added`, matching Yjs/yrs.

## v1.37.0

A minor release hardening the websocket server's coalesced persistence path.
It closes a durability gap where a room could be evicted before its pending
batch was flushed, and gives adapters an optional way to bound stored-version
growth. No breaking changes; the default behaviour of servers without a
`CompactableAdapter` is unchanged.

### Fixed

- **Lost edits on quick refresh with coalesced persistence**
  ([#175](https://github.com/reearth/ygo/issues/175)): the room-teardown paths
  (`handleDisconnect`, `CloseRoom`) now flush the pending coalesced batch
  durably — and await it — while the room is still discoverable, then re-check
  and evict. A peer that reconnects during the flush reuses the live
  in-memory document instead of reloading stale state from the backing store.

### Added

- **`CompactableAdapter` and `Server.CompactEvery`**
  ([#175](https://github.com/reearth/ygo/issues/175)): an optional
  `PersistenceAdapter` extension the server calls on room unload, and — when
  `CompactEvery > 0` — after every N persistence flushes, letting an adapter
  bound stored-version growth. `persistence.LegacyAdapter` implements it via a
  new `KeepVersions` field, forwarding to the existing
  `VersionedPersistence.Compact`.

## Install

```
go get github.com/reearth/ygo@v1.37.0
```

See [CHANGELOG.md](https://github.com/reearth/ygo/blob/main/CHANGELOG.md) for full details.

---

## v1.36.0

A minor release that changes the default persistence behaviour for the
websocket server: backing-store writes are now debounce-coalesced (2s window,
10s max wait) into a single merged `StoreUpdate` per burst, instead of one
write per committed transaction, cutting persistence latency and version
churn under load. This only affects servers with a `PersistenceAdapter`
configured (`NewServerWithPersistence`); plain `NewServer()` is unaffected.
Set `Server.PersistCoalesceWindow = -1` to opt back into the previous strict
per-update behaviour.

### Changed

- **Websocket persistence writes are coalesced by default (behaviour
  change)** ([#175](https://github.com/reearth/ygo/issues/175)): the
  per-room persistence worker debounces writes and merges each burst into a
  single `StoreUpdate` call rather than writing once per update
  (Hocuspocus parity). Only servers with a `PersistenceAdapter` configured
  are affected.

### Added

- **`Server.PersistCoalesceWindow` and `Server.PersistCoalesceMaxWait`**
  ([#175](https://github.com/reearth/ygo/issues/175)): tune or disable
  persistence coalescing. Defaults are 2s and 10s; a negative
  `PersistCoalesceWindow` (e.g. `-1`) disables coalescing and restores strict
  per-update writes.

## Install

```
go get github.com/reearth/ygo@v1.36.0
```

See [CHANGELOG.md](https://github.com/reearth/ygo/blob/main/CHANGELOG.md) for full details.

---

## v1.35.0

A minor release hardening the websocket broadcast path against slow peers. It
fixes head-of-line blocking in the writer and adds an opt-in in-place resync
policy so a transiently-slow peer recovers without reconnect churn. No breaking
changes; the default behaviour is unchanged.

### Fixed

- **Head-of-line blocking in the websocket broadcast writer**
  ([#172](https://github.com/reearth/ygo/issues/172)): the per-peer write mutex
  (`wmu`) was held across the blocking `conn.WriteMessage`, so a single slow or
  stalled peer could block broadcasts to every other peer for up to
  `writeTimeout`, and the queue-overflow branch could never fire while a write
  was in flight. The write path now holds `wmu` only to read the `closed` flag,
  then writes without it.

### Added

- **`SlowPeerResync` policy for graceful slow-peer recovery**
  ([#172](https://github.com/reearth/ygo/issues/172)): new
  `Server.SlowPeerPolicy`. `SlowPeerDisconnect` (default) closes a peer whose
  broadcast queue overflows; `SlowPeerResync` keeps the connection open, drops
  the stale delta, and sends a full-state resync once the queue drains, so the
  peer converges in place without a reconnect.

### Changed

- **Default `PeerWriteQueueSize` bumped 256 → 512**: more slack before a
  transiently-slow peer overflows (matching the yrs broadcast ring of 512);
  override via `Server.PeerWriteQueueSize`.

## Install

```
go get github.com/reearth/ygo@v1.35.0
```

See [CHANGELOG.md](https://github.com/reearth/ygo/blob/main/CHANGELOG.md) for full details.

---

## v1.34.0

A minor release bundling a full mobile on-device editor with an awareness
tombstone-reclamation fix. No breaking changes to the core library.

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
  ([#166](https://github.com/reearth/ygo/issues/166)): removal tombstones (kept so
  a client's clock can still encode removals and reject stale re-adds) were never
  reclaimed, so a high-churn room's entry count grew without bound against
  `SetMaxClients` and could eventually refuse new, legitimate clients.
  `PurgeTombstones(grace)` drops tombstones older than `grace`; `StartAutoExpiry`
  now runs it each tick as a second stage (`RemoveExpired(timeout)` then
  `PurgeTombstones(2*timeout)`).

### Changed

- **Idiomatic Yjs JSON from the mobile read accessors**
  ([#109](https://github.com/reearth/ygo/issues/109)): `Doc.GetTextJSON` now emits
  idiomatic single-op Yjs delta (`[{"insert":"hi","attributes":{...}}]`) and
  `Awareness.StatesJSON` emits `{"<clientID>": <state>}` without the internal
  clock. These reshape two mobile read accessors whose output was pre-stable
  (`GetTextJSON` was explicitly documented as unstable); no core-library change.

## Install

```
go get github.com/reearth/ygo@v1.34.0
```

See [CHANGELOG.md](https://github.com/reearth/ygo/blob/main/CHANGELOG.md) for full details.
