## v1.49.1

**Who is affected: anyone whose documents have deletion history and who loads
them through `ApplyUpdateV2`.** That is most persistence backends, since
`encode_state_as_update_v2` / `encodeStateAsUpdateV2` is the usual on-disk
format. If your documents were written by `yrs` or by `yjs` and have ever had a
nested shared type deleted, ygo has been reading them **incompletely**, with no
error returned.

This is a data-loss fix. Upgrade before your application persists anything it
read back through `ApplyUpdateV2`, because writing a partially-decoded document
back over its source destroys the part that did not decode.

- **`ApplyUpdateV2` silently dropped content on GC structs (#231).** The GC
  branch recorded the collected clock range in the delete set but never
  occupied that range in the client's struct list, so every later struct from
  that client looked like a clock gap and was parked in the pending queue
  permanently — waiting on a predecessor that was already present in the same
  update. Returned `nil` while doing it. Measured on 94 real `yrs`-written
  documents: yjs read 974 nodes, ygo read 535, and 63 of 94 documents were
  wrong. `ApplyUpdateV1` was never affected, and the fix makes V2 follow the
  V1 path exactly.

- **Conformance fixtures now cover GC structs (#232).** None of the 202
  pre-existing V2 fixtures contained one, which is how #231 reached a release.

**If you have already persisted a partially-decoded document:** upgrading does
not undo that. Check your storage for a soft-delete or object-version window
and restore the pre-overwrite copy. A cheap guard worth adding regardless of
this fix: after decoding a full-state update, treat `doc.PendingStats().Items
> 0` as a failed load rather than a smaller document, and refuse to write it
back.

## v1.49.0

**Who is affected:** three separate audiences, so read the one that is you.

- **The `MemoryPersistence` performance fix (#186)** affects only callers who
  explicitly construct `websocket.MemoryPersistence`
  (`websocket.NewMemoryPersistence()`) — **not** every deployment. A fresh
  `websocket.NewServer()` has no persistence adapter at all, and nothing under
  `provider/`, `cmd/`, or `examples/` constructs `MemoryPersistence` outside
  its own tests; you have to opt into it via `NewServerWithPersistence`.
- **The shutdown durability fix (#229)** affects **anyone running
  `provider/websocket` with any persistence adapter at all**, in the default
  configuration. If you call `Server.Shutdown` on a server that has peers
  connected, you were losing committed transactions.
- **The `provider/client` follow-ups (#228)** affect anyone using
  `provider/client`, the embeddable sync client shipped in v1.48.0. One of
  the three is directly visible on the *server* side too: every graceful
  client disconnect was logging as an abnormal closure (code 1006).

**What you must do:** nothing — all three are drop-in fixes with unchanged
constructors and signatures. The write path gets cheaper (see the trade
below), `Shutdown` gets more honest, and `provider/client` disconnects now
look like what they are.

### Fixed: `MemoryPersistence` re-merged the whole document on every write (#186)

**The bug.** `MemoryPersistence.StoreUpdate` ran
`crdt.MergeUpdatesV1(existing, update)` against the room's **entire**
accumulated state on every incremental write — an O(document) cost per write,
so a session of N updates cost O(document²) overall. `PersistenceAdapter`'s
own doc warns adapters not to do exactly this; the built-in adapter was doing
it.

**The fix.** `MemoryPersistence` now delegates to `persistence.MemoryPersistence`
+ `persistence.LegacyAdapter` (`KeepVersions = 1`) instead of re-merging by
hand: each write **appends** the update — O(update), not O(document) — and
the room folds its own backlog into one blob every `CompactEvery` writes
(new field, default 500, matching `provider/client`'s own compaction
default). The O(document) fold still happens, but only once per
`CompactEvery` writes instead of on every single one.

**Measured**, old merge-on-write vs. new append-then-compact, timing a single
`StoreUpdate` against a room already holding N updates:

| Updates already in room | Before | After | Improvement |
|---|---|---|---|
| 100 | 13,975 ns/op | 1,457 ns/op | 9.6× |
| 1,000 | 132,871 ns/op | 1,710 ns/op | 77.7× |
| 10,000 | 1,676,329 ns/op | 4,653 ns/op | 360× |

**Read the acceptance bar honestly, not generously.** #186 asked for "flush
cost no longer grows with doc size." Taken literally, that is **not fully
met**: per-write cost is now `append + O(document)/CompactEvery`, so it still
grows with document size — the growth constant is divided by `CompactEvery`
(500 by default), not eliminated. It *is* met in the sense that actually
matters day to day: cost no longer grows **per write** in proportion to the
document, because most writes are a cheap append and only one in
`CompactEvery` pays the fold. Flat, constant-time writes aren't achievable
for this storage model at all — `LoadDoc` has to keep returning the whole
room as one V1 blob, so any bounded-record scheme must periodically fold the
full document, and that fold is inherently O(document). See
[docs/PERSISTENCE.md](docs/PERSISTENCE.md) for the same framing in the
adapter's own docs.

**The trade, stated plainly:** `LoadDoc` is no longer O(1). It now folds
whatever records the room still holds — bounded by `CompactEvery` — and
persists that fold, so subsequent loads are cheap again until the log builds
back up. Writes are continuous and loads happen once per room residency, so
this is the right direction to trade in, but it is not free.

**Two corrections to the issue, verified against the code, worth knowing if
you read #186 directly:**

1. It calls `MemoryPersistence` "the default adapter." It is not.
   `websocket.NewServer()` is documented as shipping with no persistence at
   all, and the type is opt-in via `NewServerWithPersistence`.
2. Its acceptance criterion points at a benchmark
   (`BenchmarkMemoryPersistence_FlushVsDocSize` in `persistence/`) that
   measures a **different type** (`persistence.MemoryPersistence`) on a
   **different path** (`LoadDoc`, not `StoreUpdate`) than the one this issue
   is actually about. This release adds
   `BenchmarkWSMemoryPersistence_StoreUpdateVsDocSize` in
   `provider/websocket`, which measures the type and path #186 is actually
   about — the numbers above come from it.

### Added: `Compact`, `CompactEvery` (#186)

New exported surface on `MemoryPersistence`, which is why this ships MINOR
rather than PATCH even though it's a bug fix — nothing existing changes
behaviour or signature.

- **`Compact(ctx, room) error`** folds a room's appended records into one
  now, on demand. It also satisfies the optional `CompactableAdapter`
  interface, so `Server.CompactEvery` and the server's on-unload compaction
  work against `MemoryPersistence` too, additively on top of its own
  threshold.
- **`CompactEvery int`** (field) sets the self-compaction threshold; 0 or
  less means the default of 500.

**Deliberately not added:** `StoreUpdateContext`. An in-memory append has
nothing to abort, so implementing the context-aware `PersistenceAdapterContext`
variant would only cost writes — it would newly satisfy that interface and
switch the server's persistence worker onto its cancellable-ctx path, where
the coalescing-disabled path's final shutdown drain reuses a ctx a separate
goroutine cancels concurrently with that same drain. A committed update still
sitting in the queue when that race goes the wrong way is discarded with only
a log line. Measured while this was still in the branch: 51-151 of 200
concurrent writes dropped across 20 trials during a concurrent `Shutdown`,
attributable to this method.

That figure is **not** a claim that removing it makes such a `Shutdown` race
lossless. A separate, pre-existing gap in the coalescing-disabled shutdown
drain — it drains its queue exactly once and exits, whatever the adapter —
drops writes under this same repro shape regardless of which
`PersistenceAdapter` is behind it. What this removal fixes is specifically the
extra, compounding loss this method caused on top of that. The underlying gap
was filed as #229 and is fixed separately in this same release — next.

### Fixed: transactions committed during `Shutdown` were silently dropped (#229)

**This one affects everybody with persistence configured, in the default
configuration**, and is unrelated to `MemoryPersistence`. It was found during
the whole-branch review of #186 and verified as pre-existing against v1.48.0.

**The bug.** Each room's persistence worker exited on `Server.Shutdown` by
draining its 256-slot queue **once** and returning. Its producer — the
`doc.OnUpdate` observer — watched only the room-teardown signal, never the
server's shutdown signal. And `Shutdown` closes the shutdown signal as its
*first* act but the peer connections much later, so peer read loops keep
committing for the entire close-connections-and-join window. Every
transaction committed after that one-shot sweep went into a buffer nobody was
reading, and disappeared — no error, no counter, not even a log line. The
package's own doc comment promised the opposite: *"draining any buffered
updates before returning so that no committed transaction is silently lost."*

This is the same shape as the relay-side [#202](https://github.com/reearth/ygo/issues/202)
fixed in v1.46.0: `Shutdown` winding a consumer down while producers were
still producing.

**The fix — and why it is not the obvious one.** The tempting shape is to
quiesce the producers first and let the worker exit once they are provably
gone. We deliberately did not do that. Producers here are peer read loops with
no join point, `ServeHTTP` handlers that can register mid-shutdown, and any
caller holding a `*crdt.Doc`; "provably gone" is not provable, and a wrong
guess there turns silent data loss into a **hung `Shutdown`** — a strictly
worse failure. `Shutdown`'s ordering is therefore completely unchanged.

Instead the handoff itself was made race-free. The worker now closes a new
per-room retirement latch as the **first** act of every exit path — before its
final drain, not after — which leaves the producer exactly two cases and no
third:

1. the send completed while the latch was open, so `close(latch)` and
   therefore the final drain that follows it both happen after the send: the
   drain is guaranteed to pick the update up;
2. the latch is already closed, which a `select` on a closed channel always
   observes with certainty: the committing goroutine performs the write
   itself.

The producer's escape hatch (needed so a full buffer can't wedge a committing
transaction once the worker is gone) still exists — it now has a durable
destination instead of the floor.

**Also fixed, the second half of #229:** the coalescing-disabled path passed
the worker's **cancellable** context — the one a sibling goroutine cancels on
shutdown — to its final drain, so every `PersistenceAdapterContext`
implementation (`persistence/sqlite` via `NewLegacyAdapterContext`, and any
adapter that honours `ctx`) returned `ctx.Err()` and discarded the write.
Measured at 51-151 of 200 concurrent writes dropped per trial. Both paths'
final flushes now use `context.Background()`, and a store aborted by
cancellation is **retained** and re-stored on the exit path rather than
dropped — the same retain-and-re-flush rule the coalescing path already
applied to an unflushed batch. Cancellation still aborts a write already in
flight when shutdown begins, so it no longer decides which committed
transactions count.

**Read that last point precisely — it is a trade, not a free win.** Because the
final flush and the retained re-stores now run under `context.Background()`, a
`StoreUpdate` that never returns wedges that room's worker at exit where it
used to be cancelled out of the way. `Shutdown` does not hang on it — every
wait is bounded by the caller's context — but it now returns `ctx.Err()` in a
case that previously returned `nil` by discarding your data. If you have a
`Shutdown` deadline and an adapter that can block indefinitely, you may start
seeing `context.DeadlineExceeded` where you saw success before. That is the
honest signal replacing a silent lie.

There is a **second, unrelated cause** of that same error, so do not read it as
"my adapter is wedged": `Shutdown`'s new wait counts *every* committing
goroutine, not only the ones performing a write themselves. Code that holds a
retained `*crdt.Doc` and commits in a tight loop keeps that count above zero for
as long as it runs, so `Shutdown` burns its whole deadline with nothing stuck
anywhere. Stop your writers, not just your connections, if you want `Shutdown`
to return early.

**`Shutdown` now joins the writes it can see.** A transaction committed during
`Shutdown` may arrive too late for its room's worker, in which case the
committing goroutine performs the adapter write itself. `Shutdown` waits for
those to finish (bounded by your ctx) before returning — otherwise the fix
would only narrow the loss window, since the usual `srv.Shutdown(ctx)` followed
by returning from `main` would kill the process mid-write.

**The residual, stated plainly: this does not make `Shutdown` lossless, and
cannot.** A transaction that *begins* committing after `Shutdown` has observed
its last in-flight write is not covered. The producers are peer read loops and
any code holding a `*crdt.Doc`; the server has no way to join them, and
inventing one would risk a `Shutdown` that never returns — a worse failure than
the one being fixed.

What is guaranteed is narrower, and every one of its qualifiers is real:

> **Stop accepting new WebSocket connections before calling `Shutdown`.** Then,
> provided `Shutdown` returns `nil`: for every room that was present and had
> finished loading when `Shutdown` began, any commit whose `Transact` returned
> before `Shutdown` returned has been handed to the adapter — the write
> attempt completed and was not abandoned mid-flight. Whether the adapter
> *accepted* it is a separate question this does not answer: adapter errors
> and panics are logged, not propagated, so this is not an unconditional
> durability claim.

The precondition is not decoration. `Shutdown` snapshots the room set once and
skips any room still mid-load at that instant, and `ServeHTTP` has no shutdown
gate — so a connection accepted while `Shutdown` runs can create a room, or
finish loading one, after the snapshot, and `Shutdown` never waits on that
room's persistence worker. A commit into it can return from `Transact` with the
update merely buffered, and `Shutdown(ctx)`-then-exit kills the drain. That
behaviour predates v1.49.0; it is called out here because everything above
would otherwise read as promising against it.

Nor is "provided `Shutdown` returns `nil`" decoration. Every wait inside
`Shutdown` is bounded by the caller's `ctx`, not by the work actually
finishing — a deadline that fires mid-drain returns `ctx.Err()` while a final
flush or a stranded write may still be in flight, landing or not with no way
for the caller to tell. And "handed to the adapter" is exactly that, no
more: an adapter error or a recovered panic while storing a commit is logged
(see `persistStranded`'s and the persistence worker's own doc comments) and
the write abandoned right there, never propagated through `Shutdown`'s return
value. A `nil` `Shutdown` tells you every in-scope commit's write attempt
completed without being abandoned mid-flight — it cannot tell you the
adapter accepted any of them. A caller who needs that stronger property has
to get it from the adapter itself.

**What this costs you.** A straggler write is synchronous for the goroutine
that committed it and bypasses coalescing, auto-versioning, and compaction. It
also delays that update's relay fan-out, since the persistence observer runs
before the relay observers. And it writes for a room the server may already
consider gone — including, if you retain a `*crdt.Doc` past teardown,
indefinitely; before #229 those commits were silently dropped instead. Adapters
whose `Compact` mutates state keyed by room name should serialise by name; see
`CompactableAdapter`'s godoc and [docs/PERSISTENCE.md](docs/PERSISTENCE.md).

**Tested both directions**, because the failure mode of a bad fix here is a
hang rather than a loss. Sequenced (not raced) regression tests commit while
the worker is parked immediately past its final drain and assert the update
reaches the adapter, with coalescing enabled *and* disabled; a second pair
parks the adapter inside the stranded write itself and asserts `Shutdown` has
not returned, then that a wedged one still surfaces as the caller's deadline.
Against them: `Shutdown` must still return promptly for rooms with no peers at
all — idle-resident and `Apply`-created — where no producer will ever appear.

### Fixed: `provider/client` follow-ups from the #165 review rounds (#228)

**Who is affected:** anyone using `provider/client` (v1.48.0's embeddable
sync client). Three independent, previously-deferred fixes, none touching a
public signature — the most visible one is server-side: every graceful
`provider/client` disconnect was logging as an abnormal closure (code 1006)
on any server it talked to, ygo's own included.

**`Client.Close`, called a second time, always returned `nil`.** `closeErr`
was a variable local to `Close`, re-declared fresh on every call — including
repeat calls, where `closeOnce.Do` is a no-op that never touches it — so the
first call's real result (whatever the owned store's `Close` actually
returned) was silently replaced by a fresh, untouched `nil` on every call
after the first. It is now cached on the `Client` itself and returned
consistently, first call or fifth.

**`Client.maybeCompact` could lose a concurrent write count.** It read
`storeWrites` via `Load` and then reset it unconditionally via `Store(0)`,
discarding any `Add(1)` from a concurrent `onDocUpdate` — a local edit
landing in that window, from either the caller's own goroutine or the loop
goroutine — that happened to arrive between the two calls. It now consumes
exactly its threshold via `Add(-threshold)`, which cannot lose a concurrent
increment on either side of it. The impact was compaction cadence drift — a
few extra updates before the next fold — never lost data.

**`Close`'s teardown never sent a WebSocket close frame.** Tearing down via
a bare `conn.Close()` tells the peer nothing: from `ReadMessage`'s point of
view, a deliberate disconnect is indistinguishable from a crash or a severed
network path, and ygo's own server (like most implementations of the
ordinary WebSocket close handshake) logs that as an abnormal closure — on
every ordinary disconnect, not only real failures. `runLoop`'s teardown now
sends `WriteControl(CloseMessage, CloseNormalClosure)` before `conn.Close()`,
the same thing a graceful `gorilla/websocket` client is documented to do,
so the peer's read loop sees a proper close instead of a bare I/O error.
Two things worth knowing precisely, not generously: that frame goes out on
**every** exit from `runLoop`, including a rejected-auth or read-error exit,
always carrying the same `CloseNormalClosure` code — so read it as "this
client is done writing," not as a claim that the disconnect was clean. And
the frame's own write is bounded by `closeDrainTimeout` (2s), not the
10s handshake-path timeout the first version of this fix used by mistake —
a backpressured-but-alive peer could otherwise have added up to 10 seconds
to `Close`'s return latency, right where the design intent for this
teardown path was already 2 seconds.

### Documentation drift, fixed alongside

`docs/ARCHITECTURE.md`'s package dependency graph had not been redrawn since
v1.19: it showed only `provider/{websocket,http}` and omitted five packages
shipped since — `persistence/`, `cluster/`, `mobile/`, `provider/webhook`,
and `provider/client`. Every arrow in the replacement was verified against
real imports with `go list` across 14 packages, which confirmed one arrow
most readers would draw wrong: `awareness/` does **not** import `crdt/`.

Three further drifts were found while auditing the rest of that file, and are
fixed here rather than filed:

- **"Garbage collection" documented an API that does not exist.** It told you
  to write `doc.GC = true` / `doc.GC = false`. `Doc` has no exported `GC`
  field — the flag is set at construction, `crdt.New(crdt.WithGC(false))`.
  Code copied from that section would not have compiled.
- **The same section described only half the behaviour.** Automatic
  collection is **suspended for as long as any `UndoManager` is registered**
  (undoing a deletion re-inserts a copy of the deleted content, so that
  content has to still be there) — worth knowing, because it means a
  long-lived undo manager keeps deleted content alive. And `crdt.RunGC` does
  tombstone reclamation (#166): it replaces deleted content with
  `ContentDeleted` tombstones and then merges adjacent tombstones from the
  same client into single nodes. It stays available as the manual entry point
  when auto-GC is suspended, and it is destructive with respect to
  `RestoreDocument` — take snapshots first.
- **"Compatibility testing" described only the Yjs fixture layer**, omitting
  the randomised layer under `testutil/fuzz/` entirely — including
  `TestFuzzConvergenceMoves`, the oracle that found the `YArray.Move`
  divergence fixed in v1.40.0. All four oracles are now listed with what each
  proves, which require node, and how to replay a failing seed.

Finally, `mobile`'s package doc understated its own bound surface: it named
only `*Doc` and `*Awareness`, omitting `*SyncClient` and `*Subscription`, and
said the package never exposes callbacks. It exposes three observer
*interfaces* (`DocObserver`, `AwarenessObserver`, `SyncStatusObserver`) —
the only callback form `gomobile bind` supports in that direction. The claim
now says what it meant (no Go **func values**) and points at the threading
rules each observer imposes.

## v1.48.0

**Who is affected:** anyone building a Go client — or, via `mobile.SyncClient`,
a native iOS/Android app — that needs a Yjs document synced to a
`provider/websocket`-compatible server. Nothing existing changes: this
release only adds packages.

### Added: `provider/client`, an embeddable offline-first sync client (#165)

Until now, ygo's networked sync story was server-only: `provider/websocket`
answers peers, but embedding a *client* that dials it meant hand-rolling the
WebSocket connection, the sync handshake, reconnect-with-backoff, and local
durability yourself. `provider/client` closes that gap — this is the same
project's own competitive comparison against Deln0r/ygo naming
"embeddable offline-first client" as a gap the rival covered and we didn't;
it no longer is.

**The offline model, concretely — because "offline-first" gets read as
vaguer than it is.** There is no offline-op queue anywhere in this package,
and that's deliberate, not a missing feature. The y-protocol handshake
already is one: SyncStep1 is a peer's state vector ("here's what I have"),
SyncStep2 is the answer ("here's what you're missing"). An edit made while
disconnected just sits in the `*crdt.Doc` — Yjs updates are ordinary
document state — until the next connection's handshake runs, and that
handshake doesn't care how long the edit has been waiting. A client that
reconnects successfully converges with no replay mechanism required.

That leaves exactly one gap the protocol can't cover: the *process* itself
going away while still offline — killed, reaped, the device sleeps mid-edit.
Nothing about the sync protocol writes a `*crdt.Doc` to disk, so without a
local store, a restart during an offline stretch loses every edit made since
the last successful sync. `provider/client`'s local store
(`Options.Store`/`StorePath`, a CGo-free SQLite adapter by default) exists
for exactly that: every update — the caller's own edits **and** everything
ever received from the server — is persisted as it happens, and hydrated
back into a fresh `Doc` *before* the app can touch it or a dial is even
attempted (`client.New` → `client.Connect`). Call `New`, start editing
immediately, and get back everything from last time even with the server
unreachable or down — hydration never waits on the network.

**Reconnect and keepalive.** `Connect` never gives up on a connection
failure (except one case below); it reports it via `OnStatus` and retries
with jittered exponential backoff (`Options.MaxBackoff`, default 30s), reset
only when a handshake actually *completes* — not merely when a dial
succeeds, so a server that accepts and immediately drops a connection
doesn't turn into a half-second retry storm. A WebSocket ping every
`Options.PingInterval` (default 30s, matching `provider/websocket`'s own
default) plus a 2×-interval silence deadline converts a half-open
connection — the peer vanished without a clean close — into an ordinary
retryable error instead of a socket that blocks forever.

**Auth, and the one caveat worth reading before you wire it up.**
`Options.Token` sends ygo's Hocuspocus in-band auth token
(`provider/websocket`'s `OnTokenAuth` counterpart); a rejection is terminal
(`ErrAuthRejected`), not retried. But it is **not a confidentiality gate**:
ygo's own server pushes the room's full SyncStep1/SyncStep2/Awareness state
before it has read the client's token at all, so `Synced()` can close — with
real document content already in the `Doc` — on a connection whose token is
rejected moments later. If withholding content from an unauthenticated
caller matters, that has to happen at the HTTP boundary
(`provider/websocket.Server`'s `AuthFunc`/`Authorize`), not at this in-band
exchange.

**`Stats().Dropped` is durability-based, not connection-based** — this was
deliberately gotten exact, not just plausible: a document update backed by
a configured `Store` is never counted, because the store already has it and
the next hydrate+handshake (this client's own reconnect, or a brand-new
`Client` after a restart) still delivers it — that's a delay, not a loss.
A storeless document update, or any awareness (presence) update — Store or
no Store, since presence isn't document state and the handshake never
carries it — *is* counted. Alert on `Dropped` going non-zero with a `Store`
configured exactly as you would `RelayStats.Dropped` server-side.

`Client.Close` mirrors the durability-first / bounded-drain / then-teardown
discipline `Server.Shutdown` established for #202: store writes are already
durable by the time `Close` starts (they happen synchronously, on the
caller's own goroutine, not on a buffered worker), the sync loop is joined,
observers are unsubscribed, and whatever is still queued outbound is drained
and counted rather than silently discarded.

See [docs/CLIENT.md](docs/CLIENT.md) for the full design and
[examples/offline-client](examples/offline-client) for a runnable,
flag-driven demo.

### Added: `mobile.SyncClient`, the `gomobile` binding (#165)

`mobile/` has shipped a full on-device Yjs editor since v1.34.0 — text,
array, map mutation, observers, presence — but making it talk to a server
meant writing the dial/reconnect/persistence logic again on the platform
side, in Swift or Kotlin, by hand. `SyncClient` wraps `provider/client` in
the same gomobile-safe surface the rest of `mobile/` uses:
`NewSyncClient(url, dbPath, token)` returns a client whose `Doc()` is usable
immediately, and `Connect()` returns right away — the blocking sync loop
runs on its own goroutine, off the platform UI thread, with progress
delivered through a `SyncStatusObserver` instead of a return value.
`SyncedOnce()` gives platform code a poll-friendly boolean where
`client.Client.Synced()`'s channel can't cross the binding boundary. Every
behaviour above — the offline model, the auth caveat, `Stats`-style
accounting, reconnect/backoff — applies unchanged; this is a thin
translation layer over `provider/client`, not a second implementation. See
[mobile/README.md](mobile/README.md#self-syncing-syncclient).

No API changes to anything existing. Both additions are new exported
symbols in new packages, which is why this ships as a MINOR release under
this project's semver-by-API-surface convention even though nothing already
shipped changes behaviour.

### Fixed: a disconnect-triggered awareness removal could suppress a rejoining client's presence (#226)

**Who is affected:** anyone running `provider/websocket` with awareness
enabled — this is a **server-side behaviour change** affecting existing
deployments, not just new code from this release. Any room where clients
disconnect and quickly reconnect (a page refresh, a flaky connection, a
`provider/client` reconnect) was exposed.

**The bug.** When a peer disconnected, `peer.go`'s `encodeAwarenessRemoval`
synthesised that peer's removal at its current awareness clock **plus
one**. A client that reconnects and re-announces its presence calls
`Awareness.Heartbeat`, which *also* bumps by exactly one from that same base
clock — so a prompt reconnect computed the identical clock as the server's
removal. `Awareness.ApplyUpdate`'s equal-clock rule always resolves a tie in
favor of the null (removed) side over an active one, no matter which a
given peer receives first, so every other peer in the room could end up
believing the rejoining client had left — even though it was back and
correctly announcing itself. In the worst case (a half-open connection that
`AwarenessExpiry` exists to catch), the server could synthesise the removal
well after the client had already reconnected, at a clock *higher* than the
rejoin, which no client-side workaround could fully cover.

**The fix.** `encodeAwarenessRemoval` no longer bumps the clock: it encodes
the removal at exactly the clock the room's shared `Awareness` currently
holds for that client. The existing equal-clock rule already admits an
unbumped removal, and leaving it unbumped means any subsequent genuine
heartbeat from the rejoining client is strictly newer than the removal, so
the tie class this bug depended on no longer arises. This also brings ygo
in line with y-protocols' `removeAwarenessStates`, which bumps the clock
only when the removed client is the awareness instance's own local client —
never when synthesising a removal on another client's behalf, which is
exactly this function's case. `provider/client`'s existing double-heartbeat
margin on reconnect (added for #165) is unaffected and remains in place —
it now exists purely as defense-in-depth against third-party servers
(Hocuspocus, y-websocket, or any other implementation of this wire
protocol) that may still compute removal clocks the old way.

## v1.47.1

**Who is affected:** anyone building nested documents with the prelim
constructors (`NewMapPrelim`/`NewArrayPrelim`/`NewTextPrelim`, v1.43.0) —
specifically code that, by bug or by design, hands one detached handle to two
containers.

**The fix.** A shared type attaches once, but the staging entry points only
enforced that against *attached* handles and (since v1.46.0) duplicates on
the *same array*. Staging one handle onto two different containers — two
detached arrays, a detached array and a detached map, or two keys of one
detached map — succeeded at both calls, and the mistake only surfaced when
the second container attached: a panic from inside `flushPrelim` complaining
that `PushType` requires a detached type, pointing at a function the caller
may never have used, with no hint which container double-staged the handle.

`PushType`, `InsertType`, and `YMap.Set` now track where a detached handle is
staged and reject a spoken-for handle **at the call site**, naming the entry
point you actually called. The claim is released when the handle leaves its
container — overwriting the staged key, `YMap.Delete`, or a staged
`YArray.Delete` — so the legitimate "move it" flow (delete there, stage here)
works, and overwriting a staged key with the same handle remains the
documented no-op.

No API changes; misuse that previously failed late and confusingly now fails
immediately and precisely (#222).

## v1.47.0

### Added: `ygo-server` flags for periodic version capture and retention (#167)

**Who is affected:** anyone running the `ygo-server` binary who wants a version
history without writing one. The library is unchanged; this release only wires
existing capability to the command line.

**Two new flags.**

```
-version-interval duration   capture a version of each CHANGED room at most this often
-keep-snapshots int          retain this many auto-captured versions per room
```

Both default to off, so a default run behaves exactly as before.

`-version-interval 15m` captures at most one version per room per 15 minutes,
and only for rooms that actually changed — a quiet room is never re-versioned,
which is what keeps a history panel usable rather than full of identical
entries.

`-keep-snapshots 50` bounds the auto-captured versions. Retention is **per
label**, so it trims only the server's own `auto` versions and cannot evict a
snapshot your application named deliberately (say `before-migration`). That
guarantee is v1.45.0's label-scoped retention; without it this flag would have
been a footgun.

Setting `-keep-snapshots` without `-version-interval` warns at startup:
retention is applied at the moment a version is captured, so with nothing
capturing, the bound is never enforced.

**Also:** `-awareness-expiry` and `-max-awareness-clients` were missing from the
binary's documented flag list and are now included, with a test that keeps the
list from drifting again.

### Fixed: zero-size origin tokens alias in `WithTrackedOrigins` (#203)

**Who is affected:** anyone using `UndoManager` with `WithTrackedOrigins`,
and more broadly anyone minting transaction-origin tokens as pointers.

**The fix.** Tracked-origin matching is Go interface equality (`==`), and Go
satisfies every zero-size allocation from a single address
(`runtime.zerobase`) — so two origin tokens minted as `new(struct{})` compare
equal even though the code plainly intends two distinct identities. An
UndoManager tracking one such token silently captured the other's
transactions too: user B's edits landing on user A's undo stack, with no
error anywhere. This is the same aliasing that silently disabled relay
publishing inside `provider/websocket` for six releases (fixed in v1.42.0);
this occurrence sat on a public API where the library cannot fix the values
for you (#203).

**What changes for you.** `WithTrackedOrigins` now panics at construction
when given a pointer to a zero-size type, naming the offending type and the
fix. If you hit the panic, change your token type from `struct{}` to
something with size — the conventional spelling is:

```go
type originToken struct{ _ byte }
tok := &originToken{} // every allocation is now a distinct origin
```

Distinct named zero-size *value* types (`originA{}` vs `originB{}`) remain
legal and correct — interface equality compares the dynamic type first, so
they never alias each other. Ordinary comparable values (strings, ints) are
unaffected. The identity semantics are now documented on
`WithTrackedOrigins`, `Doc.Transact`, and `Doc.OnUpdate`.

No API surface changes; strictly a misuse guard plus documentation.

## v1.46.0

Two changes. `YArray.InsertType` completes the prelim constructor surface
from v1.43.0, and `Server.Shutdown` no longer silently discards queued
outbound relay updates (#202).

### Added

- **`YArray.InsertType(txn, index, type)`** places a detached shared type at
  any index; `PushType` could attach one only at the end. The gap mattered
  because the obvious workaround — `PushType` then `Move` — emits
  `ContentMove`, a ygo wire extension other implementations mis-parse,
  usually silently (#207), so mid-array placement of a nested type had no
  safe expression. Attached placement mirrors
  `Insert`: live-index semantics, splitting a plain-value run when the index
  falls inside it, and any unresolvable index anchoring at the tail. On a
  detached array the type splices into the staged content, and plain-value runs
  split around it when the array attaches. Two conformance fixtures pin the
  wire shape byte-identical to `yjs@13.6.30` — a nested type inserted between
  two attached cells, and an interior insert into a detached array's staged
  run — and a parity suite holds staged boundary behaviour identical to
  attached for interior, ends, beyond-the-end and negative indices. The
  rejection message for a shared type passed to `Insert`/`Push` as a plain
  value now points at `InsertType` first.

### Fixed: shutdown relay-tail loss (#202)

**Who is affected:** every clustered deployment — any server with a
`cluster.Relay` attached — plus authors of third-party `Relay`
implementations.

**The fix.** `Server.Shutdown` cancelled the relay context as its second act,
before closing peer connections and before the persistence drain. Peers kept
committing for the rest of shutdown — potentially seconds — into per-room
outbound lanes whose workers had already exited without draining, and nothing
joined those workers. Two consequences, both fixed (#202):

1. **Silent, uncounted tail loss.** Every update queued in that window was
   discarded with `RelayStats().Dropped` and `HardDrops` both reading zero,
   so an operator alerting on those counters concluded nothing was lost. On a
   hot room — never reloaded from persistence — the peer node never converged
   on those updates. `Shutdown` now drains each lane while publishing still
   works and cancels the relay context last; whatever it cannot deliver
   within your ctx budget is counted in `Dropped`. There is no longer any
   path where updates vanish while both counters read zero — v1.42.0's
   "`Dropped`/`HardDrops` should always be zero; alert on their presence"
   operator model now holds across `Shutdown` too, and the exception
   documented there is retired.

2. **`Publish` could outlive `Shutdown`.** Nothing joined the lane workers,
   so a `relay.Publish` call could still be in flight after `Shutdown`
   returned — unsafe for a relay that frees resources in `Close()`, despite
   the documented rule that the caller `Close()`s the relay once every
   attached server is done. `Shutdown` now joins the workers, bounded by its
   ctx.

**What changes for you.** Give `Shutdown` a real deadline: its ctx is now
also the delivery budget for the final outbound tail, and a `Shutdown` that
could not finish within it returns the ctx error with the undelivered backlog
counted in `Dropped`. `RelayStats.Dropped` is broader — it now also counts
payloads lost to a failed `relay.Publish` call at any time, not just pre-lane
discards — so a deployment that graphs it may see non-zero values it
previously missed; they were always losses, just invisible ones. Inbound
shutdown ordering is unchanged in effect: `Inject` has always refused with
`ErrServerShutdown` from the moment `Shutdown` begins, so cancelling the
relay context later cannot let remote changes mutate rooms.

**For `Relay` implementers:** the `Publish` contract now states explicitly
that it must return promptly once its ctx is cancelled — `Shutdown` relies on
this to unwedge a blocked `Publish` after your deadline. Both shipped relays
already conform.

## v1.45.0

**Who is affected:** anyone running `Server.AutoVersionEvery` together with
`LegacyAdapter.KeepSnapshots > 0` while also letting users name snapshots — and
anyone maintaining their own `SnapshotStore` implementation.

**The fix.** Snapshot retention kept the newest `KeepSnapshots` per room without
regard to how each snapshot was created, so auto-captured versions evicted ones
a user had deliberately named. A snapshot named `"before-migration"` vanished
once `KeepSnapshots` newer auto versions existed: at a 15-minute interval with
keep-50, about half a day of continuous editing, silently and with no way to
protect it. Auto versions are cheap, numerous and individually disposable, which
is the whole point of bounding them; a named snapshot is an explicit user act and
the thing they would most object to losing. Retention is now scoped to the label
class, so the two kinds bound themselves separately.

**What changes for you.** `KeepSnapshots` becomes a per-label bound rather than a
per-room one, so a room's total is now `distinct labels × KeepSnapshots` and a
deployment using varied labels will retain more than before. If you relied on it
as a hard per-room cap you no longer have one — deliberately, because capping the
total is what evicts named snapshots. If your application lets end users name
snapshots, that growth is user-driven: enumerate labels from `ListSnapshots` and
call the new `TrimSnapshots` per label if you need a ceiling.

**New:** `LegacyAdapter.TrimSnapshots(ctx, room, label) (int, error)` runs the
same retention on demand. Snapshots written straight through
`SnapshotStore.SaveSnapshot` were never trimmed — retention only ever ran from
`SaveVersion`, so with auto-versioning off `KeepSnapshots` did nothing at all —
and they still are not unless you call this. It returns how many it deleted, and
attempts every surplus snapshot even when one delete fails rather than stopping
at the first.

**If you implement `SnapshotStore`:** the conformance suite is stricter, and your
implementation may newly fail it. That is the intent. It used to pass the literal
`"room"` everywhere, so nothing checked a name needing escaping on the snapshot
path — and that path has more name-derived surface than the update path: the
per-room counter object embeds the room name, and IDs are often recovered by
parsing them back out of an object name. A room name carrying your delimiter
therefore corrupts ID handling silently rather than failing, and a wrong ID is
not cosmetic, since IDs address which state a user restores. This is not
hypothetical — we hit it downstream while implementing `SnapshotStore` on GCS,
where an id-parsing scheme that looked correct mapped some ids onto others.

The suite now runs its core round-trip under `"with/slash"`, `"with:colon"`,
`"with space"`, precomposed *and* decomposed Unicode, and `"../escape"`, and
checks cross-room isolation for name pairs that collide under naive encoding
(`a/b` vs `a:b`, `a/b` vs `a%2Fb`, `a b` vs `a+b`, NFC vs NFD). We verified the
suite against a deliberately-colliding store that rewrites awkward characters in
its storage key: the previous suite passed it clean, the current one fails it on
three of the four pairs. The in-tree memory, file, and sqlite backends pass
unchanged.

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
CI-gating benchmark, `BenchmarkEncodeStateAsUpdateV1`, measured **+7.45%**
(n=10, quiet machine). This benchmark's result is noticeably sensitive to
what else is running on the machine at the time — repeated n=10 runs have
landed anywhere from roughly flat to the high single digits depending on
background load — so +7.45% should be read as a representative order of
magnitude rather than a precise constant; treat it as "mid-single-digit to
high-single-digit percent," not a number good to two decimal places. That
benchmark is also a worst case rather than a typical one: `buildTextDoc(1000)`
performs 1000 one-character inserts, each in its own transaction, so the
encoded document is 1000 items each holding a one-byte string — maximum
validation call count, with no string length at all to amortize each call's
fixed overhead over. `BenchmarkEncodeStateAsUpdateV1_Bulk`, also committed,
encodes the same total byte count built as a single bulk insert instead of
1000 tiny ones — the shape most real documents actually have — and measured
**+2.86%** (n=10). Allocations are unchanged in both cases. `ApplyUpdateV1`
(decode), the heavier path at roughly 115µs, moves only about +1.2%, since
the decode side already validated UTF-8 before this release. This is not
rounded down to "negligible": it is a real, measured cost, worst on documents
built from many tiny string items.

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
