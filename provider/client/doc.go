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
// Both questions are decided by one thing: where an update came from. Every
// update applied to the Doc carries an origin, and the client stamps its own
// sentinel origins on the ones it applies itself, so the observer that handles
// storing and sending can tell three cases apart:
//
//   - The app's own edits are stored AND sent. That includes edits made before
//     Connect was ever called — the Doc is usable the moment New returns, so
//     the observer is registered there, not somewhere inside Connect.
//   - Updates received from the server are stored but NOT sent. Storing them
//     is the point of the store: skipping them would mean a client that syncs
//     a large document, closes, and reopens offline hydrates back only what it
//     typed itself, silently discarding everything it ever learned. Not
//     sending them is the echo guard.
//   - The one update hydration applies is neither stored (it came out of the
//     store) nor sent (the handshake conveys full state regardless).
//
// See remoteOrigin and hydrateOrigin for the two sentinels, why they are
// separate types rather than one shared one, and why both must be non-zero-
// size.
//
// # Concurrency
//
// Connect runs one goroutine that owns the socket and is the only writer of
// data frames to it. The Doc observer runs on whichever goroutine called
// Transact — the application's own — and never writes to the socket and never
// blocks on the network: it hands the update to a bounded, coalescing queue
// (internal/relaylane) and returns. An application's edit is therefore never
// slowed by a slow server, which is the client-side counterpart of the
// head-of-line coupling provider/websocket removed in #187.
//
// It IS slowed by a slow Store. The observer calls LocalStore.StoreUpdate
// synchronously, on the application's own goroutine, before handing off — so
// an edit does not return until it is durable. That ordering is deliberate
// (durability is the store's entire purpose here, and an edit reported
// complete before it is safe would defeat it), but it has a real cost worth
// knowing about: the bundled SQLite store serialises all writers behind a
// single connection, so an application edit can queue behind the sync loop
// writing a server-received update, and vice versa. Neither blocks the other
// indefinitely and neither can deadlock, but a store on slow storage shows up
// as edit latency in the application. A store that cannot afford that should
// buffer internally and make StoreUpdate cheap.
package client
