# Transact Panic Safety Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Fix `Doc.Transact`'s lock leak on panic in `fn` and define panic semantics that match upstream Yjs (JS) and `yrs` (Rust) — observers fire with partial state, original panic re-raises, no rollback.

**Architecture:** Extract today's inline Phase 2 setup into a `buildPhase2` helper. Wrap the Transact body in a deferred `recover()` that best-effort finalizes `txn`, builds the Phase 2 plan under a protective recover, unlocks, fires Phase 2 outside the lock, and re-raises the original panic. Normal-path behavior and observer firing order are unchanged.

**Tech Stack:** Go 1.22+, `testify/assert` + `testify/require`.

**Spec:** [docs/superpowers/specs/2026-04-21-transact-panic-safety-design.md](../specs/2026-04-21-transact-panic-safety-design.md)

---

## File structure

**Modified files:**
- `crdt/doc.go` — refactor `Transact` to be panic-safe; extract `buildPhase2`.
- `crdt/crdt_test.go` — add panic-safety tests.
- `provider/websocket/inject.go` — update `Apply` godoc (drop the "fn MUST NOT panic" caveat).
- `provider/websocket/inject_test.go` — re-enable `TestUnit_Apply_FnPanic_SubscriptionCleanedUp` (removed in issue #8 commit `9f6c94b`).
- `CHANGELOG.md` — `v1.1.1` entry.
- `RELEASE_NOTES.md` — overwrite with `v1.1.1` content.

**No new files.**

---

## Task 1: Extract `buildPhase2` helper (pure refactor)

**Files:**
- Modify: `crdt/doc.go` (`Transact` body, lines ~233-341)

Pure refactor — moves today's inline Phase 2 setup into a named function without changing any observable behavior. Verifies the refactor via the existing test suite. No new tests in this task.

- [ ] **Step 1: Extract `buildPhase2` in `crdt/doc.go`**

In `/Users/nimit/Documents/Eukarya/ygo/crdt/doc.go`, locate the `Transact` function. The current body runs Phase 1 under `d.mu.Lock()`, then unlocks, then fires observers. Extract lines from `var updateBytes []byte` (around line 265) through the end of the `onAfterTxnSnap` block (around line 319) — everything that builds the observer plans — into a new package-level function. Keep the Phase 2 firing loop in `Transact` for now.

Add this new function immediately before `Transact`:

```go
// buildPhase2 runs under d.mu (called from Transact while the lock is held)
// and returns a closure that fires all observers in the correct order.
// The closure must be invoked OUTSIDE d.mu — observers may re-enter Doc
// methods that acquire d.mu, which would deadlock under the lock.
//
// Returns nil only if there is nothing to fire (no observers of any kind).
func buildPhase2(d *Doc, txn *Transaction, orig any) func() {
	// Encode the incremental update and snapshot observer slices while still
	// holding the lock so we get a consistent view.
	var updateBytes []byte
	if len(d.onUpdate) > 0 {
		updateBytes = encodeV1Locked(d, txn.beforeState)
	}

	// Snapshot per-type observer closures while the write lock is held.
	// prepareFire copies each type's observer slice and builds the event struct,
	// so concurrent Observe/Unobserve calls (which also hold the write lock)
	// cannot race with the fire loop below (N-C1).
	fireFns := make([]func(), 0, len(txn.changed))
	for t, keys := range txn.changed {
		if t.owner != nil {
			if fn := t.owner.prepareFire(txn, keys); fn != nil {
				fireFns = append(fireFns, fn)
			}
		}
	}

	// Snapshot deep-observer chains.
	type deepEntry struct {
		fns []func(*Transaction)
	}
	firedDeep := make(map[*abstractType]struct{})
	var deepSnap []deepEntry
	for t := range txn.changed {
		current := t
		for current != nil {
			if _, already := firedDeep[current]; already {
				break
			}
			firedDeep[current] = struct{}{}
			if len(current.deepObservers) > 0 {
				fns := make([]func(*Transaction), len(current.deepObservers))
				for i, s := range current.deepObservers {
					fns[i] = s.fn
				}
				deepSnap = append(deepSnap, deepEntry{fns})
			}
			if current.item != nil {
				current = current.item.Parent
			} else {
				break
			}
		}
	}

	// Snapshot OnUpdate callbacks.
	onUpdateSnap := make([]func([]byte, any), len(d.onUpdate))
	for i, s := range d.onUpdate {
		onUpdateSnap[i] = s.fn
	}
	onAfterTxnSnap := make([]func(*Transaction), len(d.onAfterTxn))
	for i, s := range d.onAfterTxn {
		onAfterTxnSnap[i] = s.fn
	}

	if len(fireFns) == 0 && len(deepSnap) == 0 && len(onUpdateSnap) == 0 && len(onAfterTxnSnap) == 0 {
		return nil
	}

	return func() {
		for _, fn := range fireFns {
			fn()
		}
		for _, de := range deepSnap {
			for _, fn := range de.fns {
				fn(txn)
			}
		}
		for _, fn := range onUpdateSnap {
			fn(updateBytes, orig)
		}
		for _, fn := range onAfterTxnSnap {
			fn(txn)
		}
	}
}
```

Then modify `Transact` to call `buildPhase2` and invoke its returned closure after `d.mu.Unlock()`:

```go
func (d *Doc) Transact(fn func(*Transaction), origin ...any) {
	var orig any
	if len(origin) > 0 {
		orig = origin[0]
	}

	// ── Phase 1: run the transaction body under the lock ─────────────────────
	d.mu.Lock()

	txn := &Transaction{
		doc:         d,
		Origin:      orig,
		Local:       true,
		deleteSet:   newDeleteSet(),
		beforeState: d.store.StateVector(),
		changed:     make(map[*abstractType]map[string]struct{}),
	}

	fn(txn)

	txn.afterState = d.store.StateVector()

	// Squash adjacent same-client ContentString runs before encoding so that
	// the incremental update sent to peers is already compact.
	// Note: squashing happens before per-type observers fire. Observers therefore
	// see merged runs rather than individual character items. This is intentional:
	// the YTextEvent API does not expose raw Items, and firing after squash
	// removes the need for a second lock cycle.
	squashRuns(txn)

	phase2 := buildPhase2(d, txn, orig)

	d.mu.Unlock()
	// ── Phase 2: fire all observers OUTSIDE the lock ──────────────────────────

	if phase2 != nil {
		phase2()
	}
}
```

- [ ] **Step 2: Run the full test suite to verify no regressions**

Run: `cd /Users/nimit/Documents/Eukarya/ygo && go test -race ./... -timeout 120s`

Expected: all packages pass. `crdt`, `awareness`, `provider/websocket`, `provider/http`, `sync`, `encoding` — all green.

- [ ] **Step 3: Run vet and build**

Run: `cd /Users/nimit/Documents/Eukarya/ygo && go vet ./... && go build ./...`

Expected: no output (all clean).

- [ ] **Step 4: Commit**

```bash
cd /Users/nimit/Documents/Eukarya/ygo && git add crdt/doc.go && git commit -m "refactor(crdt): extract buildPhase2 from Transact

Pure refactor — no behavior change. Moves the inline Phase 2 setup
(update encoding, observer snapshotting) into a named helper so the
subsequent panic-safety task can invoke it from a deferred cleanup
path without duplicating logic.

The helper returns nil when there is nothing to fire; Transact skips
the Phase 2 call in that case."
```

---

## Task 2: Panic-safe `Transact` — lock release

**Files:**
- Modify: `crdt/doc.go` (`Transact` function)
- Test: `crdt/crdt_test.go` (new test)

Add the first failing test — one that demonstrates the deadlock — then wrap Transact's body in a deferred `recover()` that releases `d.mu`.

- [ ] **Step 1: Write the failing test**

Find a suitable place in `/Users/nimit/Documents/Eukarya/ygo/crdt/crdt_test.go` to add new tests (e.g., alongside other Transact tests). Append:

```go
func TestTransact_PanicReleasesLock(t *testing.T) {
	doc := New()

	// First Transact panics. We recover to prevent the test from failing.
	func() {
		defer func() { _ = recover() }()
		doc.Transact(func(txn *Transaction) {
			panic("boom")
		})
	}()

	// If the lock leaked, any subsequent operation that acquires d.mu
	// would deadlock. Use a short timeout to detect the hang.
	done := make(chan struct{})
	go func() {
		doc.Transact(func(txn *Transaction) {
			doc.GetMap("m").Set(txn, "k", "v")
		})
		close(done)
	}()

	select {
	case <-done:
		// good — lock was released, second Transact completed
	case <-time.After(2 * time.Second):
		t.Fatal("Transact deadlocked — d.mu was not released after panic in fn")
	}

	got, ok := doc.GetMap("m").Get("k")
	require.True(t, ok)
	assert.Equal(t, "v", got)
}
```

Add `"time"` to the imports in `crdt_test.go` if not already present. Add `"github.com/stretchr/testify/require"` import if not already present.

- [ ] **Step 2: Run the test — verify it FAILS (deadlock)**

Run: `cd /Users/nimit/Documents/Eukarya/ygo && go test ./crdt/ -run TestTransact_PanicReleasesLock -v -timeout 10s`

Expected: FAIL with `Transact deadlocked — d.mu was not released after panic in fn`.

- [ ] **Step 3: Implement panic-safe `Transact`**

Replace the entire body of the `Transact` function in `/Users/nimit/Documents/Eukarya/ygo/crdt/doc.go` with the following:

```go
func (d *Doc) Transact(fn func(*Transaction), origin ...any) {
	var orig any
	if len(origin) > 0 {
		orig = origin[0]
	}

	// ── Phase 1: run the transaction body under the lock ─────────────────────
	d.mu.Lock()

	txn := &Transaction{
		doc:         d,
		Origin:      orig,
		Local:       true,
		deleteSet:   newDeleteSet(),
		beforeState: d.store.StateVector(),
		changed:     make(map[*abstractType]map[string]struct{}),
	}

	// The deferred block is responsible for:
	//   1. Capturing any panic from fn / squashRuns via recover().
	//   2. Best-effort finalizing txn so buildPhase2 has the state it needs.
	//   3. Building the Phase 2 observer plan under a protective recover
	//      so that a corrupt-state encoding does not mask the original panic.
	//   4. Releasing d.mu — unconditionally, on every exit path.
	//   5. Firing Phase 2 observers OUTSIDE the lock.
	//   6. Re-raising the original panic to the caller.
	//
	// See docs/superpowers/specs/2026-04-21-transact-panic-safety-design.md
	// for the full rationale. Matches the behavior of Yjs JS (try/finally
	// fires observers on exception) and yrs (Drop::drop commits partial
	// state). Rollback is not provided.
	defer func() {
		r := recover()

		// Best-effort finalize on panic path. StateVector() might itself
		// panic if the store is corrupt, so guard with an inner recover.
		if r != nil {
			func() {
				defer func() { _ = recover() }()
				if txn.afterState == nil {
					txn.afterState = d.store.StateVector()
				}
			}()
		}

		// Build the Phase 2 plan under a protective recover: a secondary
		// panic here (e.g. encodeV1Locked crashing on partial state) must
		// not mask the caller's original panic. If buildPhase2 panics we
		// simply skip observer firing.
		var phase2 func()
		func() {
			defer func() { _ = recover() }()
			phase2 = buildPhase2(d, txn, orig)
		}()

		d.mu.Unlock()

		// Fire observers outside the lock. Observer panics propagate as
		// today — if both fn and an observer panic, the observer's panic
		// wins and fn's panic value is lost. Pre-fix code could not reach
		// this scenario because fn's panic wedged the lock; surfacing
		// observer failures is preferred over silently swallowing them.
		if phase2 != nil {
			phase2()
		}

		if r != nil {
			panic(r)
		}
	}()

	fn(txn)

	txn.afterState = d.store.StateVector()

	// Squash adjacent same-client ContentString runs before encoding so that
	// the incremental update sent to peers is already compact.
	// Note: squashing happens before per-type observers fire. Observers therefore
	// see merged runs rather than individual character items. This is intentional:
	// the YTextEvent API does not expose raw Items, and firing after squash
	// removes the need for a second lock cycle.
	squashRuns(txn)
}
```

- [ ] **Step 4: Run the test — verify it PASSES**

Run: `cd /Users/nimit/Documents/Eukarya/ygo && go test ./crdt/ -run TestTransact_PanicReleasesLock -v -timeout 10s`

Expected: PASS.

- [ ] **Step 5: Run the full test suite**

Run: `cd /Users/nimit/Documents/Eukarya/ygo && go test -race ./... -timeout 120s`

Expected: all packages pass — the refactor preserves normal-path behavior.

- [ ] **Step 6: Commit**

```bash
cd /Users/nimit/Documents/Eukarya/ygo && git add crdt/doc.go crdt/crdt_test.go && git commit -m "fix(crdt): release d.mu on panic in Transact (#9)

Wrap Transact's body in a deferred recover() that unconditionally
releases d.mu. On panic, best-effort finalizes txn, builds the Phase 2
plan under a protective recover, unlocks, fires observers with
whatever partial state was captured, and re-raises the original
panic.

Matches the behavior of Yjs JS and yrs: transactions commit partial
state on exception; observers see it. Rollback is explicitly not
supported.

Closes #9."
```

---

## Task 3: Panic observer-firing behavior tests

**Files:**
- Test: `crdt/crdt_test.go` (new tests)

Lock the documented panic semantics into regression tests.

- [ ] **Step 1: Add observer-firing tests**

Append to `/Users/nimit/Documents/Eukarya/ygo/crdt/crdt_test.go`:

```go
func TestTransact_PanicFiresOnUpdateWithPartialState(t *testing.T) {
	doc := New()

	var received [][]byte
	doc.OnUpdate(func(update []byte, _ any) {
		received = append(received, update)
	})

	func() {
		defer func() { _ = recover() }()
		doc.Transact(func(txn *Transaction) {
			doc.GetMap("m").Set(txn, "k1", "v1")
			doc.GetMap("m").Set(txn, "k2", "v2")
			panic("boom")
		})
	}()

	require.Len(t, received, 1, "OnUpdate should fire exactly once with partial state")
	assert.NotEmpty(t, received[0], "partial update must be non-empty")

	// Apply the partial update to a fresh doc and verify both keys are present.
	replica := New()
	require.NoError(t, ApplyUpdateV1(replica, received[0], nil))
	got1, ok1 := replica.GetMap("m").Get("k1")
	require.True(t, ok1)
	assert.Equal(t, "v1", got1)
	got2, ok2 := replica.GetMap("m").Get("k2")
	require.True(t, ok2)
	assert.Equal(t, "v2", got2)
}

func TestTransact_PanicFiresOnAfterTransaction(t *testing.T) {
	doc := New()

	var txnSeen *Transaction
	doc.OnAfterTransaction(func(txn *Transaction) {
		txnSeen = txn
	})

	func() {
		defer func() { _ = recover() }()
		doc.Transact(func(txn *Transaction) {
			doc.GetMap("m").Set(txn, "k", "v")
			panic("boom")
		})
	}()

	require.NotNil(t, txnSeen, "OnAfterTransaction should fire on panic")
	assert.NotEmpty(t, txnSeen.changed, "txn.changed must reflect the partial mutation")
}

func TestTransact_PanicWithNoMutationsDoesNotFireOnUpdate(t *testing.T) {
	doc := New()

	var called int
	doc.OnUpdate(func(update []byte, _ any) {
		called++
	})

	func() {
		defer func() { _ = recover() }()
		doc.Transact(func(txn *Transaction) {
			panic("immediate")
		})
	}()

	assert.Equal(t, 0, called, "OnUpdate must not fire when fn panics with no mutations")
}

func TestTransact_PanicReraises(t *testing.T) {
	doc := New()

	var caught any
	func() {
		defer func() { caught = recover() }()
		doc.Transact(func(txn *Transaction) {
			panic("original message")
		})
	}()

	assert.Equal(t, "original message", caught, "original panic value must be re-raised")
}

func TestTransact_NormalPathFiresOnUpdateOnce(t *testing.T) {
	doc := New()

	var calls int
	doc.OnUpdate(func(update []byte, _ any) {
		calls++
	})

	doc.Transact(func(txn *Transaction) {
		doc.GetMap("m").Set(txn, "k", "v")
	})

	assert.Equal(t, 1, calls, "regression: normal-path OnUpdate must fire exactly once")
}
```

- [ ] **Step 2: Run the new tests — verify they PASS**

Run: `cd /Users/nimit/Documents/Eukarya/ygo && go test ./crdt/ -run "TestTransact_Panic|TestTransact_Normal" -v -timeout 30s`

Expected: all 5 new tests pass, and the previously-added `TestTransact_PanicReleasesLock` also passes.

- [ ] **Step 3: Run the full test suite with race detector**

Run: `cd /Users/nimit/Documents/Eukarya/ygo && go test -race ./... -timeout 120s`

Expected: all pass.

- [ ] **Step 4: Commit**

```bash
cd /Users/nimit/Documents/Eukarya/ygo && git add crdt/crdt_test.go && git commit -m "test(crdt): cover Transact panic observer-firing semantics

Locks in the contract: on panic, OnUpdate fires with a non-empty
partial update, OnAfterTransaction fires with the partial txn,
panics with no mutations do not fire OnUpdate, and the original
panic re-raises. Plus a regression test for normal-path firing."
```

---

## Task 4: Update `Doc.Transact` godoc

**Files:**
- Modify: `crdt/doc.go` (Transact godoc block, lines ~224-232)

- [ ] **Step 1: Replace the Transact godoc comment**

In `/Users/nimit/Documents/Eukarya/ygo/crdt/doc.go`, find the existing `// Transact executes fn...` comment block immediately above `func (d *Doc) Transact`. Replace it with:

```go
// Transact executes fn inside a transaction. All insertions and deletions made
// during fn are batched; observers fire once after fn returns.
//
// Observers are intentionally fired OUTSIDE the document lock. This means:
//   - Observer callbacks may safely call back into any Doc method (Transact,
//     GetArray, ApplyUpdate, etc.) without deadlocking.
//   - The document may be modified by another goroutine between the time fn
//     returns and the time observers fire; observers should treat txn as a
//     snapshot of what changed, not a live view of the current state.
//
// Panic semantics:
//   - If fn panics (or any Phase 1 work panics), d.mu is released via defer.
//   - Observers fire with whatever partial state was committed before the
//     panic: OnUpdate receives a non-empty V1 update describing the
//     mutations that completed; per-type, deep, and OnAfterTransaction
//     observers fire for what was recorded in txn.changed.
//   - The original panic is re-raised to the caller after observers fire.
//   - Rollback is NOT supported. The in-memory doc reflects fn's partial
//     work. Callers who need atomicity must implement it above Transact.
//     This matches the behavior of Yjs JS and the Rust yrs implementation;
//     yrs explicitly directs users to UndoManager for transactional undo.
//   - If fn panics and an observer callback also panics during the partial
//     firing, the observer's panic reaches the caller and the original
//     fn panic value is lost.
func (d *Doc) Transact(fn func(*Transaction), origin ...any) {
```

Leave the function body unchanged (it was replaced in Task 2).

- [ ] **Step 2: Run tests to confirm no break**

Run: `cd /Users/nimit/Documents/Eukarya/ygo && go test ./crdt/ -v -timeout 60s 2>&1 | tail -5`

Expected: all Transact-related tests still pass. Doc comments don't affect behavior, so this is a sanity check.

- [ ] **Step 3: Commit**

```bash
cd /Users/nimit/Documents/Eukarya/ygo && git add crdt/doc.go && git commit -m "docs(crdt): document Transact panic semantics

Spells out the new contract added in the panic-safety fix: lock
release, observer firing with partial state, panic re-raise, and
the explicit absence of rollback. References Yjs JS and yrs."
```

---

## Task 5: Re-enable Apply panic test and update Apply godoc

**Files:**
- Modify: `provider/websocket/inject.go` (Apply godoc, ~lines 169-203)
- Modify: `provider/websocket/inject_test.go` (replace the commented-out block left by issue #8's cleanup)

- [ ] **Step 1: Update `Apply` godoc in `provider/websocket/inject.go`**

Find the `Apply` function's doc comment in `/Users/nimit/Documents/Eukarya/ygo/provider/websocket/inject.go`. It currently ends with a block that starts `// NOTE: a panic inside fn propagates to the caller...` and states `Callers MUST ensure fn does not panic.` Replace that final NOTE block with:

```go
// NOTE: a panic inside fn propagates to the caller. The OnUpdate
// subscription is cleaned up via defer, so no listener leaks. Starting
// in v1.1.1, Doc.Transact is panic-safe: the doc lock is released and
// observers fire with whatever partial state fn committed before the
// panic. Apply therefore broadcasts that partial state to peers and
// triggers persistence before re-raising the panic. Callers should
// still avoid panicking fn bodies in production because the doc is
// left with unrolled-back partial mutations; recover and either
// reconcile via sync or recreate the doc from persistence.
```

(Replace ONLY the NOTE paragraph — leave the IMPORTANT / goroutine-outlive / bypass paragraphs above it unchanged.)

- [ ] **Step 2: Replace the commented-out panic test with the re-enabled version**

In `/Users/nimit/Documents/Eukarya/ygo/provider/websocket/inject_test.go`, find the block near `TestUnit_Apply_FnPanic_BeforeTransact_NoLeak` that starts with a comment about the test being omitted (it was added in issue #8's fixup commit `9f6c94b`). The comment reads something like:

```go
// NOTE: a test that panics INSIDE transact is intentionally omitted.
// The pre-existing crdt.Doc.Transact panic-unlock bug (tracked as a
// separate follow-up issue) means such a panic leaves d.mu held, and
// Apply's defer-unsub — which needs d.mu — deadlocks. Apply's doc
// comment instructs callers: "fn MUST NOT panic." The BeforeTransact
// test below is the maximal safety guarantee we can verify today.
```

Replace that comment block (lines only — leave `TestUnit_Apply_FnPanic_BeforeTransact_NoLeak` below it alone) with this new test:

```go
func TestUnit_Apply_FnPanic_InsideTransact_BroadcastsPartialState(t *testing.T) {
	srv := ygws.NewServer()
	httpSrv := httptest.NewServer(http.HandlerFunc(srv.ServeHTTP))
	t.Cleanup(httpSrv.Close)
	conn := dial(t, httpSrv, "room")
	peerDoc := crdt.New()
	drainHandshake(t, conn, peerDoc)

	// Apply that mutates then panics inside transact.
	var panicked any
	func() {
		defer func() { panicked = recover() }()
		_ = srv.Apply(context.Background(), "room", func(doc *crdt.Doc, transact func(func(*crdt.Transaction))) {
			m := doc.GetMap("m")
			transact(func(txn *crdt.Transaction) {
				m.Set(txn, "k", "v")
				panic("boom")
			})
		})
	}()
	require.Equal(t, "boom", panicked, "original panic must propagate")

	// Peer receives the partial-state broadcast — the Set completed before
	// the panic, so the partial update carries "k" = "v".
	outerType, payload := readOne(t, conn, 2*time.Second)
	assert.Equal(t, uint64(0), outerType)
	_, _ = ygsync.ApplySyncMessage(peerDoc, payload, nil)
	got, ok := peerDoc.GetMap("m").Get("k")
	require.True(t, ok, "peer should have received the partial mutation")
	assert.Equal(t, "v", got)

	// Server doc also reflects the partial mutation.
	serverDoc := srv.GetDoc("room")
	require.NotNil(t, serverDoc)
	got, ok = serverDoc.GetMap("m").Get("k")
	require.True(t, ok)
	assert.Equal(t, "v", got)

	// Second Apply on the same room succeeds — proves the doc lock was
	// released and the OnUpdate subscription from the first Apply was
	// cleaned up via defer.
	require.NoError(t, srv.Apply(context.Background(), "room", func(doc *crdt.Doc, transact func(func(*crdt.Transaction))) {
		m := doc.GetMap("m")
		transact(func(txn *crdt.Transaction) { m.Set(txn, "k2", "v2") })
	}))
}
```

- [ ] **Step 3: Run the new test**

Run: `cd /Users/nimit/Documents/Eukarya/ygo && go test ./provider/websocket/ -run TestUnit_Apply_FnPanic -v -timeout 30s 2>&1 | tail -15`

Expected: both panic tests (`InsideTransact_BroadcastsPartialState` and the existing `BeforeTransact_NoLeak`) pass.

- [ ] **Step 4: Run the full test suite with race detector**

Run: `cd /Users/nimit/Documents/Eukarya/ygo && go test -race ./... -timeout 120s 2>&1 | tail -5`

Expected: all packages pass.

- [ ] **Step 5: Commit**

```bash
cd /Users/nimit/Documents/Eukarya/ygo && git add provider/websocket/inject.go provider/websocket/inject_test.go && git commit -m "test(ws): re-enable in-transact panic test after #9 fix

Restores the coverage dropped in 9f6c94b. With Doc.Transact now
panic-safe, a panic inside fn broadcasts the partial mutations to
peers (matching Yjs/yrs behavior) and Apply's subscription is cleaned
up cleanly. Godoc updated to reflect that fn panics no longer wedge
the room — callers are still advised to avoid them, but they are no
longer catastrophic."
```

---

## Task 6: CHANGELOG and RELEASE_NOTES for v1.1.1

**Files:**
- Modify: `CHANGELOG.md` (prepend v1.1.1 entry)
- Overwrite: `RELEASE_NOTES.md`

- [ ] **Step 1: Add v1.1.1 entry to `CHANGELOG.md`**

Open `/Users/nimit/Documents/Eukarya/ygo/CHANGELOG.md` and locate the `## [1.1.0]` heading. Insert immediately before it:

```markdown
## [1.1.1] — 2026-04-21

### Fixed

- **`Doc.Transact` lock leak on panic (#9)**: if `fn` (or any Phase 1 work) panicked, `d.mu` remained held forever, wedging the document. Any subsequent operation that needed the lock — `GetMap`, `GetText`, `ApplyUpdateV1`, a further `Transact`, an `OnUpdate` subscribe/unsubscribe — deadlocked. Transact now wraps its body in a deferred `recover()` that releases `d.mu` on every exit path.

### Changed

- **`Doc.Transact` panic semantics are now explicit.** On panic: observers fire with whatever partial state `fn` committed (matching Yjs JS and `yrs`), then the original panic is re-raised. Rollback is not provided — callers needing atomicity should recover and reconcile via sync or recreate the doc from persistence. Previously behavior was undefined (the caller deadlocked before any observer could run).
- **`websocket.Server.Apply` godoc** updated: a panicking `fn` no longer wedges the room. The caveat is softened accordingly; partial-state broadcasts are now the documented behavior.

```

- [ ] **Step 2: Overwrite `RELEASE_NOTES.md`**

Overwrite `/Users/nimit/Documents/Eukarya/ygo/RELEASE_NOTES.md` with:

```markdown
## What's new

- **`Doc.Transact` is now panic-safe (#9).** Previously, a panic inside the transaction callback left the document's write lock held forever, wedging every subsequent operation on the doc — including `websocket.Server.Apply`'s cleanup path. Transact now releases the lock on every exit path.
- **Documented panic semantics.** On panic, observers fire with the partial state that was committed before the panic (matching Yjs JS and `yrs`), then the original panic is re-raised. Rollback is not supported; callers needing atomicity should recover and reconcile.
- **`websocket.Server.Apply` no longer wedges rooms on panic.** The "fn MUST NOT panic" caveat is softened — panics now broadcast partial state to peers and trigger persistence, just like any other mutation.

## Install

```
go get github.com/reearth/ygo@v1.1.1
```

See [CHANGELOG.md](https://github.com/reearth/ygo/blob/main/CHANGELOG.md) for full details.
```

- [ ] **Step 3: Commit**

```bash
cd /Users/nimit/Documents/Eukarya/ygo && git add CHANGELOG.md RELEASE_NOTES.md && git commit -m "docs: changelog and release notes for v1.1.1 (#9)"
```

---

## Final verification

- [ ] **Step 1: Run the full test suite with the race detector**

Run: `cd /Users/nimit/Documents/Eukarya/ygo && go test -race -timeout 120s ./...`

Expected: all packages pass.

- [ ] **Step 2: Run vet, build, lint, vuln scan**

Run in parallel:
```bash
cd /Users/nimit/Documents/Eukarya/ygo && go vet ./... && go build ./...
cd /Users/nimit/Documents/Eukarya/ygo && golangci-lint run --timeout 5m ./...
cd /Users/nimit/Documents/Eukarya/ygo && govulncheck ./...
```

Expected: all clean.

- [ ] **Step 3: Check git log**

Run: `git log --oneline chore/transact-panic-safety ^main`

Expected: 7 commits — spec + refactor + panic-safety fix + observer tests + godoc update + Apply test + changelog.

---

## Self-review notes

- **Spec coverage:**
  - Lock release on panic → Task 2
  - Observers fire with partial state → Task 2 + regression tests in Task 3
  - Panic re-raise → Task 3 (`TestTransact_PanicReraises`)
  - No rollback (documentation only) → Task 4 godoc
  - Re-enable Apply panic test + update Apply godoc → Task 5
  - CHANGELOG / RELEASE_NOTES → Task 6
  - Follow-up issue reference — user's choice after PR lands (not in plan)

- **Placeholder scan:** No TBDs / TODOs / "similar to Task N" references. Each task's code is complete and copy-pasteable.

- **Type consistency:** `buildPhase2(d *Doc, txn *Transaction, orig any) func()` is defined in Task 1 and called in Task 2. Signature matches.

- **One note about ordering:** Task 2 replaces the Task 1 `Transact` body wholesale. Task 4 only modifies the doc comment block above `Transact`. The `buildPhase2` helper added in Task 1 is untouched from Task 2 onward.