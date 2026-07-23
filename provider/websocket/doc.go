// Package websocket provides a net/http-compatible WebSocket handler that
// synchronises Yjs documents between multiple peers using the y-protocols
// sync and awareness protocols.
//
// Usage:
//
//	srv := websocket.NewServer()
//	http.Handle("/yjs/{room}", srv)
//	http.ListenAndServe(":8080", nil)
//
// # Quick start
//
//	srv := websocket.NewServer()
//	// Each distinct path is an independent Yjs room.
//	http.Handle("/rooms/", http.StripPrefix("/rooms", srv))
//	http.ListenAndServe(":8080", nil)
//
// See the Example* functions for canonical usage patterns.
//
// # Hocuspocus interop
//
// ygo's y-websocket message dispatcher additionally understands the
// Hocuspocus protocol extensions (tags 4-10: SyncReply, Stateless,
// BroadcastStateless, Close, SyncStatus, Ping, Pong) on the same wire
// framing, and an in-band Auth (tag 2) sub-protocol used by
// @hocuspocus/provider to authenticate after the WebSocket upgrade.
//
// Set Server.OnTokenAuth to validate the token carried in an Auth message:
// a nil error accepts the connection (replying Authenticated with a
// "read-write" or "readonly" scope, per the returned ConnectionConfig); a
// non-nil error replies PermissionDenied and closes the connection with the
// Hocuspocus 4401 code. When OnTokenAuth is nil (the default), Auth (tag 2)
// frames are ignored — the legacy y-websocket behavior, preserved for
// backward compatibility with clients that never send them.
//
// Set Server.HocuspocusFraming to true to additionally read and write the
// Hocuspocus docName-prefixed wire framing (VarString(docName) + frame),
// enabling real interop with an unmodified @hocuspocus/provider client.
// Leave it false (the default) for native y-websocket clients.
//
// # Stability
//
// ygo follows semantic versioning. The v1.x public API is considered
// stable: new functionality lands as minor releases; bug fixes as patch
// releases; breaking changes are deferred to v2.
package websocket
