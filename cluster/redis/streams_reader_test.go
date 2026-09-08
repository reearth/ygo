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
// which goroutine owns its room. ReadBlock is shortened from the 5s default
// because it also bounds how long an idle reader waits for Redis; leaving it
// at 5s would put the tests' own assertions inside a single XREAD's blocking
// window and make them race the default rather than the code.
func readerConfig(node string) Config {
	return Config{
		Transport: Streams,
		NodeID:    []byte(node),
		Readers:   1,
		ReadBlock: 200 * time.Millisecond,
	}
}

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

// Close must not wait out ReadBlock. Both tests below deliberately use the
// DEFAULT 5s ReadBlock, because that is the configuration the defect appeared
// in and the one every operator gets.
//
// There is no way to interrupt a blocked XREAD — go-redis arms the socket read
// deadline from ctx.Deadline() only, so cancelling the context mid-read has no
// effect — and Close joins r.wg. Before readerBlockCap this Close took a
// measured 4.80s, which is a five-second stall on every Server.Shutdown.
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
	require.Less(t, time.Since(start), 2*time.Second,
		"Close must not block for ReadBlock (%s); a reader cannot be interrupted mid-XREAD, so the block itself has to be capped", defaultReadBlock)
}

// Config.ReadBlock's doc promises "a newly activated room gets a fresh short
// read folded in immediately". A reader already parked in an XREAD for
// room1 cannot see room2 until that read returns, so the read interval IS the
// activation latency. At the default 5s ReadBlock and without readerBlockCap
// this fails: the room2 update arrives about five seconds late.
func TestIntegration_StreamReader_ActivationDoesNotWaitOutReadBlock(t *testing.T) {
	mr := newMiniRedis(t)

	bcfg := readerConfig(nodeB)
	b, err := New(newClient(t, mr), bcfg)
	require.NoError(t, err)
	t.Cleanup(func() { _ = b.Close() })
	require.NoError(t, b.Start(context.Background(), &countingSink{}))

	sink := &recordingSink{}
	// Default ReadBlock (5s) on purpose — see the doc comment.
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
