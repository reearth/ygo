package mobile

import (
	"context"
	"sync"

	"github.com/reearth/ygo/crdt"
	client "github.com/reearth/ygo/provider/client"
)

// SyncStatusObserver receives connection-lifecycle notifications from a
// SyncClient, one call per underlying provider/client.Status (see SetOnStatus
// and the SyncState* constants below for the state -> int64 mapping). errMsg
// is the error's message when state == SyncStateDisconnected because of a
// failure, and "" for every other delivery, including a clean
// SyncStateDisconnected that follows Close.
//
// OnStatus runs on a background goroutine — never the UI/platform thread,
// and never while this package holds a lock — following the same rule
// mobile/observe.go's DocObserver and AwarenessObserver already document.
// Marshal to the main thread before touching UI state, exactly as those two
// observers require.
type SyncStatusObserver interface {
	OnStatus(state int64, errMsg string)
}

// SyncClient connection states delivered to SyncStatusObserver.OnStatus.
// This mapping is PART OF THE PUBLIC gomobile API: platform code switches on
// these numbers directly (gomobile cannot bind Go's client.State type across
// the boundary), so the values are pinned explicitly here — deliberately NOT
// a bare int64(client.State) cast — rather than inherited from
// provider/client.State's iota order, so a future reordering of that
// package-internal enum can never silently renumber this public contract.
// They are, however, defined in the SAME order as provider/client.State
// today (see mapSyncState) and MUST NOT be renumbered once shipped, since
// existing platform code will already be switching on these exact values.
const (
	// SyncStateConnecting means a dial or handshake attempt is in flight
	// (mirrors provider/client.StateConnecting).
	SyncStateConnecting int64 = 0
	// SyncStateConnected means the WebSocket connection is up but the
	// initial sync handshake has not yet completed (mirrors
	// provider/client.StateConnected).
	SyncStateConnected int64 = 1
	// SyncStateSynced means the sync handshake has completed at least once
	// on the current connection (mirrors provider/client.StateSynced). See
	// SyncedOnce for the latched, poll-friendly form of this same fact.
	SyncStateSynced int64 = 2
	// SyncStateDisconnected means there is no live connection and no attempt
	// is currently in flight — between backoff attempts, or after Close
	// (mirrors provider/client.StateDisconnected). errMsg is non-empty only
	// when this transition was caused by a failure.
	SyncStateDisconnected int64 = 3
)

// mapSyncState translates a provider/client.State into its public SyncState*
// constant. A default case is unreachable for any State provider/client
// itself emits, but is not a panic — an unknown future State degrades to
// SyncStateDisconnected rather than crashing an app on an otherwise-benign
// library upgrade.
func mapSyncState(s client.State) int64 {
	switch s {
	case client.StateConnecting:
		return SyncStateConnecting
	case client.StateConnected:
		return SyncStateConnected
	case client.StateSynced:
		return SyncStateSynced
	case client.StateDisconnected:
		return SyncStateDisconnected
	default:
		return SyncStateDisconnected
	}
}

// SyncClient is a gomobile-safe wrapper around *client.Client (#165): it is
// what makes an on-device Doc (#118/#119) SELF-SYNCING — dialling a
// y-websocket/Hocuspocus server, persisting locally, and reconnecting on its
// own, entirely off the platform UI thread.
//
// Construct one with NewSyncClient, bind Doc() to your UI immediately (it is
// usable before Connect — see Doc's doc), then call Connect once you are
// ready to start syncing. Call Close when done (e.g. ViewModel.onCleared /
// Swift deinit) to release the underlying network and store resources.
type SyncClient struct {
	doc *Doc           // usable before Connect; never nil; never swapped.
	c   *client.Client // never nil; provider/client.Client is safe for concurrent use.

	// mu guards closed and connectStarted, which change together with the
	// decision of whether to spawn Connect's goroutine — see Connect and
	// Close. It is intentionally a plain mutex, not an atomic pair, for the
	// same reason provider/client.Client.connectMu is: Connect's check and
	// its state flip must be one atomic decision, not two racing steps.
	mu             sync.Mutex
	closed         bool
	connectStarted bool

	// statusMu guards observer. It is DISTINCT from mu for the same reason
	// mobile/doc.go's Doc keeps subsMu separate from mu: dispatching a
	// callback must never happen while any lifecycle lock is held (see
	// handleStatus).
	statusMu sync.Mutex
	observer SyncStatusObserver
}

// NewSyncClient constructs a SyncClient for the y-websocket/Hocuspocus room
// at url (see provider/client's roomFromURL — the final URL path segment is
// the room name). dbPath, if non-empty, opens a SQLite-backed local store at
// that path via provider/client.Options.StorePath, so the device's content
// survives a process restart while offline; an empty dbPath means
// memory-only — the Doc is fully usable but starts empty on every restart,
// exactly like provider/client.Options.Store left nil. token, if non-empty,
// is sent as ygo's Hocuspocus in-band auth token (provider/client.
// Options.Token) — see that field's own "NOT a confidentiality gate" doc for
// what it does and does not protect.
//
// NewSyncClient does not touch the network — it returns before any dial is
// attempted, and the returned SyncClient's Doc() is immediately readable and
// editable. Call Connect to begin syncing.
func NewSyncClient(url, dbPath, token string) (*SyncClient, error) {
	rawDoc := crdt.New()
	opts := client.Options{
		URL:   url,
		Doc:   rawDoc,
		Token: token,
	}
	if dbPath != "" {
		opts.StorePath = dbPath
	}
	c, err := client.New(opts)
	if err != nil {
		return nil, err
	}
	s := &SyncClient{
		doc: &Doc{d: rawDoc},
		c:   c,
	}
	c.OnStatus(s.handleStatus)
	return s, nil
}

// Doc returns the *Doc this SyncClient hydrates, edits, and keeps in sync.
// It is usable immediately — before Connect is ever called, and even if
// Connect never succeeds — because offline-first means the UI binds to and
// renders the document right away, exactly as provider/client.Options.Doc's
// own doc describes for the underlying *crdt.Doc. It remains usable after
// Close too: Close stops syncing and releases the network/store, but never
// closes the Doc itself, so previously-synced or offline-edited content is
// never taken away by tearing down the connection.
func (s *SyncClient) Doc() *Doc {
	return s.doc
}

// SetOnStatus registers o to receive this SyncClient's connection-lifecycle
// notifications from here on (no replay of states already reported before
// this call, mirroring provider/client.Client.OnStatus's own no-replay
// contract). A later call replaces the previous observer rather than adding
// a second one — SyncClient supports exactly one platform-side listener,
// matching the "thin binding" scope this type exists for; an app that wants
// to fan a status out to multiple listeners can do so on its own side of the
// boundary. Passing nil detaches the current observer.
func (s *SyncClient) SetOnStatus(o SyncStatusObserver) {
	s.statusMu.Lock()
	s.observer = o
	s.statusMu.Unlock()
}

// handleStatus is the single provider/client.Client.OnStatus subscriber this
// SyncClient registers, in NewSyncClient. It translates a client.Status into
// the (int64, string) shape SyncStatusObserver.OnStatus can cross the
// gomobile boundary with, and — critically — reads the currently-registered
// observer under statusMu and RELEASES that lock before invoking it. #119's
// established rule (see mobile/observe.go's docDrain/awarenessDrain) is that
// no callback ever runs while this package holds a lock: a platform-side
// OnStatus implementation that calls back into this SyncClient (e.g.
// SetOnStatus to replace itself, or reading Doc()) must not be able to
// deadlock against the very lock that looked it up.
func (s *SyncClient) handleStatus(st client.Status) {
	msg := ""
	if st.Err != nil {
		msg = st.Err.Error()
	}
	state := mapSyncState(st.State)

	s.statusMu.Lock()
	o := s.observer
	s.statusMu.Unlock()
	if o != nil {
		o.OnStatus(state, msg)
	}
}

// Connect starts hydrating and syncing this SyncClient's Doc. It returns
// IMMEDIATELY: it spawns the underlying client.Client.Connect call (which
// blocks for the SyncClient's whole sync lifetime) on its own goroutine,
// rather than running it inline, because gomobile calls arrive on the
// platform UI thread — a blocking Connect would freeze the app for as long
// as the client stays connected, which for an offline-first client can be
// its entire lifetime. Progress is instead surfaced asynchronously through
// the SetOnStatus observer (see handleStatus).
//
// Connect is single-use, like the client.Client it wraps: a second call is a
// silent no-op, not an error — SyncClient's Connect signature has no error
// return to report ErrAlreadyConnected through, and the underlying Client
// already guarantees only one dial loop ever runs. Calling Connect after
// Close is also a no-op: closed is checked under the same lock that would
// otherwise let the spawn race Close's own teardown; a Connect call that
// loses that race harmlessly finds client.Client already torn down and
// returns almost immediately (client.Client's own Connect/Close concurrency
// contract — see its doc — covers the remaining sliver of a race where both
// happen at once).
//
// A hydration failure (client.Client.Connect's one error path that returns
// WITHOUT ever reporting anything via OnStatus — see that method's own doc)
// is the one client.Client outcome this wrapper does not merely relay: it is
// reported here as a synthetic SyncStateDisconnected status, so an app
// watching only SetOnStatus is never left with no explanation for why
// nothing happened after Connect. Every other outcome (including the
// terminal ErrAuthRejected case) is already reported by client.Client itself
// before Connect returns; this only adds the one case that would otherwise be
// silent.
func (s *SyncClient) Connect() {
	s.mu.Lock()
	if s.closed || s.connectStarted {
		s.mu.Unlock()
		return
	}
	s.connectStarted = true
	s.mu.Unlock()

	go func() {
		if err := s.c.Connect(context.Background()); err != nil {
			s.handleStatus(client.Status{State: client.StateDisconnected, Err: err})
		}
	}()
}

// SyncedOnce reports whether this SyncClient's Doc has reconciled with the
// server at least once, on any connection so far — the poll-friendly mirror
// of provider/client.Client.Synced()'s channel, for platform code that wants
// a plain boolean rather than a Go channel it cannot receive on across the
// gomobile boundary. Like Synced(), it never flips back to false once true:
// a later disconnect does not un-sync content this Doc has already received.
func (s *SyncClient) SyncedOnce() bool {
	select {
	case <-s.c.Synced():
		return true
	default:
		return false
	}
}

// Close stops syncing and releases this SyncClient's network and local-store
// resources, delegating to client.Client.Close for the actual teardown (see
// that method's doc for the full ordering: signal, join the loop goroutine,
// unsubscribe observers, drain-and-count whatever the lane could not
// deliver, then close the store IF this SyncClient's Connect call opened one
// via dbPath). It is idempotent and safe to call without ever having called
// Connect. It does NOT close Doc() — see Doc's own doc for why the document
// must remain usable after Close.
func (s *SyncClient) Close() {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.closed = true
	s.mu.Unlock()
	_ = s.c.Close()
}
