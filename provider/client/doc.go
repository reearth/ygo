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
// Connect's job past that point (later #165 tasks: the dial/handshake loop)
// is purely to reconcile with the server when and if one becomes reachable;
// it is not on the path an app needs for offline reads or edits to work.
//
// Updates that arrive via hydration (or, once the sync loop exists, via the
// server) are applied under a distinct sentinel origin so the persistence
// hook that writes local edits to the store can recognise and skip them —
// otherwise replaying stored bytes back into the Doc would immediately write
// those same bytes straight back to the store on every restart, and (once
// the sync loop exists) every update received from the server would get
// echoed back into local storage as if the app had made the edit itself. See
// remoteOrigin's own doc comment for why that sentinel must be a non-zero-
// size type.
package client
