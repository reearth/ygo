package websocket

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// versionAdapter is a recordAdapter that also implements VersionableAdapter.
type versionAdapter struct {
	recordAdapter

	vmu    sync.Mutex
	labels []string
	nextID int64
}

func (a *versionAdapter) SaveVersion(_ context.Context, _ string, label string) (int64, error) {
	a.vmu.Lock()
	defer a.vmu.Unlock()
	a.nextID++
	a.labels = append(a.labels, label)
	return a.nextID, nil
}

func (a *versionAdapter) versionCount() int {
	a.vmu.Lock()
	defer a.vmu.Unlock()
	return len(a.labels)
}

func (a *versionAdapter) versionLabels() []string {
	a.vmu.Lock()
	defer a.vmu.Unlock()
	return append([]string(nil), a.labels...)
}

const avWindow = 50 * time.Millisecond

// newAutoVersionServer wires a coalescing server with auto-versioning enabled.
func newAutoVersionServer(a PersistenceAdapter, fc *fakeClock, every time.Duration) *Server {
	s := newCoalesceServer(a, fc, avWindow, 500*time.Millisecond)
	s.AutoVersionEvery = every
	return s
}

// flushOnce applies one edit and forces a synchronous durable flush via the
// room's flushReq barrier, so exactly one store completes.
//
// The barrier is used instead of firing window timers because it is
// deterministic across repeated flushes: the flushReq case drains persistCh
// before flushing by contract, whereas hunting successive window timers can pick
// up one armed by an earlier batch.
func flushOnce(t *testing.T, r *room) {
	t.Helper()
	applyEdit(r, "x", 0)
	ack := make(chan bool, 1)
	r.flushReq <- ack
	require.True(t, <-ack, "flush should report durable success")
}

// A flush before the interval has elapsed must NOT create a version: this is the
// anti-churn guarantee that makes a history panel usable.
func TestAutoVersion_NotBeforeInterval(t *testing.T) {
	a := &versionAdapter{}
	fc := newFakeClock()
	s := newAutoVersionServer(a, fc, time.Minute)
	r, _, err := s.getOrCreateRoom(context.Background(), "doc1")
	require.NoError(t, err)

	flushOnce(t, r)
	assert.Eventually(t, func() bool { return a.count() == 1 }, time.Second, 5*time.Millisecond,
		"the edit should have been persisted")

	// Clock has not advanced, so no version is due.
	assert.Equal(t, 0, a.versionCount(), "no version before AutoVersionEvery elapses")
}

// Once the interval has elapsed, the next flush creates exactly one version.
func TestAutoVersion_OncePerIntervalOnChange(t *testing.T) {
	a := &versionAdapter{}
	fc := newFakeClock()
	s := newAutoVersionServer(a, fc, time.Minute)
	r, _, err := s.getOrCreateRoom(context.Background(), "doc1")
	require.NoError(t, err)

	fc.advance(2 * time.Minute)
	flushOnce(t, r)

	assert.Eventually(t, func() bool { return a.versionCount() == 1 }, time.Second, 5*time.Millisecond,
		"a version is due once the interval elapsed and the room changed")
	assert.Equal(t, []string{AutoVersionLabel}, a.versionLabels(),
		"server-created versions carry AutoVersionLabel")

	// A second flush immediately after must not version again: the interval
	// restarts from the version just taken.
	flushOnce(t, r)
	assert.Eventually(t, func() bool { return a.count() == 2 }, time.Second, 5*time.Millisecond)
	assert.Equal(t, 1, a.versionCount(), "interval restarts after a version is taken")

	// After another interval, one more version.
	fc.advance(2 * time.Minute)
	flushOnce(t, r)
	assert.Eventually(t, func() bool { return a.versionCount() == 2 }, time.Second, 5*time.Millisecond,
		"a further interval yields one more version")
}

// A quiet room must never be versioned, however much time passes. Versions track
// change, not wall-clock.
func TestAutoVersion_QuietRoomNeverVersioned(t *testing.T) {
	a := &versionAdapter{}
	fc := newFakeClock()
	s := newAutoVersionServer(a, fc, time.Minute)
	_, _, err := s.getOrCreateRoom(context.Background(), "doc1")
	require.NoError(t, err)

	fc.advance(24 * time.Hour)
	// No edits at all.
	assert.Never(t, func() bool { return a.versionCount() > 0 }, 200*time.Millisecond, 10*time.Millisecond,
		"a room with no changes must not be versioned")
}

// On unload, a room that changed since its last version gets one final version
// so the end of a session is not lost.
func TestAutoVersion_FinalVersionOnUnloadWhenDirty(t *testing.T) {
	a := &versionAdapter{}
	fc := newFakeClock()
	s := newAutoVersionServer(a, fc, time.Hour) // long interval: no interval-driven version
	r, _, err := s.getOrCreateRoom(context.Background(), "doc1")
	require.NoError(t, err)

	flushOnce(t, r)
	assert.Eventually(t, func() bool { return a.count() == 1 }, time.Second, 5*time.Millisecond)
	require.Equal(t, 0, a.versionCount(), "interval not elapsed, so no version yet")

	// Unload the room.
	require.NoError(t, s.CloseRoom("doc1", true))

	assert.Eventually(t, func() bool { return a.versionCount() == 1 }, time.Second, 5*time.Millisecond,
		"unload should capture a final version for a dirty room")
}

// On unload, a room with no changes since its last version must NOT be versioned
// again, otherwise every open/close cycle would add a duplicate entry.
func TestAutoVersion_NoFinalVersionWhenClean(t *testing.T) {
	a := &versionAdapter{}
	fc := newFakeClock()
	s := newAutoVersionServer(a, fc, time.Minute)
	r, _, err := s.getOrCreateRoom(context.Background(), "doc1")
	require.NoError(t, err)

	// Elapse the interval and flush, taking a version and clearing the dirty flag.
	fc.advance(2 * time.Minute)
	flushOnce(t, r)
	assert.Eventually(t, func() bool { return a.versionCount() == 1 }, time.Second, 5*time.Millisecond)

	// Unload with no further edits.
	require.NoError(t, s.CloseRoom("doc1", true))

	assert.Never(t, func() bool { return a.versionCount() > 1 }, 200*time.Millisecond, 10*time.Millisecond,
		"a clean room must not be versioned again on unload")
}

// Auto-versioning is opt-in: with AutoVersionEvery == 0 the adapter is never
// asked for a version, even on unload.
func TestAutoVersion_DisabledByDefault(t *testing.T) {
	a := &versionAdapter{}
	fc := newFakeClock()
	s := newCoalesceServer(a, fc, avWindow, 500*time.Millisecond) // AutoVersionEvery unset
	r, _, err := s.getOrCreateRoom(context.Background(), "doc1")
	require.NoError(t, err)

	fc.advance(24 * time.Hour)
	flushOnce(t, r)
	assert.Eventually(t, func() bool { return a.count() == 1 }, time.Second, 5*time.Millisecond)
	require.NoError(t, s.CloseRoom("doc1", true))

	assert.Never(t, func() bool { return a.versionCount() > 0 }, 200*time.Millisecond, 10*time.Millisecond,
		"auto-versioning must be off unless AutoVersionEvery > 0")
}

// An adapter that does not implement VersionableAdapter must be unaffected: the
// server must not panic or stall when AutoVersionEvery is set.
func TestAutoVersion_NonVersionableAdapterIgnored(t *testing.T) {
	a := &recordAdapter{} // no SaveVersion
	fc := newFakeClock()
	s := newAutoVersionServer(a, fc, time.Minute)
	r, _, err := s.getOrCreateRoom(context.Background(), "doc1")
	require.NoError(t, err)

	fc.advance(2 * time.Minute)
	flushOnce(t, r)
	assert.Eventually(t, func() bool { return a.count() == 1 }, time.Second, 5*time.Millisecond,
		"persistence must work normally for a non-versionable adapter")
	require.NoError(t, s.CloseRoom("doc1", true))
}
