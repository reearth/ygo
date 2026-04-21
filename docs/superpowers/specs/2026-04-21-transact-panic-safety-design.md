# Transact panic safety — design

**Issue:** [reearth/ygo#9](https://github.com/reearth/ygo/issues/9)
**Branch:** `chore/transact-panic-safety`
**Target release:** `v1.1.1` (patch)
**Status:** Draft — awaiting user review

## Background

`Doc.Transact` at [crdt/doc.go:233-341](../../../crdt/doc.go) acquires
`d.mu.Lock()` at entry and releases it with an explicit `d.mu.Unlock()`
before firing observers in Phase 2. The unlock is NOT deferred — so a
panic anywhere between lock and unlock (inside `fn`, `squashRuns`,
observer snapshotting, encoding) leaves `d.mu` held forever, wedging
the entire document.

Any subsequent operation that requires `d.mu` — `GetMap`, `GetText`,
`ApplyUpdateV1`, further `Transact` calls, `OnUpdate` subscribes /
unsubscribes — deadlocks.

This is amplified by `websocket.Server.Apply` (v1.1.0), whose
`defer unsub()` cleanup path also needs `d.mu`: a panic inside the
caller's `transact()` callback hangs the goroutine in the unsub before
the panic can reach the caller. The `Apply` godoc instructs callers
"fn MUST NOT panic" as a workaround.

## Goals

- Release `d.mu` on every exit path from `Transact` (panic or normal).
- Define and document `Transact`'s behavior on panic as part of the
  public contract.
- Remove the "fn MUST NOT panic" caveat from `websocket.Server.Apply`.
- Re-enable the `TestUnit_Apply_FnPanic_SubscriptionCleanedUp` test
  that was removed during issue #8 implementation.

## Non-goals

- Full transactional atomicity / rollback of partial mutations.
  Neither Yjs JS nor the Rust `yrs` implementation provides this;
  both treat transactions as non-atomic on failure. Rollback is a
  larger design problem deserving its own issue and release.
- Changing normal-path behavior. The API surface, observer firing
  order, and performance are identical when `fn` does not panic.

## Reference implementations

Both upstream Yjs implementations commit partial state on exception:

- **Yjs JS**: `transact(fn)` uses `try/finally`. Exceptions propagate
  but the `finally` block still fires observers and persists partial
  state.
- **`y-crdt` (Rust, aka `yrs`)**: `TransactionMut` is an RAII guard.
  `Drop::drop` calls `commit()`, which fires observers unconditionally.
  The doc explicitly says *"Rollbacks are not supported. If some
  operations need to be undone, use [UndoManager]."*

Our design adopts the same contract for ecosystem consistency: a Go
port that diverged from upstream observer semantics would surprise
users who cross platforms.

## Design

### Public behavior

**`Doc.Transact(fn, origin...)` panic semantics (new):**

1. `d.mu` is released on every exit path.
2. If `fn` (or subsequent Phase 1 code) panics:
   - `OnUpdate` callbacks fire with an encoded V1 update containing
     whatever mutations `fn` completed before panicking.
   - `OnAfterTransaction` callbacks fire with the (partial) `*Transaction`.
   - Per-type observers and deep observers fire for whatever was
     recorded in `txn.changed`.
   - The original panic is re-raised to the caller after observers fire.
3. Observers fire **outside the lock** — Go convention (unchanged from
   today). This differs from `yrs`, which fires under the lock due to
   Rust lifetime constraints.
4. Rollback is not provided. In-memory state reflects `fn`'s partial
   work. Callers who need atomicity must implement it at a higher layer.

### Implementation

Refactor `Doc.Transact` to:

1. Acquire the lock.
2. Create `txn` with `beforeState`.
3. Install a deferred block that:
   a. Calls `recover()` to capture any panic.
   b. On panic, best-effort finalizes `txn` (sets `afterState` if unset)
      under an inner recover that swallows secondary panics.
   c. Calls a new `buildPhase2(d, txn, orig)` helper under an inner
      recover that swallows secondary panics. The helper encodes
      update bytes and snapshots observers (as today's inline Phase 1
      tail does), returning a closure that fires them all.
   d. Unlocks `d.mu`.
   e. Calls the Phase 2 closure if one was built.
   f. Re-raises the original panic if any.
4. Calls `fn(txn)`.
5. Sets `txn.afterState` and runs `squashRuns(txn)` on the normal path.

Pseudo-code:

```go
func (d *Doc) Transact(fn func(*Transaction), origin ...any) {
    var orig any
    if len(origin) > 0 {
        orig = origin[0]
    }

    d.mu.Lock()

    txn := &Transaction{
        doc:         d,
        Origin:      orig,
        Local:       true,
        deleteSet:   newDeleteSet(),
        beforeState: d.store.StateVector(),
        changed:     make(map[*abstractType]map[string]struct{}),
    }

    defer func() {
        r := recover()

        if r != nil {
            // Best-effort finalize — safe under a swallow-recover.
            func() {
                defer func() { _ = recover() }()
                if txn.afterState == nil {
                    txn.afterState = d.store.StateVector()
                }
            }()
        }

        // Build Phase 2 plan from whatever state exists. Swallow
        // secondary panics so a corrupt-state encoding does not mask
        // the original panic.
        var phase2 func()
        func() {
            defer func() { _ = recover() }()
            phase2 = buildPhase2(d, txn, orig)
        }()

        d.mu.Unlock()

        if phase2 != nil {
            phase2() // observer panics propagate
        }

        if r != nil {
            panic(r)
        }
    }()

    fn(txn)
    txn.afterState = d.store.StateVector()
    squashRuns(txn)
}

// buildPhase2 factors today's inline Phase 1-tail logic into a single
// function: encodes updateBytes, snapshots per-type / deep / OnUpdate /
// OnAfterTransaction observers, and returns a closure that fires them
// in the current order.
func buildPhase2(d *Doc, txn *Transaction, orig any) func() { /* ... */ }
```

### Interaction with existing control flow

- **`TransactContext`** ([crdt/doc.go:434](../../../crdt/doc.go))
  transitively benefits: its inner `d.Transact` now releases the lock
  on panic and the panic propagates through `TransactContext` as
  expected.
- **Observer firing order** is preserved: per-type → deep → OnUpdate →
  OnAfterTransaction.
- **Normal-path performance** is identical modulo one extra `recover()`
  call per Transact, which is a trivial defer.

## Edge cases

| Scenario | Behavior |
|---|---|
| `fn` panics immediately (no mutations) | `txn.changed` empty; `buildPhase2` produces empty plan; no observer fires; panic re-raises. |
| `fn` panics after partial mutations | Partial update encoded; observers fire with it; persistence captures it; panic re-raises. |
| `fn` panics, then `StateVector()` panics during best-effort finalize | Inner recover swallows the secondary panic; `phase2` may still succeed with `afterState == nil`; if `buildPhase2` also fails, `phase2` stays nil, no observers fire. Original panic still re-raises. |
| `buildPhase2` panics (corrupt state) | Inner recover swallows; `phase2` stays nil; no observers fire. Original panic re-raises. |
| Observer callback panics during `phase2()` | Panic propagates to caller. If `fn` also panicked, that panic is lost (the observer's panic wins). Pre-fix code couldn't exhibit this scenario because `fn`'s panic wedged the lock before observers could run; post-fix we prefer surfacing observer failures over silently swallowing them. |
| `squashRuns` panics | Same handling as `fn` panic — best-effort finalization, observers fire with unsquashed state (slightly less compact but correct). |

## Testing

Required tests in `crdt/crdt_test.go`:

1. **Panic in fn releases the lock.** `Transact(fn_that_panics)` inside
   `recover()`; subsequent `Transact` on same doc succeeds.
2. **Partial mutations observed via `OnUpdate`.** `fn` mutates then
   panics; assert `OnUpdate` receives a non-empty V1 update reflecting
   the mutations.
3. **`OnAfterTransaction` fires with partial txn.** Similar; assert
   callback sees a `*Transaction` with non-empty `changed` map.
4. **Per-type observer fires with partial changes.** `YMap.Observe(fn)`;
   `Transact` mutates the map then panics; assert fn is called.
5. **Panic with no mutations fires no observers.** `Transact(func)` where
   func panics before any mutation; assert observers never called.
6. **Original panic re-raises.** Assert `recover()` value matches what
   fn panicked with.
7. **Regression: normal-path observer firing unchanged.** Existing tests
   in `crdt_test.go` continue to pass.
8. **Regression: observer panic propagates.** Existing test, if any, for
   observer callback panics continues to pass.

Required test in `provider/websocket/inject_test.go`:

9. **Re-enable `TestUnit_Apply_FnPanic_SubscriptionCleanedUp`.**
   (Removed in issue #8 commit `9f6c94b` because the deadlock made it
   unreachable. Now passes.) Also asserts that `OnUpdate` broadcast
   reached the peer with the partial state, demonstrating the new
   contract.

All tests run under `go test -race`.

## Documentation

**`Doc.Transact` godoc.** Update to document panic semantics:

```go
// Transact executes fn inside a transaction. All insertions and deletions made
// during fn are batched; observers fire once after fn returns.
//
// Observers fire OUTSIDE the document lock. This means:
//   - Observer callbacks may safely call back into any Doc method (Transact,
//     GetArray, ApplyUpdate, etc.) without deadlocking.
//   - The document may be modified by another goroutine between the time fn
//     returns and the time observers fire; observers should treat txn as a
//     snapshot of what changed, not a live view of the current state.
//
// Panic semantics:
//   - If fn panics (or any Phase 1 work panics), d.mu is released via defer.
//   - Observers fire with the partial state committed before the panic.
//     Callers who subscribe to OnUpdate may receive a non-empty update
//     describing whatever mutations completed; per-type and deep observers
//     fire for whatever was recorded in txn.changed.
//   - The original panic is re-raised to the caller after observers have
//     fired.
//   - Rollback is NOT supported. The in-memory doc reflects fn's partial
//     work. Callers who need atomicity must implement it above Transact.
//     This matches the behavior of Yjs JS and the Rust yrs implementation.
//   - If fn panicked and an observer callback also panics, the observer's
//     panic reaches the caller and the original fn panic is lost.
```

**`websocket.Server.Apply` godoc.** Remove the "fn MUST NOT panic"
caveat. Replace with a softer note explaining that panics now broadcast
partial state (per the `Transact` contract) rather than wedging the
room.

**CHANGELOG / RELEASE_NOTES.** Add a `v1.1.1` entry under "Fixed" and
"Changed":

- Fixed: `Doc.Transact` no longer leaks `d.mu` on panic.
- Changed: `Doc.Transact` now has documented panic semantics (observers
  fire with partial state, panic re-raises, no rollback).

## Follow-up issue

Open: `crdt: explore transactional atomicity for Doc.Transact panic
scenarios`. Reference the yrs `UndoManager` pattern as a precedent for
solving atomicity at a higher level. Link from this PR.
