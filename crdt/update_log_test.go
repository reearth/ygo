package crdt

import (
	"fmt"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func loggedOrigins(updates []LoggedUpdate) []any {
	origins := make([]any, len(updates))
	for i, u := range updates {
		origins[i] = u.Origin
	}
	return origins
}

// replay applies base and then each update to a fresh document.
func replay(t *testing.T, base []byte, updates []LoggedUpdate) *Doc {
	t.Helper()
	replica := New(WithClientID(1 << 30))
	require.NoError(t, ApplyUpdateV1(replica, base, nil))
	for _, u := range updates {
		require.NoError(t, ApplyUpdateV1(replica, u.Update, nil))
		if u.Parked != nil {
			require.NoError(t, ApplyUpdateV1(replica, u.Parked, nil))
		}
	}
	return replica
}

// The observers of two concurrent commits run in reverse commit order (the
// first committer's observer is held until the second's has run), and the
// second's observer drains both updates, in commit order, with the very
// bytes each transaction's observers were handed.
func TestUnit_UpdateLog_DrainsInCommitOrderWhenObserversRunReversed(t *testing.T) {
	doc := New(WithClientID(1))
	txt := doc.GetText("t")
	log := doc.OpenUpdateLog()
	defer log.Close()

	firstObserving := make(chan struct{})
	releaseFirst := make(chan struct{})
	var mu sync.Mutex
	var observed, drained []LoggedUpdate
	doc.OnUpdate(func(update []byte, origin any) {
		if origin == "first" {
			close(firstObserving)
			<-releaseFirst
		}
		mu.Lock()
		defer mu.Unlock()
		observed = append(observed, LoggedUpdate{Update: update, Origin: origin})
		_, updates := log.Drain()
		drained = append(drained, updates...)
	})

	done := make(chan struct{})
	go func() {
		defer close(done)
		doc.Transact(func(txn *Transaction) { txt.Insert(txn, 0, "first", nil) }, "first")
	}()
	<-firstObserving
	doc.Transact(func(txn *Transaction) { txt.Insert(txn, 0, "second ", nil) }, "second")
	close(releaseFirst)
	<-done

	require.Equal(t, []any{"second", "first"}, loggedOrigins(observed), "observers ran in reverse commit order")
	require.Equal(t, []any{"first", "second"}, loggedOrigins(drained), "the log is in commit order")
	assert.Same(t, &observed[1].Update[0], &drained[0].Update[0], "the first commit's logged bytes are its observers' bytes")
	assert.Same(t, &observed[0].Update[0], &drained[1].Update[0], "the second commit's logged bytes are its observers' bytes")
	assert.Zero(t, log.Pending())
}

// Applying the base and then each drained update in order reproduces the
// source document as it stood after each commit: local inserts, a remote
// update, a commit that only deletes, a map write and an empty transaction.
func TestUnit_UpdateLog_ReplayReproducesEachCommit(t *testing.T) {
	src := New(WithClientID(1))
	txt := src.GetText("t")
	m := src.GetMap("m")
	src.Transact(func(txn *Transaction) { txt.Insert(txn, 0, "before the log", nil) })

	log := src.OpenUpdateLog()
	defer log.Close()

	type state struct {
		sv   []byte
		text string
		m    map[string]any
	}
	snapshot := func(d *Doc) state {
		return state{EncodeStateVectorV1(d), d.GetText("t").ToString(), d.GetMap("m").Entries()}
	}

	remote := New(WithClientID(2))
	require.NoError(t, ApplyUpdateV1(remote, EncodeStateAsUpdateV1(src, nil), nil))
	remoteTxt := remote.GetText("t")

	commits := []struct {
		origin any
		commit func()
	}{
		{"insert", func() {
			src.Transact(func(txn *Transaction) { txt.Insert(txn, 0, "hello ", nil) }, "insert")
		}},
		{"remote", func() {
			remote.Transact(func(txn *Transaction) { remoteTxt.Insert(txn, remoteTxt.Len(), " and after", nil) })
			require.NoError(t, ApplyUpdateV1(src, EncodeStateAsUpdateV1(remote, nil), "remote"))
		}},
		{"delete", func() {
			src.Transact(func(txn *Transaction) { txt.Delete(txn, 0, len("hello before ")) }, "delete")
		}},
		{"map", func() {
			src.Transact(func(txn *Transaction) { m.Set(txn, "k", "v") }, "map")
		}},
		{"empty", func() {
			src.Transact(func(*Transaction) {}, "empty")
		}},
	}
	want := make([]state, 0, 1+len(commits))
	want = append(want, snapshot(src))
	wantOrigins := make([]any, 0, len(commits))
	for _, c := range commits {
		c.commit()
		want = append(want, snapshot(src))
		wantOrigins = append(wantOrigins, c.origin)
	}
	require.Equal(t, "the log and after", txt.ToString())
	require.Equal(t, want[2].sv, want[3].sv, "the delete commit only deletes")

	base, updates := log.Drain()
	require.NotNil(t, base)
	require.Equal(t, wantOrigins, loggedOrigins(updates))

	for i := range want {
		assert.Equal(t, want[i], snapshot(replay(t, base, updates[:i])), "after %d logged commits", i)
	}
}

// The base is recorded once, by the first transaction to commit after the
// log opens, and is the document's state just before that transaction,
// including state the document held before the log opened. Each log records
// its own base.
func TestUnit_UpdateLog_BaseIsStateBeforeFirstCommit(t *testing.T) {
	doc := New(WithClientID(1))
	txt := doc.GetText("t")
	doc.Transact(func(txn *Transaction) { txt.Insert(txn, 0, "old", nil) })

	first := doc.OpenUpdateLog()
	defer first.Close()
	base, updates := first.Drain()
	assert.Nil(t, base, "no transaction has committed, so no base")
	assert.Empty(t, updates)

	doc.Transact(func(txn *Transaction) { txt.Insert(txn, 3, " a", nil) }, "a")
	second := doc.OpenUpdateLog()
	defer second.Close()
	doc.Transact(func(txn *Transaction) { txt.Insert(txn, 5, " b", nil) }, "b")

	base, updates = first.Drain()
	require.NotNil(t, base)
	assert.Equal(t, "old", replay(t, base, nil).GetText("t").ToString())
	assert.Equal(t, []any{"a", "b"}, loggedOrigins(updates))
	assert.Equal(t, "old a b", replay(t, base, updates).GetText("t").ToString())

	base, updates = second.Drain()
	require.NotNil(t, base)
	assert.Equal(t, "old a", replay(t, base, nil).GetText("t").ToString())
	assert.Equal(t, []any{"b"}, loggedOrigins(updates))

	doc.Transact(func(txn *Transaction) { txt.Insert(txn, 0, "c ", nil) }, "c")
	base, updates = first.Drain()
	assert.Nil(t, base, "the base is handed over once")
	assert.Equal(t, []any{"c"}, loggedOrigins(updates))
}

// A transaction whose fn panics commits what it did before the panic, and the
// log holds the same update its OnUpdate observers receive.
func TestUnit_UpdateLog_PanickingTransactionIsLogged(t *testing.T) {
	doc := New(WithClientID(1))
	txt := doc.GetText("t")
	log := doc.OpenUpdateLog()
	defer log.Close()
	var observed []byte
	doc.OnUpdate(func(update []byte, _ any) { observed = update })

	require.PanicsWithValue(t, "boom", func() {
		doc.Transact(func(txn *Transaction) {
			txt.Insert(txn, 0, "partial", nil)
			panic("boom")
		}, "panicky")
	})

	base, updates := log.Drain()
	require.Len(t, updates, 1)
	assert.Equal(t, "panicky", updates[0].Origin)
	assert.Same(t, &observed[0], &updates[0].Update[0])
	assert.Equal(t, "partial", replay(t, base, updates).GetText("t").ToString())
}

// Pending counts the logged updates no Drain has handed over.
func TestUnit_UpdateLog_Pending(t *testing.T) {
	doc := New(WithClientID(1))
	m := doc.GetMap("m")
	log := doc.OpenUpdateLog()
	defer log.Close()
	assert.Zero(t, log.Pending())
	for i := range 3 {
		doc.Transact(func(txn *Transaction) { m.Set(txn, "k", i) })
	}
	assert.Equal(t, 3, log.Pending())
	_, updates := log.Drain()
	assert.Len(t, updates, 3)
	assert.Zero(t, log.Pending())
	doc.Transact(func(txn *Transaction) { m.Set(txn, "k", 3) })
	assert.Equal(t, 1, log.Pending())
}

// Close, called twice and concurrently with commits and Drain, stops logging:
// nothing is logged after it, the log is dropped, and the document forgets
// the log at its next commit.
func TestUnit_UpdateLog_CloseIsSafeConcurrently(t *testing.T) {
	doc := New(WithClientID(1))
	m := doc.GetMap("m")
	log := doc.OpenUpdateLog()

	var writers, others sync.WaitGroup
	stop := make(chan struct{})
	for w := range 4 {
		writers.Add(1)
		go func() {
			defer writers.Done()
			for i := range 200 {
				doc.Transact(func(txn *Transaction) { m.Set(txn, fmt.Sprint(w), i) })
			}
		}()
	}
	others.Add(1)
	go func() {
		defer others.Done()
		for {
			select {
			case <-stop:
				return
			default:
				log.Drain()
				_ = log.Pending()
			}
		}
	}()
	for range 2 {
		others.Add(1)
		go func() {
			defer others.Done()
			log.Close()
		}()
	}
	writers.Wait()
	close(stop)
	others.Wait()

	doc.Transact(func(txn *Transaction) { m.Set(txn, "after", true) })
	base, updates := log.Drain()
	assert.Nil(t, base)
	assert.Empty(t, updates)
	assert.Zero(t, log.Pending())
	assert.Empty(t, log.Reset(), "Reset on a closed log hands over nothing")
	log.Close()

	doc.mu.Lock()
	defer doc.mu.Unlock()
	assert.Empty(t, doc.updateLogs, "the document forgot the closed log")
}

// Reset hands over exactly the updates no Drain has taken, drops a recorded
// base no Drain has taken, and makes the next commit record a fresh base.
func TestUnit_UpdateLog_Reset(t *testing.T) {
	doc := New(WithClientID(1))
	txt := doc.GetText("t")
	log := doc.OpenUpdateLog()
	defer log.Close()

	doc.Transact(func(txn *Transaction) { txt.Insert(txn, 0, "a", nil) }, "a")
	assert.Equal(t, []any{"a"}, loggedOrigins(log.Reset()))
	base, updates := log.Drain()
	assert.Nil(t, base, "Reset dropped the base no Drain had taken")
	assert.Empty(t, updates)

	doc.Transact(func(txn *Transaction) { txt.Insert(txn, 1, "b", nil) }, "b")
	doc.Transact(func(txn *Transaction) { txt.Insert(txn, 2, "c", nil) }, "c")
	base, updates = log.Drain()
	require.NotNil(t, base)
	assert.Equal(t, "a", replay(t, base, nil).GetText("t").ToString(), "the fresh base is the state before the next commit")
	assert.Equal(t, []any{"b", "c"}, loggedOrigins(updates))

	doc.Transact(func(txn *Transaction) { txt.Insert(txn, 3, "d", nil) }, "d")
	doc.Transact(func(txn *Transaction) { txt.Insert(txn, 4, "e", nil) }, "e")
	assert.Equal(t, []any{"d", "e"}, loggedOrigins(log.Reset()), "Reset hands over the undrained updates")
	assert.Zero(t, log.Pending())

	doc.Transact(func(txn *Transaction) { txt.Insert(txn, 5, "f", nil) }, "f")
	base, updates = log.Drain()
	require.NotNil(t, base)
	assert.Equal(t, "abcde", replay(t, base, nil).GetText("t").ToString())
	assert.Equal(t, "abcdef", replay(t, base, updates).GetText("t").ToString())
}

// Commits that run concurrently with Reset each land once: in an update
// Reset or Drain hands over before the Reset, or after the fresh base it
// asks for. A base recorded by a commit that raced other writers, and the
// updates after it, reproduce the document.
func TestUnit_UpdateLog_ResetConcurrentWithCommits(t *testing.T) {
	doc := New(WithClientID(1))
	m := doc.GetMap("m")
	log := doc.OpenUpdateLog()
	defer log.Close()

	const writers, perWriter = 4, 200
	write := func(round string) <-chan struct{} {
		var wg sync.WaitGroup
		for w := range writers {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for i := range perWriter {
					key := fmt.Sprintf("%s/%d/%d", round, w, i)
					doc.Transact(func(txn *Transaction) { m.Set(txn, key, round) }, key)
				}
			}()
		}
		done := make(chan struct{})
		go func() {
			wg.Wait()
			close(done)
		}()
		return done
	}

	seen := make(map[any]int)
	count := func(updates []LoggedUpdate) {
		for _, u := range updates {
			seen[u.Origin]++
		}
	}
	// segment is what follows the latest Reset: its base and its updates.
	var segmentBase []byte
	var segment []LoggedUpdate
	drain := func() {
		base, updates := log.Drain()
		if base != nil {
			require.Nil(t, segmentBase, "one base per Reset")
			require.Empty(t, segment, "the base comes before the updates after it")
			segmentBase = base
		}
		if len(updates) > 0 {
			require.NotNil(t, segmentBase, "updates after a Reset follow its base")
		}
		count(updates)
		segment = append(segment, updates...)
	}
	reset := func() {
		count(log.Reset())
		segmentBase, segment = nil, nil
	}

	// Round 1: Reset and Drain race the writers.
	done := write("reset")
	for running := true; running; {
		select {
		case <-done:
			running = false
		default:
		}
		drain()
		reset()
		drain()
	}
	drain()
	reset()

	// Round 2: the writers race each other for the fresh base, and Drain
	// races them; no Reset, so this segment runs to the end.
	done = write("replay")
	for running := true; running; {
		select {
		case <-done:
			running = false
		default:
		}
		drain()
	}
	drain()

	require.Len(t, seen, 2*writers*perWriter, "every commit is logged")
	for origin, n := range seen {
		require.Equal(t, 1, n, "commit %v is logged once", origin)
	}
	require.NotNil(t, segmentBase)
	assert.Equal(t, m.Entries(), replay(t, segmentBase, segment).GetMap("m").Entries())
	assert.Equal(t, doc.StateVector(), replay(t, segmentBase, segment).StateVector())
}

// With no log open and no OnUpdate observer, a commit encodes no update, as
// before UpdateLog existed; a closed log stops costing an encoding once the
// document forgets it at its next commit.
func TestUnit_UpdateLog_NoLogNoEncoding(t *testing.T) {
	doc := New(WithClientID(1))
	txt := doc.GetText("t")
	doc.Transact(func(txn *Transaction) { txt.Insert(txn, 0, "some state", nil) })
	empty := func() { doc.Transact(func(*Transaction) {}) }

	assert.False(t, doc.wantsUpdateBytes())
	without := testing.AllocsPerRun(100, empty)

	log := doc.OpenUpdateLog()
	assert.True(t, doc.wantsUpdateBytes())
	with := testing.AllocsPerRun(100, func() {
		empty()
		log.Drain()
	})
	assert.Greater(t, with, without, "an open log encodes each commit")

	log.Close()
	empty()
	assert.False(t, doc.wantsUpdateBytes(), "the commit after Close forgot the log")
	assert.Empty(t, doc.updateLogs)

	unsubscribe := doc.OnUpdate(func([]byte, any) {})
	assert.True(t, doc.wantsUpdateBytes())
	unsubscribe()
	assert.False(t, doc.wantsUpdateBytes())
}
