package redis

import (
	"bytes"
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	goredis "github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"

	"github.com/reearth/ygo/awareness"
	"github.com/reearth/ygo/cluster"
	"github.com/reearth/ygo/crdt"
	"github.com/reearth/ygo/internal/relaylane"
)

// Assignment must be stable: a room that moved readers between cycles would
// have two readers holding cursors for it.
func TestUnit_StreamReader_AssignmentIsStable(t *testing.T) {
	mr := newMiniRedis(t)
	r, err := New(newClient(t, mr), Config{Transport: Streams, Readers: 4})
	require.NoError(t, err)
	t.Cleanup(func() { _ = r.Close() })

	first := r.readerFor("room-alpha")
	for i := 0; i < 100; i++ {
		require.Equal(t, first, r.readerFor("room-alpha"))
	}
	require.GreaterOrEqual(t, first, 0)
	require.Less(t, first, 4)
}

// With many rooms every reader should get work; a hash that piles everything
// onto one reader would defeat the sharding.
func TestUnit_StreamReader_AssignmentSpreadsAcrossReaders(t *testing.T) {
	mr := newMiniRedis(t)
	r, err := New(newClient(t, mr), Config{Transport: Streams, Readers: 4})
	require.NoError(t, err)
	t.Cleanup(func() { _ = r.Close() })

	used := map[int]int{}
	for i := 0; i < 400; i++ {
		used[r.readerFor(fmt.Sprintf("room-%d", i))]++
	}
	require.Len(t, used, 4, "every reader should own some rooms")
	for idx, n := range used {
		require.Greater(t, n, 20, "reader %d got only %d of 400 rooms", idx, n)
	}
}

// XREAD command size must be bounded independently of Readers: 10k rooms over
// 4 readers would otherwise build a ~5000-argument command.
func TestUnit_StreamReader_KeyBatching(t *testing.T) {
	keys := make([]string, 0, 1300)
	for i := 0; i < 1300; i++ {
		keys = append(keys, fmt.Sprintf("k%d", i))
	}

	batches := keyBatches(keys, maxKeysPerRead)
	require.Len(t, batches, 3)
	require.Len(t, batches[0], 512)
	require.Len(t, batches[1], 512)
	require.Len(t, batches[2], 276)

	seen := 0
	for _, b := range batches {
		seen += len(b)
	}
	require.Equal(t, len(keys), seen, "batching must not drop keys")
}

func TestUnit_StreamReader_KeyBatchingEmpty(t *testing.T) {
	require.Empty(t, keyBatches(nil, maxKeysPerRead))
}

// The Relay contract requires tolerating a successor RoomActivated before the
// predecessor's RoomDeactivated, in either order. Refcounting rides that out;
// treating deactivation as an unconditional removal would drop a live room.
//
// r.Start is required here even though the brief's version of this test
// omitted it: RoomActivated/RoomDeactivated both return immediately when
// !r.started.Load(), before ever reaching the pub/sub work this task's
// stream block is appended after — so without Start, streamRooms would
// never be touched and this test would fail regardless of whether the new
// refcounting code is correct (the same class of gap already caught and
// documented in streams_test.go's Both/PubSub Publish tests).
//
// The brief's version of this test asserted only presence/absence via
// roomsForReader, which does not actually distinguish a true integer refcount
// from a naive boolean flag that merely mirrors whether activeRooms[room] is
// zero: since every RoomActivated/RoomDeactivated call drives BOTH activeRooms
// and streamRooms off the exact same events, a flag that flips on the 0->1
// and >0->0 crossings would show identically "present" after the successor
// activation and "absent" after the final deactivation, with no assertion
// ever distinguishing it from streamRooms holding the real count (2, then 1).
// Added a direct read of r.streamRooms (this is package redis, so the
// unexported field is reachable) to require the count itself is 2 after both
// activations, and 1 after the predecessor's deactivation — the "reference
// count" the interface's own doc comment calls for, not just a derived flag.
func TestUnit_StreamReader_ActivationRefcounts(t *testing.T) {
	mr := newMiniRedis(t)
	r, err := New(newClient(t, mr), Config{Transport: Streams})
	require.NoError(t, err)
	t.Cleanup(func() { _ = r.Close() })
	require.NoError(t, r.Start(context.Background(), &countingSink{}))

	r.RoomActivated("room1") // predecessor
	r.RoomActivated("room1") // successor, before the predecessor tears down

	r.streamMu.Lock()
	require.Equal(t, 2, r.streamRooms["room1"], "two activations must produce a count of 2, not a boolean flag")
	r.streamMu.Unlock()

	r.RoomDeactivated("room1")

	r.streamMu.Lock()
	require.Equal(t, 1, r.streamRooms["room1"], "one of two activations released must leave a count of 1")
	r.streamMu.Unlock()

	idx := r.readerFor("room1")
	require.Contains(t, r.roomsForReader(idx), "room1",
		"a successor activation must keep the room assigned")

	r.RoomDeactivated("room1")
	require.NotContains(t, r.roomsForReader(idx), "room1")
}

// --- Reader loop -----------------------------------------------------------
//
// Test-double naming: this file uses recordingSink rather than the fakeSink
// the brief named. redis_test.go (package redis_test) already has a DIFFERENT
// fakeSink, and internal_test.go (package redis) has countingSink; a third
// type sharing the name fakeSink in the same directory compiles but is a
// readability trap. countingSink could not simply be extended either — it
// records only a count, and half of these tests need to assert on the actual
// payload bytes, and widening a helper five existing tests depend on is a
// larger change than adding a purpose-built one.

// recordingSink records what a relay injects, so a test can assert on the
// payload rather than only on a count.
//
// It has no room registry, unlike the brief's version: workerForInbound
// routes on r.workers (populated by RoomActivated), and cluster/redis never
// calls Sink.Rooms() at all — verified by grep. A rooms map here would have
// implied that Sink residency gates inbound delivery, which is false, and the
// next reader of this file would have believed it.
type recordingSink struct {
	mu       sync.Mutex
	injected [][]byte

	// gate, when non-nil, parks every Inject until it is closed, standing in
	// for a room whose consumer is wedged. Nil (the zero value) is the
	// ordinary non-blocking sink every other test here uses, so adding this
	// changed no existing behaviour and did not need a fourth sink type.
	gate chan struct{}
	// entered is closed the first time an Inject parks on gate, so a test can
	// wait until delivery is genuinely stuck rather than guessing with a
	// sleep.
	entered     chan struct{}
	enteredOnce sync.Once
}

func (s *recordingSink) Inject(ctx context.Context, in cluster.Inbound) error {
	if s.gate != nil {
		s.enteredOnce.Do(func() { close(s.entered) })
		select {
		case <-s.gate:
		case <-ctx.Done():
			// Honour cancellation so Close's wg.Wait cannot hang on this
			// worker if a test forgets to open the gate.
			return ctx.Err()
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.injected = append(s.injected, append([]byte(nil), in.Data...))
	return nil
}

func (s *recordingSink) Rooms() []string                                  { return nil }
func (s *recordingSink) GetAwareness(string) (*awareness.Awareness, bool) { return nil, false }
func (s *recordingSink) GetDoc(string) *crdt.Doc                          { return nil }

func (s *recordingSink) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.injected)
}

func (s *recordingSink) payloadSeen(want string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, got := range s.injected {
		if bytes.Contains(got, []byte(want)) {
			return true
		}
	}
	return false
}

// payloadCount is how many INJECTED PAYLOADS carried want, which is what
// tells a delivery from a re-delivery. Not the number of occurrences within a
// payload: a catch-up merge folds several entries into one blob, and that
// blob is still one delivery.
func (s *recordingSink) payloadCount(want string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, got := range s.injected {
		if bytes.Contains(got, []byte(want)) {
			n++
		}
	}
	return n
}

// readerTestNodeIDs are 16-byte node identities, matching nodeIDLen so these
// look like the real thing rather than relying on a short id being accepted.
const (
	nodeA = "aaaaaaaaaaaaaaaa"
	nodeB = "bbbbbbbbbbbbbbbb"
)

// readerConfig is the shared Streams config for the reader tests.
//
// Readers: 1 makes every room land on reader 0, so a test never has to guess
// which goroutine owns its room. ReadBlock is set explicitly, just below the
// 250ms default and ceiling, so these tests state the interval their own
// sleeps are sized against rather than inheriting it.
func readerConfig(node string) Config {
	return Config{
		Transport: Streams,
		NodeID:    []byte(node),
		Readers:   1,
		ReadBlock: readerTestBlock,
	}
}

// readerTestBlock is readerConfig's ReadBlock, named so a test that has to
// outwait a read cycle says so instead of hard-coding a number.
const readerTestBlock = 200 * time.Millisecond

// v1Update produces a real V1 update blob containing text verbatim, so the
// catch-up path exercises crdt.MergeUpdatesV1 for real instead of falling
// into the merge-failure fallback that a non-update payload would take.
func v1Update(t *testing.T, text string) []byte {
	t.Helper()
	d := crdt.New()
	txt := d.GetText("t") // outside Transact: GetText inside deadlocks
	var out []byte
	un := d.OnUpdate(func(u []byte, _ any) { out = append([]byte(nil), u...) })
	defer un()
	d.Transact(func(tr *crdt.Transaction) { txt.Insert(tr, 0, text, nil) })
	require.NotEmpty(t, out)
	require.Contains(t, string(out), text, "the test asserts on this text appearing in the blob")
	return out
}

// THE HEADLINE TEST. A reader stops, another node publishes, the reader comes
// back — and the update is still delivered. The pub/sub tier structurally
// cannot pass this, which is the entire reason this tier exists.
//
// Verified non-vacuous: with the reader loop absent (Start not launching
// runStreamReader) this fails at the FIRST require.Eventually, because
// nothing is ever injected. See task-6-report.md for the recorded output.
func TestIntegration_StreamReader_ResumesAfterStopAndDeliversMissedUpdates(t *testing.T) {
	mr := newMiniRedis(t)

	// Node A reads. Node B publishes; a distinct NodeID is what stops A's
	// self-delivery filter from discarding B's entries.
	sink := &recordingSink{}
	a, err := New(newClient(t, mr), readerConfig(nodeA))
	require.NoError(t, err)
	t.Cleanup(func() { _ = a.Close() })
	b, err := New(newClient(t, mr), readerConfig(nodeB))
	require.NoError(t, err)
	t.Cleanup(func() { _ = b.Close() })

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	require.NoError(t, a.Start(ctx, sink))
	require.NoError(t, b.Start(ctx, &countingSink{}))
	a.RoomActivated("room1")

	// A is up: it sees the first update.
	first, whileDown := v1Update(t, "first-edit"), v1Update(t, "while-down-edit")
	require.NoError(t, b.Publish(ctx, cluster.Outbound{
		Room: "room1", Kind: cluster.KindSync, Data: first,
	}))
	require.Eventually(t, func() bool { return sink.count() == 1 },
		5*time.Second, 10*time.Millisecond, "reader should deliver while running")

	// A goes away. Everything B publishes now would be LOST under pub/sub.
	require.NoError(t, a.Close())
	require.NoError(t, b.Publish(context.Background(), cluster.Outbound{
		Room: "room1", Kind: cluster.KindSync, Data: whileDown,
	}))

	// A comes back with a fresh relay against the same Redis.
	sink2 := &recordingSink{}
	a2, err := New(newClient(t, mr), readerConfig(nodeA))
	require.NoError(t, err)
	t.Cleanup(func() { _ = a2.Close() })
	require.NoError(t, a2.Start(ctx, sink2))
	a2.RoomActivated("room1")

	require.Eventually(t, func() bool { return sink2.payloadSeen("while-down-edit") },
		5*time.Second, 10*time.Millisecond,
		"the update published while the reader was down must still arrive: this is the guarantee")
}

// Replaying already-applied entries is harmless (V1 updates are idempotent),
// which is what lets the reader start at the oldest retained entry and so
// removes the snapshot-load race entirely. But the catch-up batch must arrive
// as ONE merged update, or replay re-broadcasts every entry to local peers.
//
// Note which assertion carries the weight. sink.count()==1 is only WEAKLY
// coupled to reader-side merging: relaylane.Lane.TakeSync merges its own
// pending backlog too, so five separate pushes still usually surface as one
// Inject, purely depending on whether the room worker drained between them.
// The Replayed assertion is the precise one — it can only be 4 if the reader
// merged the batch itself. Verified by mutation: pushing the five payloads
// individually leaves count()==1 and fails on Replayed.
func TestIntegration_StreamReader_CatchUpMergedIntoSingleInject(t *testing.T) {
	mr := newMiniRedis(t)
	b, err := New(newClient(t, mr), readerConfig(nodeB))
	require.NoError(t, err)
	t.Cleanup(func() { _ = b.Close() })
	require.NoError(t, b.Start(context.Background(), &countingSink{}))

	// Five real V1 updates exist before any reader starts.
	for i := 0; i < 5; i++ {
		require.NoError(t, b.Publish(context.Background(), cluster.Outbound{
			Room: "room1", Kind: cluster.KindSync, Data: v1Update(t, fmt.Sprintf("edit-c%d", i)),
		}))
	}

	sink := &recordingSink{}
	a, err := New(newClient(t, mr), readerConfig(nodeA))
	require.NoError(t, err)
	t.Cleanup(func() { _ = a.Close() })

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	require.NoError(t, a.Start(ctx, sink))
	a.RoomActivated("room1")

	require.Eventually(t, func() bool { return sink.count() >= 1 },
		5*time.Second, 10*time.Millisecond)
	time.Sleep(500 * time.Millisecond) // let any extra injects land

	require.Equal(t, 1, sink.count(),
		"a five-entry catch-up must reach the sink as one merged update, not five")
	require.Equal(t, uint64(4), a.StreamStats().Replayed,
		"Replayed must record the merge saving (5 entries -> 1 inject)")
	// All five edits must survive the merge: one inject is only correct if it
	// carries every entry.
	for i := 0; i < 5; i++ {
		require.True(t, sink.payloadSeen(fmt.Sprintf("edit-c%d", i)),
			"merging must not lose an entry")
	}
}

// Awareness is read from the tail and never replayed: replaying it would
// resurrect presence for clients that are long gone.
//
// The brief's version of this test asserted ONLY that the stale awareness
// payload is absent, after a fixed sleep. That would have passed even if the
// reader never ran at all, or never read awareness streams at all — which is
// the same vacuous-test class already caught in tasks 3 and 4. Two positive
// witnesses are added: a sync entry published before start MUST be replayed
// (proving the reader reached this room), and an awareness entry published
// after start MUST arrive (proving awareness delivery works at all, so the
// absence below is about the replay policy and not about a dead code path).
func TestIntegration_StreamReader_AwarenessIsNotReplayed(t *testing.T) {
	mr := newMiniRedis(t)
	b, err := New(newClient(t, mr), readerConfig(nodeB))
	require.NoError(t, err)
	t.Cleanup(func() { _ = b.Close() })
	require.NoError(t, b.Start(context.Background(), &countingSink{}))

	require.NoError(t, b.Publish(context.Background(), cluster.Outbound{
		Room: "room1", Kind: cluster.KindAwareness, Data: []byte("stale-presence"),
	}))
	require.NoError(t, b.Publish(context.Background(), cluster.Outbound{
		Room: "room1", Kind: cluster.KindSync, Data: v1Update(t, "pre-start-edit"),
	}))

	sink := &recordingSink{}
	a, err := New(newClient(t, mr), readerConfig(nodeA))
	require.NoError(t, err)
	t.Cleanup(func() { _ = a.Close() })

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	require.NoError(t, a.Start(ctx, sink))
	a.RoomActivated("room1")

	// Witness 1: the sync stream IS replayed, so the reader did reach room1.
	require.Eventually(t, func() bool { return sink.payloadSeen("pre-start-edit") },
		5*time.Second, 10*time.Millisecond, "sync published before start must be replayed")

	// Witness 2: awareness published while the reader is up DOES arrive, so
	// the awareness half of the read is live rather than silently broken.
	require.NoError(t, b.Publish(ctx, cluster.Outbound{
		Room: "room1", Kind: cluster.KindAwareness, Data: []byte("live-presence"),
	}))
	require.Eventually(t, func() bool { return sink.payloadSeen("live-presence") },
		5*time.Second, 10*time.Millisecond, "awareness published after start must arrive")

	require.False(t, sink.payloadSeen("stale-presence"),
		"awareness published before the reader started must NOT be replayed")
}

// A node must not re-inject its own writes.
//
// Strengthened over the brief, which asserted only the absence of the node's
// own payload after a fixed sleep — vacuous if the reader had not yet read
// anything. Node B's payload is the witness: it proves the reader consumed
// the same stream past A's own entry.
func TestIntegration_StreamReader_SelfFilter(t *testing.T) {
	mr := newMiniRedis(t)
	sink := &recordingSink{}
	a, err := New(newClient(t, mr), readerConfig(nodeA))
	require.NoError(t, err)
	t.Cleanup(func() { _ = a.Close() })
	b, err := New(newClient(t, mr), readerConfig(nodeB))
	require.NoError(t, err)
	t.Cleanup(func() { _ = b.Close() })

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	require.NoError(t, a.Start(ctx, sink))
	require.NoError(t, b.Start(ctx, &countingSink{}))
	a.RoomActivated("room1")

	require.NoError(t, a.Publish(ctx, cluster.Outbound{
		Room: "room1", Kind: cluster.KindSync, Data: []byte("mine"),
	}))
	require.NoError(t, b.Publish(ctx, cluster.Outbound{
		Room: "room1", Kind: cluster.KindSync, Data: []byte("theirs"),
	}))

	require.Eventually(t, func() bool { return sink.payloadSeen("theirs") },
		5*time.Second, 10*time.Millisecond, "another node's entry must be delivered")
	require.False(t, sink.payloadSeen("mine"), "a node must not re-inject its own entry")
}

// Transport's zero value is PubSub, and a PubSub relay must not grow a reader.
//
// The second half of this test is what actually pins the usesStreams gate in
// Start. The first half — activate a room on a PubSub relay and see nothing
// arrive — passes even with that gate removed, because RoomActivated has its
// own usesStreams gate on streamRooms (task 5), so a launched reader would
// simply idle with no assignment. Verified by mutation: replacing the Start
// gate with `if true` left the first half green. So the second half forces
// streamRooms to hold the room, which is the only other thing standing
// between a reader and this stream; then the gate in Start is the sole
// remaining defence, and removing it makes this test fail.
func TestIntegration_StreamReader_PubSubModeReadsNoStreams(t *testing.T) {
	mr := newMiniRedis(t)

	// A Streams-only publisher: it XADDs and never PUBLISHes.
	b, err := New(newClient(t, mr), readerConfig(nodeB))
	require.NoError(t, err)
	t.Cleanup(func() { _ = b.Close() })
	require.NoError(t, b.Start(context.Background(), &countingSink{}))
	require.NoError(t, b.Publish(context.Background(), cluster.Outbound{
		Room: "room1", Kind: cluster.KindSync, Data: []byte("streams-only"),
	}))

	sink := &recordingSink{}
	a, err := New(newClient(t, mr), Config{NodeID: []byte(nodeA)}) // zero Transport == PubSub
	require.NoError(t, err)
	t.Cleanup(func() { _ = a.Close() })

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	require.NoError(t, a.Start(ctx, sink))
	a.RoomActivated("room1")

	time.Sleep(700 * time.Millisecond)
	require.False(t, sink.payloadSeen("streams-only"),
		"a PubSub-mode relay must not read stream keys")

	// Now remove the OTHER defence: hand the room to the reader assignment
	// map directly, as Streams mode's RoomActivated would have. RoomActivated
	// above already created room1's delivery worker, so a reader that existed
	// would find a live lane and deliver. Nothing may still arrive.
	a.streamMu.Lock()
	a.streamRooms["room1"] = 1
	a.streamMu.Unlock()

	time.Sleep(700 * time.Millisecond)
	require.False(t, sink.payloadSeen("streams-only"),
		"Start must launch no reader at all in PubSub mode, even for an assigned room")
	require.Equal(t, StreamStats{}, a.StreamStats(),
		"a PubSub-mode relay's stream counters must all stay zero")
}

func TestUnit_StreamReader_CursorDefaultsAndRoundTrip(t *testing.T) {
	mr := newMiniRedis(t)
	r, err := New(newClient(t, mr), Config{Transport: Streams, Readers: 1})
	require.NoError(t, err)
	t.Cleanup(func() { _ = r.Close() })

	require.Equal(t, oldestID, r.cursorFor("never-read", oldestID),
		"an unknown sync key must fall back to the oldest retained entry")
	require.Equal(t, tailID, r.cursorFor("never-read", tailID),
		"an unknown awareness key must fall back to the tail")

	r.setCursor("k", "5-1")
	require.Equal(t, "5-1", r.cursorFor("k", oldestID))
}

// Cursor eviction must not evict a room this relay is still reading.
//
// This deviates from the brief, which evicted an ARBITRARY map entry on
// overflow. Doing that on a node with more than cursorLimit/2 active rooms
// drops a live room's cursor roughly half the time, and losing a live
// cursor replays that room's whole retention window — which is precisely
// the condition StreamStats.Replayed tells operators to alert on. Evicting
// only cursors whose room is no longer in streamRooms bounds the map by real
// load instead.
func TestUnit_StreamReader_CursorEvictionKeepsActiveRooms(t *testing.T) {
	mr := newMiniRedis(t)
	// Deliberately NOT Started: no reader goroutine, so nothing races the
	// cursor map while this test pokes it.
	r, err := New(newClient(t, mr), Config{Transport: Streams, Readers: 1})
	require.NoError(t, err)
	t.Cleanup(func() { _ = r.Close() })

	r.streamMu.Lock()
	r.streamRooms["live"] = 1
	r.streamMu.Unlock()

	liveKey := r.scfg.syncKey("live")
	r.setCursor(liveKey, "7-0")
	for i := 0; i < cursorLimit; i++ {
		r.setCursor(fmt.Sprintf("ygo:gone:%d", i), "1-0")
	}
	r.setCursor("ygo:gone:trigger", "2-0")

	require.Equal(t, "7-0", r.cursorFor(liveKey, oldestID),
		"an active room's cursor must survive eviction, or the room replays its whole window")

	r.streamMu.Lock()
	held := len(r.cursors)
	r.streamMu.Unlock()
	require.Less(t, held, cursorLimit, "eviction must actually bound the map")
}

// Both tests below run at the DEFAULT ReadBlock, because that is what every
// operator gets and, since it is also the ceiling, the worst case any
// operator can configure.
//
// A blocked XREAD cannot be interrupted — go-redis arms the socket read
// deadline from ctx.Deadline() only, so cancelling the context mid-read has
// no effect — and Close joins r.wg, so the block interval IS how long Close
// takes. At the 5s ReadBlock this package used to default to, Close was
// measured at 4.80s: a five-second stall on every Server.Shutdown. See
// maxReadBlock, which now rejects any value that could bring that back.
func TestUnit_StreamReader_CloseDoesNotWaitOutReadBlock(t *testing.T) {
	mr := newMiniRedis(t)
	r, err := New(newClient(t, mr), Config{Transport: Streams, Readers: 1})
	require.NoError(t, err)
	require.Equal(t, defaultReadBlock, r.scfg.readBlock, "this test is about the DEFAULT ReadBlock")
	require.NoError(t, r.Start(context.Background(), &countingSink{}))
	r.RoomActivated("room1")

	time.Sleep(400 * time.Millisecond) // let the reader get well into an XREAD

	start := time.Now()
	require.NoError(t, r.Close())
	require.Less(t, time.Since(start), time.Second,
		"Close must not stall on a reader: a reader cannot be interrupted mid-XREAD, so the block itself has to be short (ReadBlock %s)", defaultReadBlock)
}

// A reader already parked in an XREAD for room1 cannot see room2 until that
// read returns, so the read interval IS the activation latency — a room
// joining and then seeing no remote edits for that long. At the 5s ReadBlock
// this package used to default to, the room2 update arrived about five
// seconds late; maxReadBlock is what bounds it.
func TestIntegration_StreamReader_ActivationDoesNotWaitOutReadBlock(t *testing.T) {
	mr := newMiniRedis(t)

	bcfg := readerConfig(nodeB)
	b, err := New(newClient(t, mr), bcfg)
	require.NoError(t, err)
	t.Cleanup(func() { _ = b.Close() })
	require.NoError(t, b.Start(context.Background(), &countingSink{}))

	sink := &recordingSink{}
	// Default ReadBlock on purpose — see the doc comment.
	a, err := New(newClient(t, mr), Config{Transport: Streams, NodeID: []byte(nodeA), Readers: 1})
	require.NoError(t, err)
	t.Cleanup(func() { _ = a.Close() })
	require.Equal(t, defaultReadBlock, a.scfg.readBlock)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	require.NoError(t, a.Start(ctx, sink))
	a.RoomActivated("room1")

	// Give the reader time to be sitting inside an XREAD whose key set is
	// room1 only.
	time.Sleep(500 * time.Millisecond)

	require.NoError(t, b.Publish(ctx, cluster.Outbound{
		Room: "room2", Kind: cluster.KindSync, Data: []byte("room2-edit"),
	}))
	a.RoomActivated("room2")

	require.Eventually(t, func() bool { return sink.payloadSeen("room2-edit") },
		3*time.Second, 10*time.Millisecond,
		"a room activated mid-read must not wait out ReadBlock (%s)", defaultReadBlock)
}

// A room this reader owns but has no worker for must not lose its backlog.
//
// RoomActivated adds the room to streamRooms BEFORE it creates the room's
// worker, so a reader can own a room with no lane to deliver to. A reader
// that treated that as a drop and advanced the cursor anyway would consume
// the room's ENTIRE retained backlog — a room reached in that window is read
// from the oldest retained entry — and discard it before the worker that was
// about to exist could receive any of it.
//
// The witness room is what makes this airtight: Readers is 1, so both rooms'
// keys are in the SAME XREAD, and the witness edit arriving proves the
// orphan's entries were in that very response.
func TestIntegration_StreamReader_BacklogSurvivesAMissingWorker(t *testing.T) {
	mr := newMiniRedis(t)

	sink := &recordingSink{}
	a, err := New(newClient(t, mr), readerConfig(nodeA))
	require.NoError(t, err)
	t.Cleanup(func() { _ = a.Close() })
	b, err := New(newClient(t, mr), readerConfig(nodeB))
	require.NoError(t, err)
	t.Cleanup(func() { _ = b.Close() })

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	require.NoError(t, a.Start(ctx, sink))
	require.NoError(t, b.Start(ctx, &countingSink{}))

	// "witness" is activated properly, so it has a worker. "orphan" is only
	// added to the reader's assignment map — exactly the state RoomActivated
	// passes through on its way to creating the worker, held open here.
	a.RoomActivated("witness")
	a.streamMu.Lock()
	a.streamRooms["orphan"] = 1
	a.streamMu.Unlock()

	publish := func(room, text string) {
		require.NoError(t, b.Publish(ctx, cluster.Outbound{
			Room: room, Kind: cluster.KindSync, Data: v1Update(t, text),
		}))
	}
	orphanEdits := []string{"orphan-edit-0", "orphan-edit-1", "orphan-edit-2"}
	for _, text := range orphanEdits {
		publish("orphan", text)
	}
	publish("witness", "witness-edit")

	require.Eventually(t, func() bool { return sink.payloadSeen("witness-edit") },
		5*time.Second, 10*time.Millisecond,
		"the witness proves the reader completed a cycle covering both rooms")

	for _, text := range orphanEdits {
		require.False(t, sink.payloadSeen(text), "a room with no worker has nowhere to deliver")
	}
	require.Equal(t, oldestID, a.cursorFor(a.scfg.syncKey("orphan"), oldestID),
		"the cursor must NOT advance past entries nobody could receive")
	require.Equal(t, uint64(0), a.Stats().RouterDrops,
		"nothing was discarded, only deferred; RouterDrops counts discards and operators watch its rate")

	// The worker exists now. The backlog must still be there to read.
	a.RoomActivated("orphan")
	require.Eventually(t, func() bool {
		for _, text := range orphanEdits {
			if !sink.payloadSeen(text) {
				return false
			}
		}
		return true
	}, 5*time.Second, 10*time.Millisecond,
		"every entry read while the room had no worker must still be delivered once it has one")
}

// Awareness must not be replayed across a room's deactivate/reactivate.
//
// TestIntegration_StreamReader_AwarenessIsNotReplayed covers only a FRESH
// relay, where no cursor exists and the tail default applies on its own. The
// case that actually happens in production is a room evicted and reloaded
// inside one process — the websocket provider has done that continuously
// since idle-room residency landed (#183) — where a retained awareness cursor
// resumes mid-stream and replays presence for the room's previous occupants.
//
// The sync half is asserted in the same test on purpose: the two kinds need
// OPPOSITE retention (see cursorLimit), so a fix that dropped both would stop
// the replay and reintroduce the whole-window sync replay it exists to
// prevent.
//
// BOTH halves are asserted through DELIVERY, never by reading a cursor. An
// earlier version read Relay.cursors at an instant just after
// RoomDeactivated, and that assertion was flaky at ~25% for a reason worth
// keeping written down: RoomDeactivated's contract (cluster/relay.go)
// explicitly does not stop a read whose id vector predates it, so "the
// awareness cursor is absent right now" was never an invariant the relay
// offered — the state was reachable, just not at that instant. What an
// operator actually needs IS invariant, and is what this asserts: after a
// reactivation, no presence published before it is delivered.
//
// The closing assertion is an ordering argument rather than a timing one.
// presence-while-gone sits EARLIER in the same awareness stream than
// presence-after, so a residency resuming from a surviving cursor would read
// it in the same XREAD as presence-after, or in an earlier one. Waiting for
// presence-after therefore proves presence-while-gone had every chance to
// arrive, without this test having to guess at a duration.
func TestIntegration_StreamReader_AwarenessNotReplayedAcrossReactivation(t *testing.T) {
	mr := newMiniRedis(t)

	sink := &recordingSink{}
	a, err := New(newClient(t, mr), readerConfig(nodeA))
	require.NoError(t, err)
	t.Cleanup(func() { _ = a.Close() })
	b, err := New(newClient(t, mr), readerConfig(nodeB))
	require.NoError(t, err)
	t.Cleanup(func() { _ = b.Close() })

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	require.NoError(t, a.Start(ctx, sink))
	require.NoError(t, b.Start(ctx, &countingSink{}))
	a.RoomActivated("room1")

	pubSync := func(text string) {
		require.NoError(t, b.Publish(ctx, cluster.Outbound{
			Room: "room1", Kind: cluster.KindSync, Data: v1Update(t, text),
		}))
	}
	pubPresence := func(text string) error {
		return b.Publish(ctx, cluster.Outbound{
			Room: "room1", Kind: cluster.KindAwareness, Data: []byte(text),
		})
	}

	// Establish BOTH cursors by getting both kinds delivered.
	pubSync("edit-before")
	require.Eventually(t, func() bool {
		if err := pubPresence("presence-before"); err != nil {
			return false
		}
		return sink.payloadSeen("edit-before") && sink.payloadSeen("presence-before")
	}, 5*time.Second, 20*time.Millisecond, "both kinds must flow before the room is deactivated")

	a.RoomDeactivated("room1")
	// Outwait the DEPARTING residency, not just the read. A read in flight at
	// the deactivation can still push onto a lane whose worker has not yet
	// performed its final drain, and presence delivered to the occupants who
	// are leaving is legitimate — it is not the replay this test hunts. This
	// wait is what makes everything published below unambiguously "published
	// while the room was gone", so the closing assertion cannot mistake a
	// teardown delivery for a resurrection.
	time.Sleep(3 * readerTestBlock)

	// Published to a room nothing is reading: this presence belongs to
	// clients that left with the room.
	require.NoError(t, pubPresence("presence-while-gone"))
	pubSync("edit-while-gone")

	a.RoomActivated("room1")

	// Witness: the surviving sync cursor still delivers what was published
	// while the room was gone, which also proves the reader is reading room1
	// again — so the awareness absence below is policy, not a dead path.
	require.Eventually(t, func() bool { return sink.payloadSeen("edit-while-gone") },
		5*time.Second, 10*time.Millisecond, "a kept sync cursor must still deliver")

	// Witness: live presence flows again after reactivation. Republished each
	// attempt because a tail-started stream has a one-cycle blind spot for an
	// entry appended between two reads — which is what a real client's
	// heartbeat rides out too.
	require.Eventually(t, func() bool {
		if err := pubPresence("presence-after"); err != nil {
			return false
		}
		return sink.payloadSeen("presence-after")
	}, 5*time.Second, 20*time.Millisecond, "awareness must flow after reactivation")

	require.False(t, sink.payloadSeen("presence-while-gone"),
		"presence published while the room was gone must not be replayed to its new occupants")

	// The opposite half of the retention rule, asserted the same way. A sync
	// cursor that did NOT survive the deactivation reads from the oldest
	// retained entry, so the reactivated room would be handed edit-before a
	// second time — inside the catch-up merge that also carries
	// edit-while-gone, hence a second payload containing it rather than a
	// second occurrence inside the first.
	require.Equal(t, 1, sink.payloadCount("edit-before"),
		"deactivation must KEEP the sync cursor, or room churn replays the whole retention window")
}

// The fence that makes the test above hold under the read it cannot stop.
//
// Deterministic where the integration test is opportunistic: it plays out the
// exact interleaving — a read's id vector is built on one residency, the room
// is deactivated and reactivated, and only THEN does the response apply. The
// entries in that response are presence published before the reactivation, so
// none of them may reach the successor residency, and neither may the cursor
// they would have advanced.
func TestUnit_StreamReader_AwarenessFromAPriorResidencyIsNotDelivered(t *testing.T) {
	mr := newMiniRedis(t)
	// Deliberately NOT Started: registering workers by hand keeps this to the
	// two lifecycle transitions under test, with no goroutine draining a lane
	// whose depth is the assertion.
	r, err := New(newClient(t, mr), Config{Transport: Streams, Readers: 1})
	require.NoError(t, err)
	t.Cleanup(func() { _ = r.Close() })

	register := func(room string) *roomWorker {
		w := &roomWorker{room: room, lane: relaylane.New(r.laneCap), done: make(chan struct{})}
		r.workersMu.Lock()
		r.workers[room] = w
		r.workersMu.Unlock()
		return w
	}

	// The residency the read was issued for, mid-stream on its own cursor.
	old := register("room1")
	_, from := r.awarenessCursor("room1")
	require.Equal(t, tailID, from, "a fresh residency starts at the tail")
	tgt := streamTarget{
		key: r.scfg.awKey("room1"), room: "room1", isAwareness: true, residency: old,
	}
	// A batch rather than a single entry: deliverAwareness pushes only the
	// last payload of a read, so a fence applied to that one payload and a
	// fence applied to the read are the same code — but a single-entry batch
	// could not tell the two apart if that ever stopped being true.
	msgs := []goredis.XMessage{
		streamEntry(t, "7-0", nodeB, 1, cluster.KindAwareness, []byte("presence-while-gone")),
		streamEntry(t, "8-0", nodeB, 2, cluster.KindAwareness, []byte("presence-while-gone-2")),
		streamEntry(t, "9-0", nodeB, 3, cluster.KindAwareness, []byte("presence-while-gone-3")),
	}

	// Deactivate, then reactivate: a NEW worker, which is what makes the
	// reset structural. stopWorker is the real production path.
	r.stopWorker("room1")
	fresh := register("room1")
	require.NotSame(t, old, fresh, "a reactivation must be a new residency")

	require.Equal(t, streamConsumed, r.handleStream(tgt, msgs),
		"the entries are dealt with, not deferred: the successor reads from the tail")
	require.Equal(t, 0, fresh.lane.Depth(),
		"presence published before the reactivation must not reach the new occupants")
	// And not onto the RETIRED residency's lane either, which is the half a
	// residency check performed separately from the push would miss. A
	// stopped worker performs one final drain into Sink.Inject (see
	// runRoomWorker), and Inject is addressed by ROOM — so a blob parked on
	// the old lane still reaches room1, whose occupants are now the new ones.
	require.Equal(t, 0, old.lane.Depth(),
		"a read from a prior residency must not push anywhere: the retired lane still drains into the room")
	residency, awFrom := r.awarenessCursor("room1")
	require.Same(t, fresh, residency)
	require.Equal(t, tailID, awFrom,
		"and the stale read must not advance the successor's cursor, or the next read resumes mid-presence-stream")
	require.Equal(t, uint64(0), r.Stats().RouterDrops,
		"a fenced presence blob is a policy discard, not a router drop operators alert on")
}

// Inbound latency must not scale with a reader's batch COUNT.
//
// Each XREAD blocks for the whole ReadBlock, so running the batches of one
// cycle sequentially with every one of them blocking puts data waiting in
// batch 10 behind nine full blocks. keyBatches's own doc cites 2500 rooms per
// reader, which is 10 batches.
func TestUnit_StreamReader_OnlyTheLastBatchOfACycleBlocks(t *testing.T) {
	mr := newMiniRedis(t)
	block := 40 * time.Millisecond
	r, err := New(newClient(t, mr), Config{Transport: Streams, Readers: 1, ReadBlock: block})
	require.NoError(t, err)
	t.Cleanup(func() { _ = r.Close() })

	require.Negative(t, int64(nonBlockingRead),
		"a non-blocking read needs a NEGATIVE Block: go-redis emits the BLOCK argument for any Block >= 0, and Redis reads BLOCK 0 as block FOREVER")

	// One batch is also the last batch, so nothing changes for the ordinary
	// single-batch reader: it still blocks.
	require.Equal(t, block, r.blockForBatch(0, 1, false))
	require.Equal(t, nonBlockingRead, r.blockForBatch(0, 1, true),
		"a cycle that already found entries has work to do and must not sit in a block")

	const n = 10 // 2500 rooms at maxKeysPerRead/streamsPerRoom per batch
	for i := 0; i < n-1; i++ {
		require.Equal(t, nonBlockingRead, r.blockForBatch(i, n, false),
			"batch %d of %d must not block: an entry in a later batch would wait out every earlier one", i, n)
	}
	require.Equal(t, block, r.blockForBatch(n-1, n, false),
		"the trailing block is what stops an idle reader spinning")
	require.Equal(t, nonBlockingRead, r.blockForBatch(n-1, n, true))
}

// End-to-end companion to the test above: a room in the SECOND batch is
// delivered normally.
//
// This is the guard against getting the non-blocking value wrong. "BLOCK 0"
// means block forever, so a batch-0 read issued with Block: 0 would never
// return and nothing here would ever arrive.
func TestIntegration_StreamReader_DeliversToARoomInALaterBatch(t *testing.T) {
	mr := newMiniRedis(t)

	sink := &recordingSink{}
	a, err := New(newClient(t, mr), readerConfig(nodeA))
	require.NoError(t, err)
	t.Cleanup(func() { _ = a.Close() })
	b, err := New(newClient(t, mr), readerConfig(nodeB))
	require.NoError(t, err)
	t.Cleanup(func() { _ = b.Close() })

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	require.NoError(t, a.Start(ctx, sink))
	require.NoError(t, b.Start(ctx, &countingSink{}))

	// More rooms than one batch holds, so there are two. roomsForReader sorts,
	// so the last name is in the last batch.
	perBatch := maxKeysPerRead / streamsPerRoom
	rooms := make([]string, 0, perBatch+4)
	for i := 0; i < perBatch+4; i++ {
		rooms = append(rooms, fmt.Sprintf("room-%04d", i))
	}
	for _, room := range rooms {
		a.RoomActivated(room)
	}
	require.Len(t, keyBatches(a.roomsForReader(0), perBatch), 2, "this test needs two batches")

	last := rooms[len(rooms)-1]
	require.NoError(t, b.Publish(ctx, cluster.Outbound{
		Room: last, Kind: cluster.KindSync, Data: v1Update(t, "later-batch-edit"),
	}))

	require.Eventually(t, func() bool { return sink.payloadSeen("later-batch-edit") },
		5*time.Second, 10*time.Millisecond,
		"a room in the last batch must be delivered to, not stuck behind an earlier batch's block")
}

// --- Gap detection ---------------------------------------------------------

// noteSeq must key its baseline by (nodeID, stream key). One node's two
// streams carry two INDEPENDENT series — nextSeq counts per stream — so
// folding them into one series by nodeID alone compares numbers that were
// never meant to be compared.
//
// The A,A,A,B,B,B ordering below is what makes this test discriminating: with
// per-node keying it reads as 1,2,3 then a DECREASE to 1, so Restarts climbs
// on a node that merely publishes to two rooms. Ordering the two series
// strictly alternately would hide the defect, since 1,1,2,2,3,3 contains
// neither a jump nor a decrease.
func TestUnit_StreamReader_NoteSeqIsPerNodePerStream(t *testing.T) {
	mr := newMiniRedis(t)
	// Deliberately NOT Started: no reader goroutine, so nothing else touches
	// the counters this test asserts on.
	r, err := New(newClient(t, mr), Config{Transport: Streams, Readers: 1})
	require.NoError(t, err)
	t.Cleanup(func() { _ = r.Close() })

	keyA, keyB := r.scfg.syncKey("room1"), r.scfg.syncKey("room2")
	src := []byte(nodeB)

	for _, seq := range []uint64{1, 2, 3} {
		r.noteSeq(keyA, src, seq)
	}
	for _, seq := range []uint64{1, 2, 3} {
		r.noteSeq(keyB, src, seq)
	}
	require.Equal(t, uint64(0), r.StreamStats().Gaps,
		"two contiguous per-stream series from one node are not a gap")
	require.Equal(t, uint64(0), r.StreamStats().Restarts,
		"the second stream's series starting over is not a restart")

	// A real jump within ONE stream is still a gap.
	r.noteSeq(keyA, src, 9)
	require.Equal(t, uint64(1), r.StreamStats().Gaps, "a jump on one stream must be reported")

	// And the other stream's baseline was untouched by it.
	r.noteSeq(keyB, src, 4)
	require.Equal(t, uint64(1), r.StreamStats().Gaps, "streams must not interfere")

	// A different node on the same stream is its own series, so its first
	// entry is a baseline rather than a decrease.
	r.noteSeq(keyA, []byte(nodeA), 1)
	require.Equal(t, uint64(0), r.StreamStats().Restarts)
	require.Equal(t, uint64(1), r.StreamStats().Gaps)

	// A genuine decrease on one series is still a restart.
	r.noteSeq(keyA, src, 2)
	require.Equal(t, uint64(1), r.StreamStats().Restarts)
}

// A source that restarts its in-memory counter must not just avoid being
// misreported as data loss — detection must keep working against the NEW
// baseline afterwards. This is the half of restart-handling that a
// restarts-vs-gaps classification alone does not prove: a baseline that
// never resets down would either report every post-restart entry as a fresh
// gap, or (if the fix instead treated already-seen-looking lower numbers as
// duplicates) silently stop advancing for that source at all — a stall, not
// merely a miscount.
//
// Verified by mutation: changing noteSeq to only ever raise its baseline
// (`if seq > prev { r.lastSeq[src] = seq }`, imitating a fix that tracks the
// high-water mark instead of the last-seen value) leaves this test as the
// only one in the package that fails; every other gap/restart test still
// passes because none of them re-probes classification after a decrease.
func TestUnit_StreamReader_GapDetectionResumesAfterARestart(t *testing.T) {
	mr := newMiniRedis(t)
	r, err := New(newClient(t, mr), Config{Transport: Streams, Readers: 1})
	require.NoError(t, err)
	t.Cleanup(func() { _ = r.Close() })

	key := r.scfg.syncKey("room1")
	src := []byte(nodeB)

	for _, seq := range []uint64{1, 2, 3} {
		r.noteSeq(key, src, seq)
	}

	r.noteSeq(key, src, 1) // the same node, restarted
	require.Equal(t, uint64(1), r.StreamStats().Restarts)
	require.Zero(t, r.StreamStats().Gaps, "a restart must not itself be reported as data loss")

	// The baseline must now be the restarted value, not the pre-restart one:
	// a contiguous follow-on must stay silent...
	r.noteSeq(key, src, 2)
	require.Zero(t, r.StreamStats().Gaps)
	require.Equal(t, uint64(1), r.StreamStats().Restarts, "no second restart on ordinary advancement")

	// ...and a real jump measured from that new baseline must still be
	// caught, proving detection did not stall or silently latch onto the
	// pre-restart series.
	r.noteSeq(key, src, 7)
	require.Equal(t, uint64(1), r.StreamStats().Gaps, "gap detection must work after a restart")
}

// Two Publish calls for the SAME room can overlap — Relay.Publish's contract
// requires tolerating exactly that across a room's eviction/reload handoff,
// and nextSeq deliberately releases streamMu before the XADD it numbered is
// issued (holding it across a Redis call would recouple every room on the node
// to one slow write). So a publisher's entries can be written to the stream in
// the order [2, 1, 3]: the reader sees a DECREASE and then, from the new
// baseline of 1, what looks like a jump to 3.
//
// That must not score a Gaps. Gaps is documented "ALERT ON PRESENCE… a single
// gap means data was lost", so a counter that ticks on routine room churn is
// worth less than no counter at all. The first comparison after a decrease
// re-baselines instead of accusing.
//
// The Restarts increment for the decrease is expected and asserted here: the
// suppression must be exactly one comparison deep, not a general amnesty.
//
// Confirmed by mutation: deleting noteSeq's `prev.afterDecrease` early return
// (so every jump counts, as before this fix) leaves this test as the ONLY
// failing test in the package — no existing gap/restart test covers a jump
// adjacent to a decrease.
func TestUnit_StreamReader_NoGapOnTheEntryAfterADecrease(t *testing.T) {
	mr := newMiniRedis(t)
	// Deliberately NOT Started: nothing else may touch these counters.
	r, err := New(newClient(t, mr), Config{Transport: Streams, Readers: 1})
	require.NoError(t, err)
	t.Cleanup(func() { _ = r.Close() })

	key := r.scfg.syncKey("room1")
	src := []byte(nodeB)

	// Entries land as [2, 1, 3] — two overlapping publishes, then the next.
	r.noteSeq(key, src, 2) // baseline
	r.noteSeq(key, src, 1) // the reordered partner: a decrease
	require.Equal(t, uint64(1), r.StreamStats().Restarts,
		"a decrease is still classified and counted; only the gap is suppressed")
	r.noteSeq(key, src, 3) // 3 > 1+1: a jump only because of the reordering
	require.Zero(t, r.StreamStats().Gaps,
		"a jump in the first comparison after a decrease must re-baseline, not accuse")

	// One comparison deep: the very next jump, no longer adjacent to a
	// decrease, is reported normally. Without this the suppression would be a
	// latch that disables gap detection for a source forever.
	r.noteSeq(key, src, 40)
	require.Equal(t, uint64(1), r.StreamStats().Gaps,
		"the suppression must clear after one comparison")

	// And the suppression is per series: a decrease on one source must not
	// license a silent gap on another source reading the same stream.
	other := []byte(nodeA)
	r.noteSeq(key, other, 1)
	r.noteSeq(key, src, 1) // decrease on src arms src's suppression only
	r.noteSeq(key, other, 9)
	require.Equal(t, uint64(2), r.StreamStats().Gaps,
		"a decrease on one source must not suppress another source's gap")
}

// The first sequence number ever seen from a source establishes a baseline,
// however large it is — it must never be compared against an implicit zero.
// A relay that only just started tracking a stream (or a source whose first
// live entry lands well past 1, e.g. after a retention window skipped ahead
// of a fresh reader) must not have that first observation mistaken for a
// jump.
//
// Deliberately uses a large first value (12345, not 1): every other test in
// this file happens to start its series at 1, which cannot distinguish
// "unknown source treated as a fresh baseline" from "unknown source treated
// as if its previous seq were 0" — both behave identically when the first
// real seq is 1. Confirmed by mutation: dropping noteSeq's `!known` case (so
// an untracked source silently compares against a zero-value prev) passes
// every other gap/restart test in the package but fails only this one.
func TestUnit_StreamReader_FirstSeqEstablishesBaselineRegardlessOfValue(t *testing.T) {
	mr := newMiniRedis(t)
	r, err := New(newClient(t, mr), Config{Transport: Streams, Readers: 1})
	require.NoError(t, err)
	t.Cleanup(func() { _ = r.Close() })

	key := r.scfg.syncKey("room1")
	r.noteSeq(key, []byte(nodeB), 12345)
	require.Zero(t, r.StreamStats().Gaps, "a source's first observed seq is a baseline, never a gap")
	require.Zero(t, r.StreamStats().Restarts)

	// And normal tracking proceeds from that baseline.
	r.noteSeq(key, []byte(nodeB), 12346)
	require.Zero(t, r.StreamStats().Gaps)
}

// A healthy node publishing to SEVERAL rooms must leave a reader's Gaps and
// Restarts at zero. This is the assertion that protects the contract:
// StreamStats.Gaps is documented "ALERT ON PRESENCE, not on rate — a single
// gap means data was lost", so a counter that ticks during ordinary
// multi-room operation is worse than no counter at all, and issue #196 reads
// this number directly.
//
// Two phases, because the two halves of the defect need different traffic
// shapes to expose them, and one of them is invisible under the other's
// shape:
//
//   - Interleaved across rooms catches the PUBLISHER half. With one counter
//     shared across rooms, room1's stream held [1 3 5] and room2's [2 4 6];
//     measured Gaps=4 against that counter.
//   - Room-at-a-time catches the READER half. With lastSeq keyed by nodeID
//     alone, one node's two per-stream series concatenate into 4,5,6 then
//     4,5,6 and the second one reads as a DECREASE; measured Restarts=1
//     against that keying. Interleaved traffic hides it entirely, because
//     4,4,5,5,6,6 contains neither a jump nor a decrease — which is why this
//     phase exists and why it waits for delivery between rooms.
//
// Both mutations recorded in task-6-report.md.
func TestIntegration_StreamReader_HealthyMultiRoomReaderRecordsNoGaps(t *testing.T) {
	mr := newMiniRedis(t)

	sink := &recordingSink{}
	a, err := New(newClient(t, mr), readerConfig(nodeA))
	require.NoError(t, err)
	t.Cleanup(func() { _ = a.Close() })
	b, err := New(newClient(t, mr), readerConfig(nodeB))
	require.NoError(t, err)
	t.Cleanup(func() { _ = b.Close() })

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	require.NoError(t, a.Start(ctx, sink))
	require.NoError(t, b.Start(ctx, &countingSink{}))
	a.RoomActivated("room1")
	a.RoomActivated("room2")

	publish := func(room, text string) {
		require.NoError(t, b.Publish(ctx, cluster.Outbound{
			Room: room, Kind: cluster.KindSync, Data: v1Update(t, text),
		}))
	}
	// A witness on every phase: without it a Gaps assertion would pass on a
	// reader that never delivered anything at all.
	waitSeen := func(texts ...string) {
		require.Eventually(t, func() bool {
			for _, text := range texts {
				if !sink.payloadSeen(text) {
					return false
				}
			}
			return true
		}, 5*time.Second, 10*time.Millisecond, "every edit must be delivered: %v", texts)
	}

	// Phase 1: interleaved across rooms, which is what a node hosting two
	// rooms ordinarily does.
	var interleaved []string
	for i := 0; i < 3; i++ {
		for _, room := range []string{"room1", "room2"} {
			text := fmt.Sprintf("edit-%s-%d", room, i)
			interleaved = append(interleaved, text)
			publish(room, text)
		}
	}
	waitSeen(interleaved...)

	// Phase 2: one room's whole batch delivered before the other's starts, so
	// the reader observes the two per-stream series back to back.
	for _, room := range []string{"room1", "room2"} {
		var batch []string
		for i := 3; i < 6; i++ {
			text := fmt.Sprintf("edit-%s-%d", room, i)
			batch = append(batch, text)
			publish(room, text)
		}
		waitSeen(batch...)
	}

	require.Equal(t, uint64(0), a.StreamStats().Gaps,
		"a healthy multi-room node must produce NO gaps: Gaps is an alert-on-presence signal")
	require.Equal(t, uint64(0), a.StreamStats().Restarts,
		"no node restarted, so nothing may be reported as one")
}

// --- Backpressure: declining the cursor advance ----------------------------

// streamEntry builds one XREAD entry the way go-redis surfaces it, so a unit
// test can drive handleStream without a round trip through Redis.
//
// Built from streamFields, the real encoder, rather than from a hand-written
// map: a literal here would keep passing if the wire field names changed under
// it, and would then be testing a format nothing writes.
func streamEntry(t *testing.T, id, node string, seq uint64, kind cluster.Kind, data []byte) goredis.XMessage {
	t.Helper()
	fields := streamFields([]byte(node), seq, kind, data)
	vals := make(map[string]any, len(fields)/2)
	for i := 0; i < len(fields); i += 2 {
		name, ok := fields[i].(string)
		require.True(t, ok, "field name %d is not a string", i)
		switch v := fields[i+1].(type) {
		case string:
			vals[name] = v
		case []byte:
			// Redis returns every field as a bulk string; go-redis hands them
			// back as Go strings. See streams_test.go's note on this.
			vals[name] = string(v)
		default:
			t.Fatalf("field %q has unexpected type %T", name, v)
		}
	}
	return goredis.XMessage{ID: id, Values: vals}
}

// wedgeLane registers a room worker whose lane is at capacity and which has NO
// goroutine, so nothing drains it. That is exactly the state a room reaches
// when its Sink.Inject is wedged, and it is reachable here without a running
// websocket server and without a test-only hook in production code — an
// earlier draft of this change carried a laneFullForTest func on Relay, which
// this makes unnecessary.
func wedgeLane(t *testing.T, r *Relay, room string, capacity int) *roomWorker {
	t.Helper()
	w := &roomWorker{room: room, lane: relaylane.New(capacity), done: make(chan struct{})}
	r.workersMu.Lock()
	r.workers[room] = w
	r.workersMu.Unlock()

	for i := 0; i < capacity; i++ {
		w.lane.Push(cluster.KindSync, v1Update(t, fmt.Sprintf("filler-%d", i)))
	}
	require.True(t, w.lane.Full(), "the lane must really be at capacity")
	require.Zero(t, r.Stats().Coalesced, "filling to capacity must not itself have merged")
	return w
}

// This is what a durable stream buys that pub/sub cannot. With the local lane
// at capacity, pub/sub's only options are to merge the backlog or drop it,
// because the message exists nowhere else. Here the reader declines to advance
// the cursor and reads the same entries again next cycle: nothing is
// discarded, and the lane is not made to merge.
func TestUnit_StreamReader_FullLaneDeclinesTheCursorAdvance(t *testing.T) {
	mr := newMiniRedis(t)
	// Deliberately NOT Started: no reader goroutine and no worker goroutine,
	// so the lane this test fills stays full and the counters it asserts on
	// are touched by nothing else.
	r, err := New(newClient(t, mr), Config{Transport: Streams, Readers: 1, RoomQueueSize: 2})
	require.NoError(t, err)
	t.Cleanup(func() { _ = r.Close() })

	w := wedgeLane(t, r, "room1", 2)
	key := r.scfg.syncKey("room1")
	r.setCursor(key, "5-0")

	tgt := streamTarget{key: key, room: "room1"}
	msgs := []goredis.XMessage{
		streamEntry(t, "6-0", nodeB, 1, cluster.KindSync, v1Update(t, "held-back")),
	}

	require.Equal(t, streamStalled, r.handleStream(tgt, msgs))
	require.Equal(t, "5-0", r.cursorFor(key, oldestID),
		"the cursor must stay put so the entry is read again next cycle")
	require.Equal(t, uint64(1), r.StreamStats().Stalled)
	require.Equal(t, uint64(0), r.StreamStats().Deferred,
		"the room HAS a worker; this is backpressure, not the activation window")
	require.Equal(t, uint64(0), r.Stats().RouterDrops, "nothing was discarded, only deferred")
	require.Equal(t, 2, w.lane.Depth(), "the payload must not be pushed onto a full lane")
	require.Equal(t, uint64(0), r.Stats().Coalesced, "and the lane must not be made to merge")

	// The sequence number must NOT have been recorded. The entry is going to be
	// read again, and a lastSeq holding it would make the re-read's lower
	// number look like the publisher had restarted.
	r.streamMu.Lock()
	_, known := r.lastSeq[seqSource{node: nodeB, stream: key}]
	r.streamMu.Unlock()
	require.False(t, known, "a stalled stream must not record sequences it did not consume")

	// The other half of the promise: once the lane drains, the SAME entries go
	// through and the cursor moves on.
	_, ok := w.lane.TakeSync()
	require.True(t, ok)
	require.Equal(t, streamConsumed, r.handleStream(tgt, msgs))
	require.Equal(t, "6-0", r.cursorFor(key, oldestID), "a drained lane must advance the cursor")
	require.Equal(t, 1, w.lane.Depth(), "and the held-back payload must be delivered")
	require.Equal(t, uint64(1), r.StreamStats().Stalled, "a clean pass must not count as a stall")
}

// Awareness must NOT be held back by a full lane. Lane.Full reports on the
// sync queue, the only thing the lane's capacity governs; an awareness push
// replaces a single latest-only slot, so declining one would cost presence
// freshness and buy nothing — and would re-read presence that the next push
// supersedes anyway.
func TestUnit_StreamReader_AwarenessIsExemptFromLaneBackpressure(t *testing.T) {
	mr := newMiniRedis(t)
	r, err := New(newClient(t, mr), Config{Transport: Streams, Readers: 1, RoomQueueSize: 2})
	require.NoError(t, err)
	t.Cleanup(func() { _ = r.Close() })

	w := wedgeLane(t, r, "room1", 2)
	awKey := r.scfg.awKey("room1")
	// residency is what readBatch would have captured: the worker the room was
	// on when the id vector was built. See deliverAwareness.
	tgt := streamTarget{key: awKey, room: "room1", isAwareness: true, residency: w}
	msgs := []goredis.XMessage{
		streamEntry(t, "7-0", nodeB, 1, cluster.KindAwareness, []byte("presence")),
	}

	require.Equal(t, streamConsumed, r.handleStream(tgt, msgs))
	_, from := r.awarenessCursor("room1")
	require.Equal(t, "7-0", from, "an awareness stream advances regardless")
	require.Equal(t, uint64(0), r.StreamStats().Stalled, "awareness must not register as a stall")
	require.Equal(t, 3, w.lane.Depth(), "the awareness blob must be delivered to the lane")
	require.Equal(t, uint64(0), r.Stats().Coalesced,
		"and the awareness slot is separate, so nothing merged")
}

// One read of a busy presence stream returns several blobs; the lane holds
// ONE. Pushing all of them would supersede the reader's own work N-1 times —
// N-1 lane-mutex acquisitions and Signal sends discarded on the spot, and N-1
// AwarenessSuperseded increments reporting a backlog this room's worker never
// had. Only the last payload of a read is pushed. See deliverAwareness.
func TestUnit_StreamReader_AwarenessBatchPushesOnlyTheLatest(t *testing.T) {
	mr := newMiniRedis(t)
	// Deliberately NOT Started: with no worker goroutine, the lane's depth and
	// contents are assertions rather than a race with a concurrent drain.
	r, err := New(newClient(t, mr), Config{Transport: Streams, Readers: 1})
	require.NoError(t, err)
	t.Cleanup(func() { _ = r.Close() })

	w := &roomWorker{room: "room1", lane: relaylane.New(r.laneCap), done: make(chan struct{})}
	r.workersMu.Lock()
	r.workers["room1"] = w
	r.workersMu.Unlock()

	tgt := streamTarget{
		key: r.scfg.awKey("room1"), room: "room1", isAwareness: true, residency: w,
	}
	msgs := []goredis.XMessage{
		streamEntry(t, "7-0", nodeB, 1, cluster.KindAwareness, []byte("presence-1")),
		streamEntry(t, "8-0", nodeB, 2, cluster.KindAwareness, []byte("presence-2")),
		streamEntry(t, "9-0", nodeB, 3, cluster.KindAwareness, []byte("presence-3")),
	}

	require.Equal(t, streamConsumed, r.handleStream(tgt, msgs))

	require.Equal(t, uint64(0), r.Stats().AwarenessSuperseded,
		"a three-entry read must cost ONE push, not three: the other two would be "+
			"superseded before anything could read them, and each would leave an "+
			"AwarenessSuperseded increment claiming this room's worker fell behind")
	require.Equal(t, 1, w.lane.Depth(), "so the lane holds exactly one blob")
	got, ok := w.lane.TakeAwareness()
	require.True(t, ok)
	require.Equal(t, []byte("presence-3"), got,
		"and it is the LAST payload of the read, the only one still current")

	_, from := r.awarenessCursor("room1")
	require.Equal(t, "9-0", from,
		"the cursor still advances past the WHOLE read, or the superseded entries are re-read forever")
}

// Backoff must grow so a wedged room is not re-read as fast as Redis can
// answer, and must be capped at the reader's own read interval so recovery
// stays prompt and no pause outlives a bound ReadBlock documents.
func TestUnit_StreamReader_StallBackoffGrowsAndCaps(t *testing.T) {
	const limit = maxReadBlock // 250ms, the largest ReadBlock accepted

	require.Equal(t, stalledBackoffBase, stallBackoff(1, limit))
	require.Equal(t, 2*stalledBackoffBase, stallBackoff(2, limit))
	require.Equal(t, 4*stalledBackoffBase, stallBackoff(3, limit))
	require.Equal(t, limit, stallBackoff(4, limit),
		"the fourth doubling would be 400ms, past the cap")

	// Monotone, positive and capped for every streak length, including ones
	// long past the point where a shift of the base would have overflowed to a
	// negative duration (stalledBackoffBase << 37 does).
	prev := time.Duration(0)
	for n := 1; n <= 64; n++ {
		d := stallBackoff(n, limit)
		require.GreaterOrEqual(t, d, prev, "backoff must be monotone at n=%d", n)
		require.LessOrEqual(t, d, limit, "backoff must never exceed the cap at n=%d", n)
		require.Positive(t, d, "backoff must stay positive at n=%d", n)
		prev = d
	}

	// A ReadBlock below the base is legal, down to minReadBlock, and then the
	// cap wins outright: a reader must never pause longer than its own read
	// interval.
	require.Equal(t, minReadBlock, stallBackoff(1, minReadBlock))
	require.Equal(t, minReadBlock, stallBackoff(9, minReadBlock))
}

// THE BACKPRESSURE HEADLINE, end to end. A room whose consumer is wedged
// stalls its reader, and when the consumer frees up every entry published
// during the stall is still delivered. Delivery is at-least-once within
// min(retention, MaxLen/publish-rate); this test stays far inside that window,
// which is what makes the entries still be there.
//
// The pub/sub tier structurally cannot pass this: at a full lane it must
// either merge the backlog or drop it.
func TestIntegration_StreamReader_WedgedConsumerLosesNothing(t *testing.T) {
	mr := newMiniRedis(t)

	// RoomQueueSize 1: a single queued payload makes the lane full, so the
	// stall is reached in a few cycles instead of dozens.
	acfg := readerConfig(nodeA)
	acfg.RoomQueueSize = 1

	sink := &recordingSink{gate: make(chan struct{}), entered: make(chan struct{})}
	gateOnce := sync.Once{}
	openGate := func() { gateOnce.Do(func() { close(sink.gate) }) }
	defer openGate() // never leave a worker parked, even on a failure path

	a, err := New(newClient(t, mr), acfg)
	require.NoError(t, err)
	t.Cleanup(func() { _ = a.Close() })
	b, err := New(newClient(t, mr), readerConfig(nodeB))
	require.NoError(t, err)
	t.Cleanup(func() { _ = b.Close() })

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	require.NoError(t, a.Start(ctx, sink))
	require.NoError(t, b.Start(ctx, &countingSink{}))
	a.RoomActivated("room1")

	// Publish until the reader has actually declined an advance. A fixed
	// number of publishes could race the cycle boundary and never observe the
	// stall; every publish here appends a real entry, so the loop always makes
	// progress toward the condition, and every text it published is asserted
	// on below.
	var texts []string
	deadline := time.Now().Add(20 * time.Second)
	for a.StreamStats().Stalled == 0 {
		require.False(t, time.Now().After(deadline),
			"the reader never declined an advance after %d publishes", len(texts))
		text := fmt.Sprintf("wedged-edit-%d", len(texts))
		texts = append(texts, text)
		require.NoError(t, b.Publish(ctx, cluster.Outbound{
			Room: "room1", Kind: cluster.KindSync, Data: v1Update(t, text),
		}))
		time.Sleep(10 * time.Millisecond)
	}
	require.GreaterOrEqual(t, len(texts), 2, "a stall needs the lane to have filled first")

	// Delivery is genuinely parked, and the cursor is being held back rather
	// than the entries discarded.
	select {
	case <-sink.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the sink never parked, so nothing was wedged")
	}
	require.Equal(t, uint64(0), a.Stats().RouterDrops, "a stall discards nothing")
	require.Equal(t, uint64(0), a.Stats().HardDrops)

	// Let the consumer go. Everything published during the stall must arrive.
	openGate()
	require.Eventually(t, func() bool {
		for _, text := range texts {
			if !sink.payloadSeen(text) {
				return false
			}
		}
		return true
	}, 20*time.Second, 20*time.Millisecond,
		"every entry held back by the stall must be delivered once the lane drains")

	require.Equal(t, uint64(0), a.StreamStats().Gaps,
		"nothing was trimmed out from under the reader, so no gap may be reported")
	require.Equal(t, uint64(0), a.Stats().RouterDrops)
	require.Equal(t, uint64(0), a.Stats().HardDrops)
}

// The pacing decision itself: a stall streak must escalate, any non-stalled
// cycle must reset it, and a cycle that both stalled and deferred must be
// paced for the stall — the longer-lived of the two conditions.
func TestUnit_StreamReader_PacingEscalatesAndResets(t *testing.T) {
	const readBlock = maxReadBlock
	stalledCycle := readResult{got: true, deferred: true, stalled: true}
	unreadyCycle := readResult{got: true, deferred: true}
	cleanCycle := readResult{got: true}

	// A consecutive run escalates.
	stalls := 0
	var seen []time.Duration
	for i := 0; i < 4; i++ {
		var d time.Duration
		d, stalls = nextPause(stalledCycle, stalls, readBlock)
		seen = append(seen, d)
	}
	require.Equal(t, []time.Duration{
		stalledBackoffBase, 2 * stalledBackoffBase, 4 * stalledBackoffBase, readBlock,
	}, seen, "consecutive stalls must double until the cap")
	require.Equal(t, 4, stalls)

	// A clean cycle resets the streak outright, so the next stall starts over
	// at the base rather than resuming an escalation it no longer needs.
	d, stalls := nextPause(cleanCycle, stalls, readBlock)
	require.Zero(t, d, "a clean cycle must not pause at all")
	require.Zero(t, stalls)
	d, stalls = nextPause(stalledCycle, stalls, readBlock)
	require.Equal(t, stalledBackoffBase, d, "recovery must be prompt, not penalised")
	require.Equal(t, 1, stalls)

	// So does a cycle deferred only for a missing worker: no lane was full, so
	// there is no backpressure streak to continue. That one gets the short flat
	// pause, because the window it waits out is short.
	d, stalls = nextPause(unreadyCycle, stalls, readBlock)
	require.Equal(t, deferredReadBackoff, d)
	require.Zero(t, stalls, "an unready cycle is not a stall")

	// Both at once is paced as a stall.
	d, _ = nextPause(stalledCycle, 0, readBlock)
	require.Equal(t, stalledBackoffBase, d, "a stalled cycle outranks a merely deferred one")
	require.Greater(t, stallBackoff(3, readBlock), deferredReadBackoff,
		"and escalation must be able to exceed the flat deferral pause, or it buys nothing")
}

// The cycle must carry the stall out to the pacer, and must carry the two
// deferral causes out SEPARATELY. A cycle that reported a stall only as a
// plain deferral would be paced with the short flat pause forever, so the
// escalation would never happen and a wedged room would be re-read at
// deferredReadBackoff's rate indefinitely.
//
// Driven by calling readOnce directly on a relay that was never Started, so
// there is no reader goroutine and no worker goroutine to race the assertions.
func TestUnit_StreamReader_ReadOnceReportsEachDeferralCauseSeparately(t *testing.T) {
	mr := newMiniRedis(t)
	r, err := New(newClient(t, mr), Config{
		Transport: Streams, Readers: 1, ReadBlock: readerTestBlock, RoomQueueSize: 1,
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = r.Close() })

	// Two rooms on the one reader: "wedged" has a worker whose lane is full,
	// "unready" has no worker at all.
	appendEntry := func(room, text string) {
		require.NoError(t, r.client.XAdd(context.Background(), &goredis.XAddArgs{
			Stream: r.scfg.syncKey(room),
			Values: streamFields([]byte(nodeB), 1, cluster.KindSync, v1Update(t, text)),
		}).Err())
	}
	r.streamMu.Lock()
	r.streamRooms["unready"] = 1
	r.streamMu.Unlock()
	appendEntry("unready", "unready-edit")

	res, err := r.readOnce(context.Background(), 0)
	require.NoError(t, err)
	require.True(t, res.got)
	require.True(t, res.deferred, "a missing worker defers")
	require.False(t, res.stalled, "but it is not lane backpressure")
	require.Equal(t, uint64(1), r.StreamStats().Deferred)
	require.Equal(t, uint64(0), r.StreamStats().Stalled)

	// Now the wedged room, alongside it.
	wedgeLane(t, r, "wedged", 1)
	r.streamMu.Lock()
	r.streamRooms["wedged"] = 1
	r.streamMu.Unlock()
	appendEntry("wedged", "wedged-edit")

	res, err = r.readOnce(context.Background(), 0)
	require.NoError(t, err)
	require.True(t, res.stalled,
		"a stalled stream must reach the pacer as a stall, or nothing ever escalates")
	require.True(t, res.deferred, "a stall is also a deferral: the same entries come back")
	require.Equal(t, uint64(1), r.StreamStats().Stalled)
	require.Equal(t, uint64(2), r.StreamStats().Deferred,
		"the unready room deferred again; the two causes are counted apart")

	// And neither cursor moved, so both entries are still there to be read.
	require.Equal(t, oldestID, r.cursorFor(r.scfg.syncKey("unready"), oldestID))
	require.Equal(t, oldestID, r.cursorFor(r.scfg.syncKey("wedged"), oldestID))
}
