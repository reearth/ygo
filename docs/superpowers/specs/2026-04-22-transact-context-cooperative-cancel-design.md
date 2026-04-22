# TransactContext cooperative cancellation — design

**Issue:** [reearth/ygo#10](https://github.com/reearth/ygo/issues/10)
**Branch:** `chore/transact-context-cooperative-cancel`
**Target release:** `v1.1.2` (patch)
**Status:** Draft — awaiting user review

## Background

`Doc.TransactContext(ctx, fn, origin...)` at [crdt/doc.go:517](../../../crdt/doc.go)
is marketed as a context-aware variant of `Transact`. The current
implementation only checks `ctx.Err()` at entry and exit; it cannot
interrupt `fn` once it starts running. The return value after
cancellation during `fn` signals "cancellation happened" but the
transaction still commits, which is misleading.

The existing test `TestUnit_TransactContext_CancelledDuringRun_ReturnsError`
inadvertently documents the bug as intended behavior.

## Goals

- Let callers write transactions that cooperatively observe `ctx` and
  exit early on cancellation.
- Keep the existing `TransactContext` signature backward-compatible:
  callers that do not poll ctx see no change.
- Document the semantics clearly so the "cancellation cannot interrupt
  arbitrary Go code" constraint is explicit.
- Rename the misleading test and add new tests for the cooperative path.

## Non-goals

Neither of the following is considered a gap in this PR or in ygo:

- **Uncooperative mid-fn interruption.** No Yjs-ecosystem implementation
  (JS, Rust) supports interrupting a transaction once started. Yjs JS
  wraps `f` in `try/finally` and always commits; yrs has no cancel/abort
  on `TransactionMut`. Go's language constraints (no safe goroutine
  kill, no OS-level preemption of arbitrary code) make this permanently
  infeasible regardless of upstream precedent.
- **Rollback of partial state on cancellation.** Both Yjs JS and yrs
  deliberately commit partial state and direct users to `UndoManager`
  for logical undo. Go matches this contract per issue #9.

One genuine related gap is tracked separately:
[#14 — error-returning fn variants for Transact / TransactContext](https://github.com/reearth/ygo/issues/14).
This spec does not address it; cooperative ctx polling is the right
first step, and the error-return variant is a larger, orthogonal
ergonomics change.

## Reference implementations

- **Yjs JS** `doc.transact(f)` runs `f` synchronously in `try/finally`.
  No cancellation concept; aborts are communicated via exceptions that
  do not roll back state.
- **yrs** `doc.transact_mut()` returns a `TransactionMut` guard (RAII).
  No closure, no ctx, no cancel. The caller can fail-fast by not
  calling further mutation methods and letting the guard drop (which
  always commits).

Neither provides mid-fn interruption. Go's cooperative-polling model
is the natural fit given its `context.Context` idiom — we expose the
context, and callers' `fn` can choose to poll.

## Design

### Public surface

**Add to `Transaction` struct** (`crdt/transaction.go`):

```go
type Transaction struct {
    // ... existing fields ...
    ctx context.Context // defaults to context.Background() in Transact
}
```

**Add accessor method** (`crdt/transaction.go`):

```go
// Ctx returns the context associated with this transaction. Transactions
// started via Transact return context.Background(); transactions started
// via TransactContext return the caller's ctx. fn can poll Ctx().Err()
// or <-Ctx().Done() to detect cancellation and return early.
//
// Returning early from fn commits whatever mutations have been made so
// far — there is no rollback. Callers needing atomicity should recover
// and reconcile via sync or recreate the doc from persistence.
func (t *Transaction) Ctx() context.Context {
    return t.ctx
}
```

**Modify `Doc.Transact`** to initialize the ctx field to
`context.Background()`:

```go
txn := &Transaction{
    doc:         d,
    Origin:      orig,
    Local:       true,
    deleteSet:   newDeleteSet(),
    beforeState: d.store.StateVector(),
    changed:     make(map[*abstractType]map[string]struct{}),
    ctx:         context.Background(),
}
```

**Modify `Doc.TransactContext`** to thread the caller's ctx into the
transaction. Because `Transact` constructs the `Transaction` internally,
we need a small private helper or an internal entry point that accepts
a pre-configured ctx. Simplest approach: add an unexported
`transactInternal` that both `Transact` and `TransactContext` call,
with ctx as a parameter.

**Private helper signature:**

```go
// transactInternal is the shared entry point that Transact and
// TransactContext both delegate to. It handles lock acquisition,
// panic recovery, and Phase 2 firing — identical to Transact's
// existing body — plus the ctx field on Transaction.
func (d *Doc) transactInternal(ctx context.Context, fn func(*Transaction), origin ...any) {
    // Today's Transact body, with txn.ctx = ctx added to the struct literal.
}
```

Both `Transact` and `TransactContext` become thin wrappers around this.
`Transact` passes `context.Background()`; `TransactContext` passes the
caller's ctx after the entry-guard check.

```go
// TransactContext is like Transact but associates a context with the
// transaction so fn can cooperatively cancel.
//
// If ctx is already cancelled when TransactContext is called, fn is not
// invoked and ctx.Err() is returned immediately.
//
// Inside fn, callers can poll txn.Ctx().Err() or <-txn.Ctx().Done() to
// detect cancellation and return early. Partial mutations committed
// before fn returns are kept (no rollback, consistent with the overall
// Transact contract).
//
// If ctx cancels during fn and fn does not poll, fn runs to completion —
// Go has no safe mechanism for interrupting arbitrary fn code. ctx.Err()
// is returned after the transaction commits as a "missed cancellation"
// signal to the caller.
func (d *Doc) TransactContext(ctx context.Context, fn func(*Transaction), origin ...any) error {
    if err := ctx.Err(); err != nil {
        return err
    }
    d.transactInternal(ctx, fn, origin...)
    return ctx.Err()
}
```

### Semantics table

| Scenario | Behavior |
|---|---|
| `TransactContext(ctx, fn)` with pre-cancelled ctx | fn not called; `ctx.Err()` returned. Unchanged from today. |
| `TransactContext(ctx, fn)` where fn polls `txn.Ctx().Err()` and returns early | Partial mutations commit; observers fire with partial state; `TransactContext` returns `ctx.Err()` if ctx was cancelled, otherwise `nil`. |
| `TransactContext(ctx, fn)` where ctx cancels during fn and fn does not poll | fn runs to completion; mutations commit; observers fire; `ctx.Err()` returned — the caller's signal that they may have missed the cancel. |
| `Transact(fn)` (no-ctx variant) | `txn.Ctx()` returns `context.Background()`; `.Err()` always nil; `.Done()` channel never fires. |
| `fn` panics (any path) | Per issue #9: lock released via defer, observers fire with partial state, panic re-raises. The panic propagates past `TransactContext`'s trailing `return ctx.Err()` line, so the caller sees the panic, not a ctx error. This is consistent with Go semantics — a panicking function does not execute its explicit return statement. |

### Backward compatibility

- Existing `Transact(fn)` callers: no observable change. `txn.Ctx()` returns `context.Background()` for them; they don't have to use it.
- Existing `TransactContext(ctx, fn)` callers that ignore `txn.Ctx()`: no observable change. They still get the "cancellation detected" return value.
- `Transaction` struct gains one unexported field; struct literals outside the crdt package never existed (the struct is constructed only by Doc methods).

## Tests

### Test updates

1. **Rename `TestUnit_TransactContext_CancelledDuringRun_ReturnsError`** to
   `TestUnit_TransactContext_CancelledDuringRun_ReportsButDoesNotInterruptFn`.
   The old name implied interruption; the new name matches actual behavior.
   Assertion changes:
   - Track how many mutations fn completes; verify ALL of them commit
     (not interrupted).
   - Verify `ctx.Err()` is returned after fn completes.

### New tests

2. **`TestUnit_TransactContext_CooperativeCancellationViaCtx`** — fn
   runs a loop, calls `m.Set(txn, key, value)` on each iteration, polls
   `txn.Ctx().Err()` after each iteration, returns early when cancelled.
   Assertions:
   - Only the iterations before the cancel check commit.
   - `ctx.Err()` is returned.
   - Subsequent reads on the doc see the expected partial state.

3. **`TestUnit_Transact_CtxReturnsBackground`** — plain `Transact(fn)`;
   inside fn assert `txn.Ctx() != nil`, `txn.Ctx().Err() == nil`, and
   `<-txn.Ctx().Done()` never fires (verified by a select with default
   branch).

### Unchanged tests

- `TestUnit_TransactContext_CancelledBeforeRun_ReturnsError` — pre-cancelled ctx is a separate code path; assertion unchanged.
- All existing `TestTransact_*` panic-safety tests from issue #9.
- All `TestUnit_Apply_*` tests from issues #8 and #9.

## Documentation

- `TransactContext` godoc rewritten to document the cooperative model
  clearly (see the code block above).
- `Transaction.Ctx()` godoc explains cooperative polling, no-rollback
  semantic, and the `Background()` default for bare `Transact`.
- CHANGELOG `[1.1.2]` entry under **Added** (the new `Ctx()` method) and
  **Changed** (TransactContext godoc clarification). No **Fixed** entry
  because behavior is not changing for existing callers — this is
  additive.
- RELEASE_NOTES.md overwritten with v1.1.2 content.
- `docs/comparison/ygo-vs-yrs.md` — update the "Context-aware
  transaction" row to reflect that Go uses cooperative polling.

## Implementation plan at a glance

Six tasks, likely small each:

1. Extract a private `transactInternal(ctx, fn, origin...)` helper from
   `Transact` that both callers delegate to, with ctx as parameter.
2. Add `ctx` field to `Transaction` struct + `Ctx()` accessor method.
3. Modify `Transact` to default `ctx = context.Background()`;
   `TransactContext` to thread the caller's ctx.
4. Rename the misleading existing test; adjust assertions.
5. Add the two new tests.
6. CHANGELOG + RELEASE_NOTES + godoc updates + comparison doc.

Full plan to be written after spec approval.

## Semver

`v1.1.2` patch. Purely additive — new method on Transaction, no
existing behavior changes for callers that don't use `Ctx()`. The
godoc clarification on `TransactContext` describes what the method
always did, not a new behavior.
