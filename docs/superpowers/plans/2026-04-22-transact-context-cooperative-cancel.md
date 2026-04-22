# TransactContext Cooperative Cancellation Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Expose the transaction's context via `Transaction.Ctx()` so `fn` can cooperatively poll for cancellation, and rewrite `TransactContext` to route through a shared private helper that threads ctx into the transaction.

**Architecture:** Add an unexported `ctx` field to `Transaction` plus an exported `Ctx()` accessor. Extract a private `transactInternal(ctx, fn, origin...)` from the current `Transact` body; both `Transact` and `TransactContext` become thin wrappers. No behavior change for callers that don't read `txn.Ctx()`. Matches Go's cooperative cancellation idiom; neither yrs nor Yjs JS offer mid-fn interruption.

**Tech Stack:** Go 1.23+, `testify/assert` + `testify/require`.

**Spec:** [docs/superpowers/specs/2026-04-22-transact-context-cooperative-cancel-design.md](../specs/2026-04-22-transact-context-cooperative-cancel-design.md)

---

## File structure

**Modified files:**
- `crdt/transaction.go` — add `ctx` field; add `Ctx()` accessor method.
- `crdt/doc.go` — extract `transactInternal`; rewrite `Transact` and `TransactContext` as wrappers; update their godoc.
- `crdt/crdt_test.go` — rename the misleading test; add new tests for `Ctx()` accessor and cooperative cancellation.
- `docs/comparison/ygo-vs-yrs.md` — update the two "Context-aware transaction" rows.
- `CHANGELOG.md` — `v1.1.2` entry.
- `RELEASE_NOTES.md` — overwrite with v1.1.2 content.

**No new files.**

---

## Task 1: Add `ctx` field and `Ctx()` accessor to `Transaction`

**Files:**
- Modify: `crdt/transaction.go` (struct + new method)
- Test: `crdt/crdt_test.go` (one new test)

Extend the `Transaction` struct with a context field and expose it via a simple accessor. This task is test-first: the new test proves that after Task 2 threads ctx through, bare `Transact` callers see `context.Background()`. For Task 1 alone we only add the field + method — the test will compile and pass because Task 2 hasn't happened yet, but the accessor returns the zero value (nil), which we detect in the test and accept — the TDD discipline is enforced by Task 2's stricter assertion.

Actually we can do both halves here since they're coupled: add the field, the accessor, AND default the field in `Transact` to `context.Background()`. The test below then passes on first run.

- [ ] **Step 1: Write the new test**

Append to `/Users/nimit/Documents/Eukarya/ygo/crdt/crdt_test.go`:

```go
func TestUnit_Transact_CtxReturnsBackground(t *testing.T) {
	doc := New()

	var ctxInFn context.Context
	doc.Transact(func(txn *Transaction) {
		ctxInFn = txn.Ctx()
	})

	require.NotNil(t, ctxInFn, "bare Transact must populate a non-nil ctx")
	assert.NoError(t, ctxInFn.Err(), "bare Transact ctx must not report an error")

	// Done() must be a never-firing channel (not nil, non-receivable).
	select {
	case <-ctxInFn.Done():
		t.Fatal("bare Transact ctx.Done() must never fire")
	default:
		// good
	}
}
```

Verify `"context"` is in the imports of `crdt_test.go`. It likely already is from the existing `TransactContext` tests; if not, add it.

- [ ] **Step 2: Run the test — verify it FAILS (compile error)**

Run: `cd /Users/nimit/Documents/Eukarya/ygo && go test ./crdt/ -run TestUnit_Transact_CtxReturnsBackground -v -timeout 10s`

Expected: compile error — `txn.Ctx` undefined.

- [ ] **Step 3: Add `ctx` field to `Transaction` struct**

In `/Users/nimit/Documents/Eukarya/ygo/crdt/transaction.go`, find the `Transaction` struct (starts at line 11). Add a `ctx` field immediately after `newItems`:

```go
type Transaction struct {
	doc         *Doc
	Origin      any  // user-supplied tag forwarded to update observers
	Local       bool // true when the change originated on this peer
	deleteSet   DeleteSet
	beforeState StateVector
	afterState  StateVector
	// changed tracks which types (and which map keys within them) were modified.
	changed map[*abstractType]map[string]struct{}
	// newItems collects ContentString items integrated during this transaction.
	// Used by squashRuns to merge adjacent same-client runs after observers fire.
	newItems []*Item
	// ctx is the context associated with this transaction. Set to
	// context.Background() by Transact and to the caller's ctx by
	// TransactContext. Exposed via the Ctx() method so fn can poll for
	// cancellation.
	ctx context.Context
}
```

Add `"context"` to `transaction.go`'s imports if not already present.

- [ ] **Step 4: Add `Ctx()` accessor method**

In the same file, after the `Transaction` struct definition and before `squashRuns`, add:

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

- [ ] **Step 5: Default the ctx field in `Transact` to `context.Background()`**

In `/Users/nimit/Documents/Eukarya/ygo/crdt/doc.go`, find the `Transaction{...}` struct literal inside `Transact` (around line 345). Add `ctx: context.Background(),` as a new field assignment:

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

Verify `"context"` is already in `doc.go`'s imports — it should be because `TransactContext` already takes a ctx parameter.

- [ ] **Step 6: Run the test — verify it PASSES**

Run: `cd /Users/nimit/Documents/Eukarya/ygo && go test ./crdt/ -run TestUnit_Transact_CtxReturnsBackground -v -timeout 10s`

Expected: PASS.

- [ ] **Step 7: Run full suite with race detector**

Run: `cd /Users/nimit/Documents/Eukarya/ygo && go test -race ./... -timeout 120s 2>&1 | tail -10`

Expected: all packages pass. `Transact` callers see no change — the new field is populated but existing tests don't read it.

- [ ] **Step 8: Commit**

```bash
cd /Users/nimit/Documents/Eukarya/ygo && git add crdt/transaction.go crdt/doc.go crdt/crdt_test.go && git commit -m "feat(crdt): add Transaction.Ctx() accessor (#10)

Expose the context associated with a transaction so fn can poll for
cooperative cancellation. Transact defaults the context to
context.Background(); TransactContext will thread the caller's ctx
in the next commit.

Backward compatible: existing callers that don't call txn.Ctx() see
no change in behavior."
```

---

## Task 2: Extract `transactInternal` and thread ctx through `TransactContext`

**Files:**
- Modify: `crdt/doc.go` (extract `transactInternal`; rewrite `Transact` and `TransactContext` as wrappers)

Pure refactor for `Transact`; behavior change for `TransactContext` (it now threads the caller's ctx). No new tests here — Task 3 rewrites the misleading existing test, and Task 4 adds the cooperative test that exercises the new path end-to-end.

- [ ] **Step 1: Add `transactInternal` helper**

In `/Users/nimit/Documents/Eukarya/ygo/crdt/doc.go`, add a new function `transactInternal` BEFORE the existing `Transact`. Copy the current `Transact` body verbatim, but:
- Change the signature to take `ctx context.Context` as the first parameter.
- Change the struct literal's `ctx: context.Background(),` line to `ctx: ctx,`.

Concretely, insert this at the appropriate spot in `doc.go` (immediately above the `Transact` function):

```go
// transactInternal is the shared transaction entry point that Transact
// and TransactContext both delegate to. It acquires d.mu, runs fn under
// the lock with panic-safe cleanup, fires Phase 2 observers outside the
// lock, and re-raises any panic to the caller. The ctx parameter is
// stored on the Transaction struct and exposed to fn via Transaction.Ctx().
//
// See docs/superpowers/specs/2026-04-21-transact-panic-safety-design.md
// for the full rationale on the defer/recover structure.
func (d *Doc) transactInternal(ctx context.Context, fn func(*Transaction), origin ...any) {
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
		ctx:         ctx,
	}

	defer func() {
		r := recover()

		if r != nil {
			func() {
				defer func() { _ = recover() }()
				if txn.afterState == nil {
					txn.afterState = d.store.StateVector()
				}
			}()
		}

		var phase2 func()
		if r != nil {
			func() {
				defer func() { _ = recover() }()
				phase2 = buildPhase2(d, txn)
			}()
		} else {
			phase2 = buildPhase2(d, txn)
		}

		d.mu.Unlock()

		if phase2 != nil {
			phase2()
		}

		if r != nil {
			panic(r)
		}
	}()

	fn(txn)

	txn.afterState = d.store.StateVector()

	squashRuns(txn)
}
```

- [ ] **Step 2: Replace `Transact`'s body with a call to `transactInternal`**

In `/Users/nimit/Documents/Eukarya/ygo/crdt/doc.go`, find the existing `Transact` function. Replace its ENTIRE body (everything inside the curly braces) with a single delegating call. Keep the docstring unchanged:

```go
func (d *Doc) Transact(fn func(*Transaction), origin ...any) {
	d.transactInternal(context.Background(), fn, origin...)
}
```

- [ ] **Step 3: Rewrite `TransactContext` to delegate to `transactInternal`**

In `/Users/nimit/Documents/Eukarya/ygo/crdt/doc.go`, find the existing `TransactContext` function. Replace its ENTIRE body with:

```go
func (d *Doc) TransactContext(ctx context.Context, fn func(*Transaction), origin ...any) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	d.transactInternal(ctx, fn, origin...)
	return ctx.Err()
}
```

Leave the existing godoc comment above `TransactContext` unchanged for now — Task 5 rewrites it.

- [ ] **Step 4: Run full suite**

Run: `cd /Users/nimit/Documents/Eukarya/ygo && go test -race ./... -timeout 120s 2>&1 | tail -10`

Expected: all packages pass. Every existing test continues to work because the code path is semantically identical — `Transact` still uses `context.Background()` internally, and `TransactContext` still does entry-check + run + exit-check, just now with the caller's ctx reaching the Transaction struct.

Note: the existing `TestUnit_TransactContext_CancelledDuringRun_ReturnsError` still passes — fn runs to completion (the bug is preserved because we haven't yet added cooperative polling, and this test doesn't poll). Task 3 rewrites it to reflect the actual semantics.

- [ ] **Step 5: Run `go vet` and build**

Run: `cd /Users/nimit/Documents/Eukarya/ygo && go vet ./... && go build ./...`

Expected: no output.

- [ ] **Step 6: Commit**

```bash
cd /Users/nimit/Documents/Eukarya/ygo && git add crdt/doc.go && git commit -m "refactor(crdt): route Transact and TransactContext through transactInternal

Extract the shared transaction body into transactInternal(ctx, fn, origin...)
and make both Transact and TransactContext thin wrappers. Transact passes
context.Background(); TransactContext passes the caller's ctx after an
entry-guard check.

Behavior preserved for every existing caller. The new structure lets
fn poll txn.Ctx() for cooperative cancellation when called through
TransactContext."
```

---

## Task 3: Rewrite the misleading `TransactContext_CancelledDuringRun` test

**Files:**
- Modify: `crdt/crdt_test.go`

The existing test asserts that cancelling `ctx` during `fn` causes `TransactContext` to return an error. That's true, but the test name (`CancelledDuringRun_ReturnsError`) implies interruption. The actual semantic is "fn runs to completion regardless; ctx.Err() is returned as a signal." Rename and reinforce the semantics.

- [ ] **Step 1: Locate the existing test**

In `/Users/nimit/Documents/Eukarya/ygo/crdt/crdt_test.go` there is:

```go
func TestUnit_TransactContext_CancelledDuringRun_ReturnsError(t *testing.T) {
	doc := newTestDoc(1)
	ctx, cancel := context.WithCancel(context.Background())

	called := false
	err := doc.TransactContext(ctx, func(txn *Transaction) {
		called = true
		cancel() // cancel during transaction
	})
	assert.True(t, called)
	assert.Error(t, err, "ctx.Err() should propagate after cancel")
}
```

- [ ] **Step 2: Replace it with the renamed version**

Delete the existing test block and replace it with:

```go
func TestUnit_TransactContext_CancelledDuringRun_ReportsButDoesNotInterruptFn(t *testing.T) {
	// Cancelling ctx during fn does NOT interrupt fn — Go has no safe
	// mechanism for preempting arbitrary code. fn runs to completion;
	// every mutation it makes is committed. TransactContext returns
	// ctx.Err() after the commit as a "cancellation happened" signal
	// so the caller can decide whether to compensate.
	doc := newTestDoc(1)
	m := doc.GetMap("m")
	ctx, cancel := context.WithCancel(context.Background())

	const n = 5
	completed := 0
	err := doc.TransactContext(ctx, func(txn *Transaction) {
		for i := 0; i < n; i++ {
			if i == 2 {
				cancel() // cancel in the middle; fn must not notice
			}
			m.Set(txn, "k"+string(rune('0'+i)), i)
			completed++
		}
	})

	assert.Equal(t, n, completed, "fn must run to completion regardless of ctx cancellation")
	require.Error(t, err, "TransactContext must return ctx.Err() after a mid-fn cancel")
	assert.ErrorIs(t, err, context.Canceled)

	// Every mutation fn made is committed to the doc.
	for i := 0; i < n; i++ {
		got, ok := m.Get("k" + string(rune('0'+i)))
		require.True(t, ok, "key k%d must be committed", i)
		assert.Equal(t, int64(i), got)
	}
}
```

Note: `m.Set(txn, key, int)` — the value is stored as `int` but retrieved as `int64` because YMap's ContentAny decodes integers as int64. The `assert.Equal(t, int64(i), got)` reflects this, matching patterns in the existing websocket tests.

- [ ] **Step 3: Run the renamed test**

Run: `cd /Users/nimit/Documents/Eukarya/ygo && go test ./crdt/ -run TestUnit_TransactContext_CancelledDuringRun_ReportsButDoesNotInterruptFn -v -timeout 10s`

Expected: PASS.

- [ ] **Step 4: Verify the old test name no longer exists**

Run: `cd /Users/nimit/Documents/Eukarya/ygo && go test ./crdt/ -run TestUnit_TransactContext_CancelledDuringRun_ReturnsError -v -timeout 5s 2>&1 | tail -5`

Expected: `ok` with `testing: warning: no tests to run` (Go does not report errors for missing test names; the absence of a "PASS" line for that specific name confirms the rename).

- [ ] **Step 5: Run full suite**

Run: `cd /Users/nimit/Documents/Eukarya/ygo && go test -race ./... -timeout 120s 2>&1 | tail -5`

Expected: all packages pass.

- [ ] **Step 6: Commit**

```bash
cd /Users/nimit/Documents/Eukarya/ygo && git add crdt/crdt_test.go && git commit -m "test(crdt): clarify TransactContext cancellation-during-run semantics

Rename TestUnit_TransactContext_CancelledDuringRun_ReturnsError to
TestUnit_TransactContext_CancelledDuringRun_ReportsButDoesNotInterruptFn
and reinforce the assertions: fn runs to completion, every mutation
commits, ctx.Err() is returned for caller awareness. The old name
implied mid-fn interruption, which Go cannot safely provide."
```

---

## Task 4: Add cooperative cancellation test

**Files:**
- Modify: `crdt/crdt_test.go`

Add a test that exercises the new `Ctx()` path: fn polls `txn.Ctx().Err()` between iterations and returns early when cancelled. This is the whole point of the cooperative model.

- [ ] **Step 1: Add the test**

Append to `/Users/nimit/Documents/Eukarya/ygo/crdt/crdt_test.go`:

```go
func TestUnit_TransactContext_CooperativeCancellationViaCtx(t *testing.T) {
	// When fn cooperatively polls txn.Ctx(), it can detect cancellation
	// and return early. Mutations made before the check commit; mutations
	// that would have happened after the check do not.
	doc := newTestDoc(1)
	m := doc.GetMap("m")
	ctx, cancel := context.WithCancel(context.Background())

	const total = 10
	const cancelAt = 3 // cancel after this many mutations
	completed := 0
	err := doc.TransactContext(ctx, func(txn *Transaction) {
		for i := 0; i < total; i++ {
			if err := txn.Ctx().Err(); err != nil {
				return // cooperative early return
			}
			m.Set(txn, "k"+string(rune('0'+i)), i)
			completed++
			if completed == cancelAt {
				cancel() // caller cancels after cancelAt completed
			}
		}
	})

	require.Error(t, err, "TransactContext must return ctx.Err() after cooperative cancel")
	assert.ErrorIs(t, err, context.Canceled)
	assert.Equal(t, cancelAt, completed, "fn must have returned after the cancelAt-th iteration's ctx check")

	// Exactly `cancelAt` mutations are committed; the remaining are not.
	for i := 0; i < cancelAt; i++ {
		got, ok := m.Get("k" + string(rune('0'+i)))
		require.True(t, ok, "key k%d must be committed (before cancel)", i)
		assert.Equal(t, int64(i), got)
	}
	for i := cancelAt; i < total; i++ {
		_, ok := m.Get("k" + string(rune('0'+i)))
		assert.False(t, ok, "key k%d must NOT be committed (after cancel)", i)
	}
}
```

- [ ] **Step 2: Run the test**

Run: `cd /Users/nimit/Documents/Eukarya/ygo && go test ./crdt/ -run TestUnit_TransactContext_CooperativeCancellationViaCtx -v -timeout 10s`

Expected: PASS.

- [ ] **Step 3: Run full suite**

Run: `cd /Users/nimit/Documents/Eukarya/ygo && go test -race ./... -timeout 120s 2>&1 | tail -5`

Expected: all packages pass.

- [ ] **Step 4: Commit**

```bash
cd /Users/nimit/Documents/Eukarya/ygo && git add crdt/crdt_test.go && git commit -m "test(crdt): cover cooperative cancellation via txn.Ctx() (#10)

fn polls txn.Ctx().Err() between iterations and returns early when
cancelled. Verifies the committed subset matches exactly the
iterations that ran before the ctx check observed cancellation, and
that TransactContext returns ctx.Err()."
```

---

## Task 5: Update godoc on `Transact`, `TransactContext`, and the comparison doc

**Files:**
- Modify: `crdt/doc.go` (rewrite the docstring above `TransactContext`)
- Modify: `docs/comparison/ygo-vs-yrs.md` (two rows)

- [ ] **Step 1: Rewrite `TransactContext`'s godoc**

In `/Users/nimit/Documents/Eukarya/ygo/crdt/doc.go`, find the docstring comment immediately above `func (d *Doc) TransactContext(...)`. Today it reads:

```go
// TransactContext is like Transact but returns immediately with ctx.Err() if
// the context is already cancelled before the transaction starts.
// This is useful when the caller needs a cancellation path (e.g. server
// shutdown) without changing call sites that use the bare Transact form.
```

Replace the entire comment block with:

```go
// TransactContext is like Transact but associates a context with the
// transaction so fn can cooperatively observe cancellation.
//
// If ctx is already cancelled when TransactContext is called, fn is not
// invoked and ctx.Err() is returned immediately.
//
// Inside fn, callers can poll txn.Ctx().Err() or <-txn.Ctx().Done() to
// detect cancellation and return early. Any mutations fn completed
// before returning are committed (no rollback, consistent with the
// Transact panic-safety contract — see Transact's godoc).
//
// If ctx cancels during fn and fn does not poll, fn runs to completion —
// Go has no safe mechanism for interrupting arbitrary fn code. ctx.Err()
// is returned after the transaction commits as a "missed cancellation"
// signal to the caller. It is not an error flag for the mutations; those
// are committed either way.
//
// Neither Yjs JS nor the Rust yrs implementation offers mid-fn
// interruption either; cooperative polling is the ecosystem norm.
```

- [ ] **Step 2: Update the `docs/comparison/ygo-vs-yrs.md` rows**

In `/Users/nimit/Documents/Eukarya/ygo/docs/comparison/ygo-vs-yrs.md`, find the two rows mentioning `Doc.TransactContext`. Based on grep output earlier they are at line 75 and line 199.

Find the row at **line ~75** — it likely reads something like:

```
| **Context-aware transaction** | ❌ Not present | ✅ `Doc.TransactContext(ctx, ...)` | — | ygo-only; useful for request-scoped cancellation |
```

Replace the right-side note (the "ygo-only..." text) with more precise wording:

```
| **Context-aware transaction** | ❌ Not present | ✅ `Doc.TransactContext(ctx, ...)` with cooperative polling via `Transaction.Ctx()` | — | ygo-only; fn polls ctx to exit early; no mid-fn interrupt (matches yrs' no-cancel model) |
```

Apply the same update to the row at **line ~199** (same wording, same replacement).

If the exact cell contents differ slightly (because of CHANGELOG/release-notes updates since the grep), preserve the existing format and only swap the notes column to the new wording. Use Read on those lines first if unsure.

- [ ] **Step 3: Run tests and vet**

Run: `cd /Users/nimit/Documents/Eukarya/ygo && go test -race ./... -timeout 60s 2>&1 | tail -3 && go vet ./...`

Expected: all pass.

- [ ] **Step 4: Commit**

```bash
cd /Users/nimit/Documents/Eukarya/ygo && git add crdt/doc.go docs/comparison/ygo-vs-yrs.md && git commit -m "docs(crdt): rewrite TransactContext godoc for cooperative model

Spells out the contract: ctx checked at entry, fn must poll
txn.Ctx() to exit early, mutations committed regardless, ctx.Err()
returned as a signal after completion. Cross-references that
neither Yjs JS nor yrs offer mid-fn interrupt either. Updated the
comparison doc's two mentions of TransactContext to reflect the
cooperative model."
```

---

## Task 6: CHANGELOG and RELEASE_NOTES for v1.1.2

**Files:**
- Modify: `CHANGELOG.md` (prepend v1.1.2 entry)
- Overwrite: `RELEASE_NOTES.md`

- [ ] **Step 1: Add v1.1.2 entry to `CHANGELOG.md`**

Open `/Users/nimit/Documents/Eukarya/ygo/CHANGELOG.md`. Find the `## [1.1.1]` heading and insert IMMEDIATELY BEFORE it:

```markdown
## [1.1.2] — 2026-04-22

### Added

- **`Transaction.Ctx()` accessor (#10)**: fn running inside `Transact` or `TransactContext` can now call `txn.Ctx().Err()` or `<-txn.Ctx().Done()` to cooperatively detect cancellation and return early. Mutations made before the early return commit; those that would have happened after do not. `Transact` populates the ctx with `context.Background()` so bare callers see a non-cancellable context.

### Changed

- **`TransactContext` godoc rewritten** to document the cooperative-polling contract explicitly. Behavior is unchanged for existing callers: the entry-guard check still runs, fn still executes to completion if it does not poll, and ctx.Err() is still returned as a "cancellation happened" signal. The new godoc clarifies that Go cannot safely interrupt arbitrary fn code (same constraint as Yjs JS and yrs).

```

Add a link at the bottom of `CHANGELOG.md` matching the existing `[x.y.z]: https://github.com/reearth/ygo/releases/tag/vx.y.z` pattern. Look at how v1.1.1 / v1.1.0 / v1.0.5 are linked (added in commit `2367984`); insert `[1.1.2]: https://github.com/reearth/ygo/releases/tag/v1.1.2` above the v1.1.1 entry.

- [ ] **Step 2: Overwrite `RELEASE_NOTES.md`**

Overwrite `/Users/nimit/Documents/Eukarya/ygo/RELEASE_NOTES.md` with:

```markdown
## What's new

- **`Transaction.Ctx()` for cooperative cancellation (#10).** `fn` inside `Transact` or `TransactContext` can now poll `txn.Ctx()` to detect cancellation and return early. Mutations made before the early return commit; those that would follow do not. Closes a long-standing gap where `TransactContext` promised more than it delivered — Go cannot safely interrupt arbitrary `fn` code, so cooperative polling is the mechanism both Yjs JS and the Rust yrs implementation rely on too.
- **`TransactContext` godoc rewritten** to document the contract explicitly. No behavior change for callers that ignore `txn.Ctx()`.

## Install

```
go get github.com/reearth/ygo@v1.1.2
```

See [CHANGELOG.md](https://github.com/reearth/ygo/blob/main/CHANGELOG.md) for full details.
```

Note: use actual triple-backticks in the final file.

- [ ] **Step 3: Commit**

```bash
cd /Users/nimit/Documents/Eukarya/ygo && git add CHANGELOG.md RELEASE_NOTES.md && git commit -m "docs: changelog and release notes for v1.1.2 (#10)"
```

---

## Final verification

- [ ] **Step 1: Run the full test suite with the race detector**

Run: `cd /Users/nimit/Documents/Eukarya/ygo && go test -race -timeout 120s ./...`

Expected: all packages pass.

- [ ] **Step 2: Run the three CI-equivalent jobs locally**

```bash
cd /Users/nimit/Documents/Eukarya/ygo && go vet ./... && go build ./...
cd /Users/nimit/Documents/Eukarya/ygo && golangci-lint run --timeout 5m ./...
cd /Users/nimit/Documents/Eukarya/ygo && govulncheck ./...
```

Expected: all clean.

- [ ] **Step 3: Check git log**

Run: `git log --oneline chore/transact-context-cooperative-cancel ^main`

Expected: 7 commits — spec + Task 1 (Ctx method) + Task 2 (transactInternal refactor) + Task 3 (test rewrite) + Task 4 (cooperative test) + Task 5 (godoc + comparison) + Task 6 (CHANGELOG + RELEASE_NOTES).

---

## Self-review notes

- **Spec coverage:**
  - `Transaction.Ctx()` accessor → Task 1
  - `Transact` defaults ctx to `context.Background()` → Task 1 step 5
  - `TransactContext` threads ctx via `transactInternal` → Task 2
  - Refactor to shared helper → Task 2
  - Rewrite misleading test → Task 3
  - New cooperative test → Task 4
  - New `TestUnit_Transact_CtxReturnsBackground` → Task 1
  - Godoc on `Transaction.Ctx()` → Task 1 step 4
  - Godoc on `TransactContext` → Task 5 step 1
  - `docs/comparison/ygo-vs-yrs.md` update → Task 5 step 2
  - CHANGELOG / RELEASE_NOTES → Task 6
  - Panic semantics referenced, not re-tested here → inherited from #9 tests; no new coverage needed

- **Placeholder scan:** No TBDs / TODOs / "similar to Task N" references. Every step has concrete code or exact commands.

- **Type consistency:**
  - `transactInternal(ctx context.Context, fn func(*Transaction), origin ...any)` — Task 2 step 1 declares, Task 2 steps 2-3 call. Signature matches.
  - `Transaction.Ctx() context.Context` — Task 1 step 4 declares, Tasks 3 and 4 call. Signature matches.
  - `ctx` field on `Transaction` struct — Task 1 step 3 declares; Task 1 step 5 populates in `Transact`; Task 2 step 1 populates in `transactInternal`. Consistent.
