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
// # Stability
//
// ygo follows semantic versioning. The v1.x public API is considered
// stable: new functionality lands as minor releases; bug fixes as patch
// releases; breaking changes are deferred to v2.
package websocket
