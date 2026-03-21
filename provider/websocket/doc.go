// Package websocket provides a net/http-compatible WebSocket handler that
// synchronises Yjs documents between multiple peers using the y-protocols
// sync and awareness protocols.
//
// Usage:
//
//	srv := websocket.NewServer()
//	http.Handle("/yjs/{room}", srv)
//	http.ListenAndServe(":8080", nil)
package websocket
