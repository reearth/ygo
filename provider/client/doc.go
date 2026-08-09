// Package client implements an embeddable offline-first sync client for ygo
// (#165): a *crdt.Doc plus a durable local store, wired to a y-websocket
// server via the same wire protocol provider/websocket speaks, so that an
// application can read and edit its document at any time — connected,
// disconnected, or never-yet-connected — without special-casing the offline
// case in application code.
//
// # Why a local store, given the protocol already resyncs
//
// The y-protocol sync handshake (SyncStep1/SyncStep2) is, by construction,
// already an "offline flush": step 1 is the client's state vector, step 2 is
// the server's answer of "here is everything you're missing", and any
// updates the client made before the handshake are simply queued and sent as
// part of the same exchange once the connection exists. A client that is
// connected — or that successfully reconnects — never needs a local store to
// converge; the handshake alone reconciles offline edits with the server the
// moment a connection reappears.
//
// So the local store this package adds is not there to make sync work. It
// exists for the case the handshake cannot help with at all: the process
// restarting (or the device losing power) while still offline. A *crdt.Doc
// lives in memory; without something writing its updates to disk as they
// happen, an app that edits offline and then loses the process — closes the
// app, the OS kills it, the laptop sleeps and the process is reaped, whatever
// — loses every edit made since the last successful sync. The store's entire
// job is to survive that gap: every locally-originated update is persisted
// (LocalStore.StoreUpdate) as it is produced, and on the next New/Connect the
// same bytes are loaded back (LocalStore.LoadDoc) into a fresh in-memory Doc
// before that Doc is exposed to the caller or a dial is even attempted.
//
// That hydrate-before-dial ordering is what makes this client "offline-first"
// rather than merely "offline-tolerant": an application can call New,
// immediately start reading and editing the Doc, and get back everything it
// persisted last time, even with the network unreachable or the server
// down — because hydration never waits on the network to begin with.
// Connect's job past that point — the dial/handshake loop — is purely to
// reconcile with the server when and if one becomes reachable; it is not on
// the path an app needs for offline reads or edits to work, which is why a
// dial failure does not make Connect give up.
//
// # What gets stored, and what gets sent
//
// Everything applied to the Doc is stored, whatever produced it: the app's
// own edits AND updates received from the server. Storing only local edits
// would mean a client that syncs a large document, closes, and reopens
// offline hydrates back only what it typed itself, silently discarding
// everything it ever learned from the server.
//
// Sending is the opposite way round, and that is what the sentinel origin is
// for. Updates the client applies from the server carry a distinct origin
// (see remoteOrigin, and its doc comment for why that sentinel must be a
// non-zero-size type), so the observer can tell them apart from the app's own
// edits and decline to bounce them straight back down the socket they arrived
// on. Hydration uses the same sentinel but never reaches the observer at all:
// Connect registers it after hydrating, precisely so replaying the store
// cannot echo onto the wire.
//
// # Concurrency
//
// Connect runs one goroutine that owns the socket, and it is the only writer
// to it. The Doc observer runs on whichever goroutine called Transact — the
// application's own — so it never writes to the socket and never blocks on
// the network: it hands the update to a bounded, coalescing queue
// (internal/relaylane) and returns immediately. An application's edit is
// therefore never slowed by a slow server, which is the client-side
// counterpart of the head-of-line coupling provider/websocket removed in
// #187.
package client
