package websocket_test

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/reearth/ygo/crdt"
	"github.com/reearth/ygo/persistence"
	ygws "github.com/reearth/ygo/provider/websocket"
)

// foldSpy wraps a real store and records the write index at which each Compact
// was attempted, failing the attempts a test selects. Recording the WRITE INDEX
// rather than a bare count is what lets these tests assert on the CADENCE
// (the gap between attempts), which is the property under test — an attempt
// count alone cannot tell "backed off" from "gave up".
//
// Not safe for concurrent use; every test here drives it from one goroutine.
type foldSpy struct {
	persistence.VersionedPersistence
	failWhile func(attempt int) bool
	writeIdx  *int
	attempts  []int
}

func (f *foldSpy) Compact(ctx context.Context, room string, keep int) (int, error) {
	f.attempts = append(f.attempts, *f.writeIdx)
	if f.failWhile != nil && f.failWhile(len(f.attempts)) {
		return 0, errors.New("fold unavailable")
	}
	return f.VersionedPersistence.Compact(ctx, room, keep)
}

// newFoldSpy builds a MemoryPersistence whose fold can be made to fail.
func newFoldSpy(compactEvery int, failWhile func(attempt int) bool) (*ygws.MemoryPersistence, *foldSpy, *int) {
	idx := 0
	spy := &foldSpy{
		VersionedPersistence: persistence.NewMemoryPersistence(),
		failWhile:            failWhile,
		writeIdx:             &idx,
	}
	ad := persistence.NewLegacyAdapter(spy)
	ad.KeepVersions = 1
	p := ygws.NewMemoryPersistenceForTest(ad)
	p.CompactEvery = compactEvery
	return p, spy, &idx
}

// gaps reports the distance between consecutive fold attempts, in writes.
func gaps(attempts []int) []int {
	if len(attempts) < 2 {
		return nil
	}
	out := make([]int, 0, len(attempts)-1)
	for i := 1; i < len(attempts); i++ {
		out = append(out, attempts[i]-attempts[i-1])
	}
	return out
}

// seqWriter appends one character per write to a SINGLE document, so the
// expected content accumulates across a whole test (storeN builds a fresh doc
// per call, which would make a content assertion vacuous). It also advances the
// spy's write index, which is what the cadence assertions are measured in.
type seqWriter struct {
	p    *ygws.MemoryPersistence
	room string
	doc  *crdt.Doc
	txt  *crdt.YText
	sv   crdt.StateVector
	idx  *int
	n    int
}

func newSeqWriter(t *testing.T, p *ygws.MemoryPersistence, room string, idx *int) *seqWriter {
	t.Helper()
	d := crdt.New()
	return &seqWriter{p: p, room: room, doc: d, txt: d.GetText("t"), idx: idx}
}

// write stores one more update and returns the full expected text so far.
func (w *seqWriter) write(t *testing.T) string {
	t.Helper()
	*w.idx = w.n
	w.n++
	w.doc.Transact(func(txn *crdt.Transaction) { w.txt.Insert(txn, w.txt.Len(), "a", nil) })
	require.NoError(t, w.p.StoreUpdate(w.room, crdt.EncodeStateAsUpdateV1(w.doc, w.sv)))
	w.sv = w.doc.StateVector()
	return w.txt.ToString()
}

const backoffEvery = 10

// A healthy fold must be attempted exactly once per CompactEvery writes. This
// is the control: every other test here is only meaningful if the undamped
// cadence is what it claims to be.
func TestUnit_MemoryPersistence_HealthyFoldCadenceIsUnchanged(t *testing.T) {
	p, spy, idx := newFoldSpy(backoffEvery, nil)
	w := newSeqWriter(t, p, "room", idx)
	want := ""
	for i := 0; i < 100; i++ {
		want = w.write(t)
	}
	require.Len(t, spy.attempts, 100/backoffEvery)
	for _, g := range gaps(spy.attempts) {
		require.Equal(t, backoffEvery, g)
	}
	require.NotEmpty(t, want)
}

// A permanently failing fold must not be retried on every write. Left undamped
// the ledger re-fires forever (outstanding() stays over the threshold), costing
// O(writes) attempts each paying a full merge over a log the failed attempt
// could not shrink — quadratic total work (#239).
func TestUnit_MemoryPersistence_PermanentlyFailingFoldBacksOff(t *testing.T) {
	const writes = 800
	p, spy, idx := newFoldSpy(backoffEvery, func(int) bool { return true })
	w := newSeqWriter(t, p, "room", idx)
	for i := 0; i < writes; i++ {
		w.write(t)
	}

	undamped := writes - backoffEvery + 1 // what re-firing every write costs
	t.Logf("attempts=%d (undamped would be %d), gaps=%v", len(spy.attempts), undamped, gaps(spy.attempts))

	// Geometric growth capped at 64x the base threshold: the tail runs at one
	// attempt per 64*CompactEvery writes, so 800 writes must stay far below the
	// undamped count while still making steady progress.
	require.Less(t, len(spy.attempts), 25,
		"a permanently failing fold is being retried far too often")

	// Gaps must never shrink: backoff is monotone while failures continue.
	g := gaps(spy.attempts)
	for i := 1; i < len(g); i++ {
		require.GreaterOrEqual(t, g[i], g[i-1], "backoff gap shrank without a success")
	}
}

// Backoff must be capped, not unbounded: a store that stays down for a long
// time must still be retried periodically, or a recovered store would never be
// noticed and the log would grow without bound.
func TestUnit_MemoryPersistence_BackoffIsCappedNotAbandoned(t *testing.T) {
	const writes = 4000
	p, spy, idx := newFoldSpy(backoffEvery, func(int) bool { return true })
	w := newSeqWriter(t, p, "room", idx)
	for i := 0; i < writes; i++ {
		w.write(t)
	}
	g := gaps(spy.attempts)
	require.NotEmpty(t, g)
	maxGap := 0
	for _, v := range g {
		if v > maxGap {
			maxGap = v
		}
	}
	t.Logf("attempts=%d maxGap=%d", len(spy.attempts), maxGap)
	require.LessOrEqual(t, maxGap, 64*backoffEvery,
		"backoff grew past its cap; un-folded records are effectively unbounded")
	require.GreaterOrEqual(t, len(spy.attempts), writes/(64*backoffEvery),
		"backoff stopped retrying altogether")
}

// A transient failure must not leave the room permanently degraded: once a fold
// succeeds, a LATER failure must start its backoff from the base cadence again
// rather than resuming the old geometric progression.
//
// Asserting only that the cadence returns to CompactEvery after the success
// does NOT test this — retryAt is an absolute appended-mark, so after a
// successful fold it already lies in the past and outstanding() >= CompactEvery
// becomes the binding constraint again on its own. That assertion passes with
// the reset deleted (verified by mutation). The failure counter is the state
// that actually survives, so the reset has to be observed through a second
// failure run.
func TestUnit_MemoryPersistence_BackoffResetsAfterSuccess(t *testing.T) {
	// Attempts 1-3 fail, attempt 4 succeeds, everything after fails again.
	p, spy, idx := newFoldSpy(backoffEvery, func(attempt int) bool { return attempt != 4 })
	w := newSeqWriter(t, p, "room", idx)
	for i := 0; i < 600; i++ {
		w.write(t)
	}
	g := gaps(spy.attempts)
	t.Logf("attempts=%d gaps=%v", len(spy.attempts), g)
	require.GreaterOrEqual(t, len(g), 6, "not enough attempts to observe a second failure run")

	// g[0..2] are the first failure run: N, 2N, 4N.
	require.Equal(t, []int{backoffEvery, 2 * backoffEvery, 4 * backoffEvery}, g[:3])

	// g[3] follows the SUCCESS at attempt 4 — base cadence.
	require.Equal(t, backoffEvery, g[3], "cadence did not return to CompactEvery after a successful fold")

	// g[4] follows the first failure of the SECOND run. If the success did not
	// clear the failure counter this resumes the old progression (8N) instead.
	require.Equal(t, backoffEvery, g[4],
		"backoff did not restart from the base cadence after a success; the failure counter was not reset")
	require.Equal(t, 2*backoffEvery, g[5], "second failure run is not doubling from the base")
}

// Backing off changes WHEN records are folded, never whether content survives:
// an un-folded record is still a stored record.
func TestUnit_MemoryPersistence_BackoffPreservesContent(t *testing.T) {
	p, _, idx := newFoldSpy(backoffEvery, func(int) bool { return true })
	w := newSeqWriter(t, p, "room", idx)
	want := ""
	for i := 0; i < 120; i++ {
		want = w.write(t)
	}
	require.Equal(t, want, docTextAfterReload(t, p, "room"))
}

// parkingFailStore fails its first failFirst folds, then succeeds — and parks
// inside the successful one so a test can land a write in the fold window.
type parkingFailStore struct {
	*persistence.MemoryPersistence
	failFirst int
	calls     atomic.Int32
	entered   chan struct{}
	proceed   chan struct{}
}

func (s *parkingFailStore) Compact(ctx context.Context, room string, keep int) (int, error) {
	n := int(s.calls.Add(1))
	if n <= s.failFirst {
		return 0, errors.New("fold unavailable")
	}
	if n == s.failFirst+1 {
		close(s.entered)
		<-s.proceed
	}
	return s.MemoryPersistence.Compact(ctx, room, keep)
}

// The reset-on-success must survive a fold whose ledger is NOT dropped.
//
// A successful fold that leaves nothing outstanding has its ledger deleted by
// dropIfIdleLocked, which clears the failure counter incidentally — so the
// reset is invisible there, and asserting on cadence alone passes even with
// the reset removed. The counter only does work when a write lands inside the
// fold window, leaving the entry alive afterwards. That is the interleaving
// below, made deterministic by parking in the wrapped store:
//
//	3 explicit folds fail ───────► failures climbs to 3
//	C4 reads its mark ───────────► parks in the store, before folding
//	a concurrent write lands ────► outstanding must become 1
//	C4 released ─────────────────► folds, succeeds; the entry SURVIVES
//	                               its failure counter must read 0
//
// CompactEvery is set high so StoreUpdate's threshold never fires and every
// fold here is driven explicitly — otherwise the concurrent write would
// recurse into a nested fold and the test would assert on the wrong mechanism.
func TestUnit_MemoryPersistence_BackoffResetSurvivesUndroppedLedger(t *testing.T) {
	const failFirst = 3
	store := &parkingFailStore{
		MemoryPersistence: persistence.NewMemoryPersistence(),
		failFirst:         failFirst,
		entered:           make(chan struct{}),
		proceed:           make(chan struct{}),
	}
	adapter := persistence.NewLegacyAdapter(store)
	adapter.KeepVersions = 1
	p := ygws.NewMemoryPersistenceForTest(adapter)
	p.CompactEvery = 1_000_000 // never self-compact; drive every fold explicitly

	storeN(t, p, "room", 3)

	for i := 0; i < failFirst; i++ {
		require.Error(t, p.Compact(context.Background(), "room"))
	}
	failures, retryAt := ygws.MemoryPersistenceBackoffState(p, "room")
	require.Equal(t, failFirst, failures, "precondition: consecutive failures were not recorded")
	require.Positive(t, retryAt, "precondition: no backoff mark was set")

	done := make(chan error, 1)
	go func() { done <- p.Compact(context.Background(), "room") }()
	waitEntered(t, store.entered, "the succeeding Compact")

	// Lands while the fold is parked, so its mark cannot cover this write and
	// the entry stays alive once the fold reports.
	concurrentDoc := crdt.New()
	concurrentTxt := concurrentDoc.GetText("t")
	concurrentDoc.Transact(func(txn *crdt.Transaction) { concurrentTxt.Insert(txn, 0, "b", nil) })
	require.NoError(t, p.StoreUpdate("room", crdt.EncodeStateAsUpdateV1(concurrentDoc, nil)))

	close(store.proceed)
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(10 * time.Second):
		t.Fatal("the succeeding Compact never returned")
	}

	require.Equal(t, 1, ygws.MemoryPersistencePendingCount(p, "room"),
		"precondition: the ledger must have survived the fold for this test to mean anything")

	failures, retryAt = ygws.MemoryPersistenceBackoffState(p, "room")
	require.Zero(t, failures, "a successful fold did not clear the consecutive-failure count")
	require.Zero(t, retryAt, "a successful fold did not clear the backoff mark")
}

// Backoff must gate ONLY the automatic trigger in StoreUpdate. LoadDoc folds
// before materialising so that later loads are cheap again; if it ever started
// honouring the backoff mark, a room whose fold had failed once would keep
// re-merging its whole log on every load instead — the cost LoadDoc's fold
// exists to avoid, reappearing silently.
//
// The same holds for an explicit Compact call, which is the "fold now" API.
func TestUnit_MemoryPersistence_BackoffDoesNotGateLoadDocOrExplicitCompact(t *testing.T) {
	// Fails once, then succeeds — so a pending backoff exists, and the fold
	// that LoadDoc drives can still be observed collapsing the records.
	p, spy, idx := newFoldSpy(backoffEvery, func(attempt int) bool { return attempt == 1 })
	w := newSeqWriter(t, p, "room", idx)
	for i := 0; i < backoffEvery; i++ {
		w.write(t)
	}
	require.Len(t, spy.attempts, 1, "precondition: the threshold fired exactly once")

	// One more write, nowhere near the backed-off mark: the automatic trigger
	// must stay quiet.
	want := w.write(t)
	require.Len(t, spy.attempts, 1, "the automatic trigger fired while backed off")

	// LoadDoc must fold anyway, and must still return the full content.
	require.Equal(t, want, docTextAfterReload(t, p, "room"))
	require.Len(t, spy.attempts, 2, "LoadDoc did not fold while a backoff was pending")
	require.Equal(t, 1, ygws.MemoryPersistenceRecordCount(p, "room"),
		"LoadDoc's fold did not collapse the records")

	// An explicit Compact is the "fold now" API and must not be gated either.
	require.NoError(t, p.Compact(context.Background(), "room"))
	require.Len(t, spy.attempts, 3, "an explicit Compact was gated by the backoff")
}
