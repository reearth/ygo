package redis

import (
	"bytes"
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/reearth/ygo/awareness"
	"github.com/reearth/ygo/cluster"
	"github.com/reearth/ygo/crdt"
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
}

func (s *recordingSink) Inject(_ context.Context, in cluster.Inbound) error {
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
	// Outwait any read that was already in flight: its id vector was built
	// before the deactivation, so it can still write a cursor back once.
	time.Sleep(3 * readerTestBlock)

	a.streamMu.Lock()
	_, awKnown := a.cursors[a.scfg.awKey("room1")]
	_, syncKnown := a.cursors[a.scfg.syncKey("room1")]
	a.streamMu.Unlock()
	require.False(t, awKnown,
		"deactivation must forget the awareness cursor, or reactivation resumes mid-presence-stream")
	require.True(t, syncKnown,
		"deactivation must KEEP the sync cursor, or room churn replays the whole retention window")

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
