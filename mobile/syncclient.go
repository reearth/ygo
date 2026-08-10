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
//
// # Safe to call SyncClient.Close from inside OnStatus
//
// Unlike the underlying provider/client.Client.OnStatus (whose callbacks run
// synchronously on that Client's own sync-loop goroutine — see that method's
// "fn must never call Close" doc, and Close's own doc for why calling Close
// from inside one deadlocks permanently), this delivery runs on a dedicated
// drain goroutine (see handleStatus/statusDrain) that is never the loop
// goroutine SyncClient.Close ultimately joins. So the obvious Swift/Kotlin
// shape — `onStatus { state, _ -> if (fatal(state)) client.close() }` — is
// safe here specifically because SyncClient interposes that goroutine
// between provider/client's callback and the platform observer; it would
// NOT be safe against the raw Go Client type (#165 final whole-branch
// review, Important B; see TestSyncClient_CloseFromOnStatusDoesNotDeadlock).
type SyncStatusObserver interface {
	OnStatus(state int64, errMsg string)
}

// statusPending is the mailbox between provider/client.Client's OnStatus
// callback (handleStatus, invoked synchronously on that Client's own loop
// goroutine — see emitStatus's doc) and statusDrain, the goroutine that
// actually calls the platform SyncStatusObserver. Unlike mobile/observe.go's
// docPending/awarenessPending, entries here are never merged or coalesced:
// every Status transition is individually meaningful to a platform observer
// (StateConnecting, StateConnected and StateSynced are all distinct signals,
// not supersede-able the way a document update or a presence batch is), so
// this is a plain, unbounded FIFO queue instead — bounded in practice only by
// how far behind a slow or blocked observer falls, which for a lifecycle
// signal firing at most a few times per connection attempt is not a
// realistic concern.
type statusPending struct {
	mu      sync.Mutex
	cond    *sync.Cond
	queue   []client.Status
	stopped bool
}

func newStatusPending() *statusPending {
	p := &statusPending{}
	p.cond = sync.NewCond(&p.mu)
	return p
}

// push enqueues st for delivery by statusDrain. Called synchronously from
// provider/client.Client.emitStatus, on that Client's own loop goroutine (see
// handleStatus) — it must therefore be cheap and non-blocking, exactly like
// mobile/observe.go's Doc/Awareness bridge callbacks; it never calls the
// platform observer itself.
func (p *statusPending) push(st client.Status) {
	p.mu.Lock()
	if p.stopped {
		p.mu.Unlock()
		return
	}
	p.queue = append(p.queue, st)
	p.cond.Signal()
	p.mu.Unlock()
}

// stop signals statusDrain to exit once it next wakes, abandoning any
// still-queued statuses. It does NOT join statusDrain — see that function's
// own doc for why, mirroring mobile/observe.go's Subscription.Close.
func (p *statusPending) stop() {
	p.mu.Lock()
	p.stopped = true
	p.cond.Broadcast()
	p.mu.Unlock()
}

// statusDrain delivers queued Status values to s's currently-registered
// SyncStatusObserver, one at a time, in the order provider/client.Client
// produced them, on a goroutine that is NEVER that Client's own loop
// goroutine — see handleStatus's and SyncStatusObserver's own doc for why
// that separation is the entire point of this indirection. It exits once
// p.stopped is observed, abandoning any still-queued statuses; a status
// already dequeued before that point is still delivered. Not joined by
// SyncClient.Close, for the same reason mobile/observe.go's docDrain/
// awarenessDrain are not joined by Subscription.Close: this goroutine may
// currently be inside the observer's OnStatus call, which is free to call
// back into this SyncClient (including Close itself — see
// SyncStatusObserver's doc), so waiting for it here could deadlock against
// exactly the call this indirection exists to make safe.
func statusDrain(p *statusPending, s *SyncClient) {
	for {
		p.mu.Lock()
		for len(p.queue) == 0 && !p.stopped {
			p.cond.Wait()
		}
		if p.stopped {
			p.mu.Unlock()
			return
		}
		st := p.queue[0]
		p.queue[0] = client.Status{}
		p.queue = p.queue[1:]
		p.mu.Unlock()
		s.deliverStatus(st) // off all locks
	}
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
	// deliverStatus).
	statusMu sync.Mutex
	observer SyncStatusObserver

	// statusPend is the mailbox statusDrain's goroutine reads from and
	// handleStatus writes to — see statusPending's own doc for why this
	// indirection exists (#165 final whole-branch review, Important B:
	// without it, a platform SyncStatusObserver.OnStatus that calls Close
	// deadlocks permanently). Never nil after NewSyncClient; stopped, not
	// joined, by Close.
	statusPend *statusPending
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
		doc:        &Doc{d: rawDoc},
		c:          c,
		statusPend: newStatusPending(),
	}
	c.OnStatus(s.handleStatus)
	go statusDrain(s.statusPend, s)
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
// SyncClient registers, in NewSyncClient. It runs synchronously on
// provider/client.Client's own loop goroutine (see that method's own
// "fn must never call Close" doc) — exactly why it must do nothing here but
// enqueue: it hands st to statusPend and returns immediately, never calling
// the platform observer itself. statusDrain, running on its own goroutine,
// is what actually calls SyncStatusObserver.OnStatus (via deliverStatus)
// — see statusPending's doc for why this indirection exists (#165 final
// whole-branch review, Important B) and SyncStatusObserver's doc for the
// guarantee it buys a platform implementation: safe to call SyncClient.Close
// from inside OnStatus, unlike the raw provider/client.Client this method
// wraps.
func (s *SyncClient) handleStatus(st client.Status) {
	s.statusPend.push(st)
}

// deliverStatus translates one client.Status into the (int64, string) shape
// SyncStatusObserver.OnStatus can cross the gomobile boundary with, and —
// critically — reads the currently-registered observer under statusMu and
// RELEASES that lock before invoking it. #119's established rule (see
// mobile/observe.go's docDrain/awarenessDrain) is that no callback ever runs
// while this package holds a lock: a platform-side OnStatus implementation
// that calls back into this SyncClient (e.g. SetOnStatus to replace itself,
// reading Doc(), or — per SyncStatusObserver's doc — Close) must not be able
// to deadlock against the very lock that looked it up.
//
// Called only from statusDrain, on that dedicated goroutine — never from
// handleStatus directly, and therefore never from provider/client.Client's
// own loop goroutine (see handleStatus's doc for why that separation is the
// entire point).
func (s *SyncClient) deliverStatus(st client.Status) {
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
// Every way client.Client.Connect can end — including a hydration failure —
// is reported via OnStatus before Connect returns (see that method's own
// doc), so this goroutine's return value needs no separate handling here:
// handleStatus has, by construction, already run for it. #165 Task 11's
// review caught an earlier version of this method that relayed the
// hydration-failure case itself, calling handleStatus AFTER s.c.Connect
// returned — i.e. after connectWG.Done() had already fired inside it — which
// could let a concurrent Close's connectWG.Wait() return before that
// synthetic status was ever delivered, breaking client.Client's own
// guarantee that every status it emits completes before Close returns. The
// fix belongs at the source: client.Client.Connect now emits it itself, from
// inside the hydrate-failure branch, still inside the window connectWG
// covers.
func (s *SyncClient) Connect() {
	s.mu.Lock()
	if s.closed || s.connectStarted {
		s.mu.Unlock()
		return
	}
	s.connectStarted = true
	s.mu.Unlock()

	go func() { _ = s.c.Connect(context.Background()) }()
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
// Connect, and — per SyncStatusObserver's own doc — safe to call from inside
// a SyncStatusObserver.OnStatus callback.
//
// By the time s.c.Close() returns, every Status that Client will ever emit
// has already been pushed to statusPend (see client.Client.Close's own doc:
// it joins the loop goroutine before returning, and every status is emitted
// from inside that goroutine's own call stack). statusPend.stop() is called
// AFTER that, so no legitimately-emitted status is lost to the stop
// racing ahead of its own push — but stop does not JOIN statusDrain, for the
// same reason mobile/observe.go's Subscription.Close does not join
// docDrain/awarenessDrain: that goroutine may currently be inside the
// observer's OnStatus call (possibly this very call, if Close was invoked
// from inside one), and waiting for it here would defeat the whole point of
// dispatching off provider/client's own loop goroutine in the first place.
//
// It does NOT close Doc() — see Doc's own doc for why the document must
// remain usable after Close.
func (s *SyncClient) Close() {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.closed = true
	s.mu.Unlock()
	_ = s.c.Close()
	s.statusPend.stop()
}
