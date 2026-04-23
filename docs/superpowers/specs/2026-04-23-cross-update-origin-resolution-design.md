# Cross-update Origin resolution (pending structs) — design

**Issue:** [reearth/ygo#11](https://github.com/reearth/ygo/issues/11)
**Branch:** `chore/cross-update-origin-resolution`
**Target release:** `v1.2.0` (minor — user-observable behavior change)
**Status:** Draft — awaiting user review

## Background

`ApplyUpdateV1` today has a two-phase pending loop (`crdt/update.go:440-568`)
that retries items whose parent is unresolved **within the same update**.
Items whose dependencies live in a **future update** fall through to the
fallback at `crdt/update.go:556-565`, which stores them in the struct
store *without linked-list integration* and never retries them.

The bug: when a peer receives independent delta updates from concurrent
producers (delta A produces items; delta B produces items that reference
A's items as `Origin`/`OriginRight`), and B arrives before A, B's items
become permanent orphans. A arrives later, A's items integrate fine, but
B's orphans stay dangling forever.

Symptoms: `YMap.Get` / `YArray.Get` / etc. return absent values for
logically-present keys; peers see permanent convergence gaps that only
a forced sync step 1/2 exchange can repair.

A second, adjacent bug shares the same fix surface: **same-client clock
gaps**. If a peer receives clocks 4 and 5 from client X without clock 3,
the current code at `crdt/update.go:486-521` computes `offset=0`, calls
`integrate()` with a `nil` origin lookup, and silently inserts the items
at the head of the parent list — incorrect placement.

Both upstream implementations (Yjs JS and `yrs` Rust) solve this with a
"pending structs" machinery. This spec ports the same mechanism to Go,
adapting the retry loop to Go's non-reentrant mutex.

## Goals

- Items whose Origin, OriginRight, or Parent references refer to clocks
  not yet integrated are parked in a doc-level pending queue rather than
  silently orphaned or misplaced.
- Same-client clock gaps are detected and parked.
- The parked items are automatically retried at the end of every
  subsequent `ApplyUpdateV1` that could potentially satisfy their
  dependencies.
- Delete-set entries targeting not-yet-integrated items are parked in a
  separate pending delete-set and retried similarly.
- `StateVector()` continues to report only integrated clocks (critical
  for re-send-based recovery via upstream peers — unchanged from today).

## Non-goals

- **Including pending bytes in `EncodeStateAsUpdateV1` output.** Yjs JS
  appends pending update bytes to `encodeStateAsUpdate` so peer C can
  propagate what peer A has received but not yet integrated. Convergence
  is still correct without this — state-vector gaps trigger re-send from
  the original source. Pure bandwidth optimization; file as a follow-up
  if demand emerges.
- **Memory cap on pending queue.** Both upstream implementations are
  unbounded. Matches their initial posture; add a metric and optional
  cap in a later release if adversarial patterns emerge.
- **Rollback on pending-queue-inability-to-resolve.** Matches upstream —
  pending items sit indefinitely until their dependencies arrive or the
  process restarts.
- **ContentMove / WeakLink reference handling.** yrs' `missing()` also
  checks these content types. ygo's `ContentMove` does not use them in
  the same way; the existing integration path for `ContentMove` is out
  of scope for this PR.

## Reference implementations

Summary from prior research (see session record dated 2026-04-23):

| Aspect | Yjs JS | yrs (Rust) | ygo decision |
|---|---|---|---|
| Pending queue location | `StructStore.pendingStructs` | `Store.pending` | `structStore.pending` |
| Pending storage format | Raw bytes (re-encode + re-decode on retry) | Decoded `Box<Item>` | Decoded `[]*Item` (yrs model) |
| Delete-set pending | `pendingDs: Uint8Array` (separate) | `pending_ds: Option<DeleteSet>` (separate) | `pendingDs DeleteSet` (separate) |
| `missing` tracking | `Map<clientID, clock>` | `StateVector` | `StateVector` |
| Retry mechanism | Recursive `applyUpdate` at end of integrate | Recursive `apply_update` at end of integrate | **Inline** (Go mutex is not reentrant; semantically equivalent) |
| Same-client clock gap | Park via `struct.id.clock > store.getClock(client)` check | Park via separate path with `missing_sv.set_min(id.client, id.clock - 1)` | Park via explicit gap check (matches yrs pattern) |
| SV computation | Integrated-only | Integrated-only | Integrated-only (already correct) |
| Include pending in encode | Yes | Yes | **No** (follow-up) |
| Memory cap | None | None | None (follow-up) |

## Design

### Data structures

In `crdt/store.go`, add two fields to `structStore`:

```go
type structStore struct {
    clients map[ClientID][]*Item
    // ... existing fields ...

    // pending holds items whose Origin / OriginRight / Parent references
    // clocks not yet integrated, and items that form a same-client clock
    // gap with the integrated state. Retried at the end of every
    // ApplyUpdateV1 that could potentially satisfy their dependencies.
    pending *pendingUpdate

    // pendingDs holds delete-set entries targeting items not yet
    // integrated. Merged with each incoming update's unresolvable
    // delete-set entries and retried alongside pending.
    pendingDs DeleteSet
}

// pendingUpdate holds parked items plus the minimum per-client clocks
// that must be reached before a retry is worth attempting.
type pendingUpdate struct {
    items   []*Item     // parked items, in arrival order; retried as a batch
    missing StateVector // clientID -> min clock that must arrive to potentially satisfy some parked item
}
```

Flat `[]*Item` rather than per-client grouping. Simpler; retry iterates all
items and checks each against current store clocks. Optimization to
per-client `VecDeque` (matching yrs) can come later if profiling warrants.

### When items are parked (three triggers)

At item-decode time inside `applyV1Txn`, after the existing "skip /
partially-integrated / GC-orphan" branches but before `integrate()` is
called:

1. **Cross-client Origin / OriginRight gap.** If `item.Origin != nil`
   and `item.Origin.Client != item.ID.Client` and
   `item.Origin.Clock >= store.Clock(item.Origin.Client)` — park. Same
   check for `item.OriginRight`.

2. **Parent ID gap.** If `item.Parent == nil` (remote representation,
   parent unresolved), and neither `item.Origin` nor `item.OriginRight`
   can resolve the parent (after the within-update pending loop), and
   either of their clocks is in the future — park instead of applying
   the current orphan-store fallback.

3. **Same-client clock gap.** If `item.ID.Clock > existingEnd` for the
   current client (item's clock is past the store's current clock for
   that client), park. Before today this path fell into `integrate()`
   with a `nil` origin lookup and silently mis-placed the item.

Parking appends the item to `d.store.pending.items`. The `missing`
state vector records the **store's current clock for the missing
client at park time** — matching yrs' `missing_sv.set_min(dep,
local_sv.get(&dep))` semantic. This value is used as a watermark:
retry is worthwhile when `store.Clock(client) > missing[client]`
for any client in missing, i.e., any new item has arrived for a
client whose items are blocking pending progress.

- For a cross-client origin/right-origin gap, set
  `missing[origin.Client] = min(current missing value,
  store.Clock(origin.Client))` at park time.
- For a same-client clock gap, set `missing[item.ID.Client] =
  min(current missing value, store.Clock(item.ID.Client))`.

The retry gate is:

```go
func retryable(missing StateVector, store *structStore) bool {
    for client, parkedAt := range missing {
        if store.Clock(client) > parkedAt {
            return true
        }
    }
    return false
}
```

This matches yrs' `for (client, &clock) in pending.missing.iter()
{ if clock < store.blocks.get_clock(client) { retry = true; break; } }`
and Yjs JS's equivalent check in `readUpdateV2`.

### Within-update retry loop (existing logic — kept)

The existing loop at `crdt/update.go:526-568` handles items whose
dependencies arrive **later in the same update** (different client group
decoded after them). Keep this loop as-is with one change: in the
"truly unresolvable" branch (lines 556-565), instead of the blanket
`store.Append(item)` fallback, branch on:

- If the item's unresolved refs point to **future clocks** (present in
  the incoming update's state-vector delta or clearly beyond current
  store clock) → park into `d.store.pending`.
- If the item's unresolved refs point to **GC'd / lost parent info**
  (origin clock is in the past but the item itself is missing) → keep
  the existing `Append` orphan path.

The distinction is clock-based: "could this be resolved if we receive
more updates?" vs. "this will never be resolvable."

### Delete-set parking

When applying the decoded delete-set (`ds.applyTo(txn)` at
`crdt/update.go:574`), entries targeting items not yet in the store
need to be parked. Replace the current unconditional `applyTo` with:

```go
unresolvable := ds.applyToPartial(txn) // returns the entries that couldn't apply
if !unresolvable.IsEmpty() {
    d.store.pendingDs.MergeFrom(unresolvable)
}
```

`applyToPartial` is a new method on `DeleteSet` that applies entries
whose target items exist and returns a new `DeleteSet` containing
entries whose target items are absent. Implementation details in the
plan; semantically equivalent to the current `applyTo` plus a miss-track.

### Retry (inline, at end of applyV1Txn)

After the within-update pending loop and delete-set application, but
before returning:

```go
// Drain pending items whose dependencies are now satisfied. Loop until
// no item makes progress, to handle chained dependencies (B's deps
// satisfied by A's retry → C's deps satisfied by B's retry → ...).
for d.store.pending != nil && retryable(d.store.pending.missing, d.store) {
    items := d.store.pending.items
    d.store.pending = nil
    var stillPending []*Item
    var stillMissing StateVector
    for _, item := range items {
        if ok := tryIntegrate(txn, item); !ok {
            stillPending = append(stillPending, item)
            updateMissing(&stillMissing, item)
        }
    }
    if len(stillPending) > 0 {
        d.store.pending = &pendingUpdate{items: stillPending, missing: stillMissing}
    }
}

// Retry pending delete-set against current store.
d.store.pendingDs = retryPendingDs(txn, d.store.pendingDs)
```

**Critical — inline, not recursive.** `ApplyUpdateV1` calls
`doc.Transact`, which holds `d.mu` for its entire Phase 1. Go's
`sync.Mutex` is not reentrant, so a recursive `ApplyUpdateV1` call
would deadlock. Inline retry keeps all integration work inside the
current Phase 1, under the same lock, producing a single `OnUpdate` at
Phase 2 that includes everything that ended up integrated.

`tryIntegrate` is a small helper extracted from the existing
in-line integration logic. It returns `true` on successful
integration, `false` if the item is still blocked.

### State vector — unchanged behavior, new test

`structStore.StateVector()` already reads from `clients` (the
integrated items map) and does not touch `pending`. This is correct
and critical: if the SV advertised pending items, remote peers would
believe they were integrated and would not re-send the missing
dependencies, creating a permanent gap.

Add a regression test locking this invariant: fill `pending` with
items; assert `StateVector()` returns the integrated-only SV.

### Thread safety

`applyV1Txn` runs under `d.mu` (via `doc.Transact`). All reads and
writes to `d.store.pending` and `d.store.pendingDs` happen under that
lock. No new locks needed; the structures inherit the existing
store-level write mutex discipline.

`OnUpdate` observers fire in Phase 2 outside the lock. The update
bytes emitted are `encodeV1Locked(d, txn.beforeState)` which covers
everything integrated during this transaction — including pending
items that got drained. No ordering issue.

### Error / panic handling

Per the #9 contract: a panic inside the integration loop (including
the pending retry) releases `d.mu` via defer, fires observers with
partial state, and re-raises. Pending items that weren't drained
before the panic remain in `d.store.pending` for future apply calls.
The retry is idempotent — items can be retried any number of times.

### V2 update path

`applyV2Txn` in `crdt/update_v2.go` has parallel structure to
`applyV1Txn`. The same parking and retry logic applies, sharing
the `tryIntegrate` and `retryable` helpers. Both paths read and
write the **same** `d.store.pending` and `d.store.pendingDs` —
one doc, one pending queue, format-agnostic. A V1 arrival may
drain items parked by a prior V2 arrival (or vice versa) because
both paths integrate into the same `structStore`.

## Edge cases

| Scenario | Behavior |
|---|---|
| Update decodes cleanly, nothing to park | `d.store.pending` stays nil; retry loop no-ops |
| Parked item's deps arrive in next update | Inline retry at end of second update drains it |
| Chained deps (B parked, A arrives and satisfies B, B drains; C parked against B, C now drains too) | Inner retry loop handles this — drains pending until no progress, allowing B→C chain in one pass |
| Update references deps in the same update, out of client order | Within-update pending loop (existing, lines 526-568) handles it before the doc-level pending queue is consulted |
| GC'd parent with lost name info | Kept orphan-store fallback — distinguishable from "missing but future" via clock comparison |
| Panic during retry | Per #9, defer unlocks, observers fire with partial state, panic re-raises, pending queue is not corrupted |
| Pending grows unboundedly (adversarial peer) | Accepted per non-goals; metric exposed as `d.store.pending.items` length for future observability |
| Same-client clock gap with subsequent in-order update | First update parks items with clocks > expected; second update arrives with missing clocks; inline retry drains parked items |

## Testing

### Functional

1. **Concurrent producers, reverse arrival order.** Peer A mutates;
   peer B mutates (referencing A's items); peer C receives B's delta
   first, then A's delta. Assert convergence: final state matches
   A's-and-B's combined state on all peers.

2. **Chain of dependencies.** A, B, C are three updates where each
   references the previous. Deliver out-of-order (C, A, B). Assert
   final state converges.

3. **Same-client clock gap.** Receive clock 4 from client X; assert
   it's parked (not mis-integrated at head of parent list). Receive
   clock 3; assert both 3 and 4 integrate correctly.

4. **Delete-set on not-yet-integrated item.** Update A contains an
   item; update B contains a delete-set entry targeting A's item.
   Deliver B before A. Assert B's delete is parked; after A arrives,
   the delete applies correctly and the item is tombstoned.

5. **Multiple pending items from one arrival.** Single update contains
   multiple items all referencing a missing predecessor. Assert all
   are parked together; missing SV records the predecessor clock; all
   drain on the arrival of the predecessor.

### State-vector

6. **SV during pending.** Fill `pending` with items; call `StateVector()`;
   assert only integrated items' clocks are reported — pending clocks
   do not leak. This is the critical invariant for re-send recovery.

### Panic safety

7. **Panic during pending retry.** Force a panic inside `tryIntegrate`
   (e.g., via a malformed parked item). Assert `d.mu` is released,
   observers fire with partial state, original panic re-raises. The
   remaining pending items stay in the queue.

### Regression

8. **Normal-path behavior unchanged.** All existing `TestInteg_*` and
   `TestUnit_*` tests continue to pass without modification.

### Concurrency

9. **Race-detector pass.** Run full suite under `-race`. No data races
   introduced by new pending-queue reads / writes.

## Documentation

- CHANGELOG `[1.2.0]` entry under **Fixed** (describes the cross-update
  Origin / same-client gap bugs) and **Changed** (documents the new
  convergence contract matching upstream).
- RELEASE_NOTES.md overwritten with v1.2.0 content.
- `docs/comparison/ygo-vs-yrs.md`: update the row describing update /
  state-vector handling to indicate pending-structs parity with yrs.

No godoc changes needed on public API — the fix is internal; callers
see corrected behavior without signature changes.

## Out of scope (follow-ups to file)

- Include pending bytes in `EncodeStateAsUpdateV1` output (bandwidth
  optimization, not correctness).
- Memory cap + eviction policy on pending queue.
- Per-client VecDeque grouping inside pendingUpdate for large-scale
  workloads (profiling-driven optimization).

## Semver

`v1.2.0` minor. Peers now converge correctly under delivery orders
that previously produced permanent gaps. This is a **user-observable
behavior change**: applications depending on the old buggy behavior
(which would be strange) would see different state. Patch bump would
misrepresent the impact.

## Estimated scope

- `crdt/store.go` — new fields, small helpers (~50 LOC)
- `crdt/update.go` + `crdt/update_v2.go` — decode-path rewrite, retry
  loop, tryIntegrate extraction (~300-400 LOC)
- `crdt/delete_set.go` — `applyToPartial` method (~50 LOC)
- `crdt/crdt_test.go` — new test matrix (~200-300 LOC)
- CHANGELOG + RELEASE_NOTES + comparison doc — small
- Total: ~600-800 LOC including tests

Approximately 4-7 days of focused work including subagent-driven
implementation, review loops, and CI validation.
