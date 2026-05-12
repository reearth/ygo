// Package http provides a REST HTTP handler for Yjs document synchronisation.
//
// Endpoints:
//
//	GET  /doc/{room}?sv=<base64-state-vector>  — returns a binary update diff
//	POST /doc/{room}                            — applies a binary update body
//
// # Quick start
//
//	srv := http.NewServer()
//	// Mount under a prefix; the remainder of the path is the room name.
//	mux.Handle("/sync/", nethttp.StripPrefix("/sync", srv))
//	nethttp.ListenAndServe(":8080", mux)
//
// See the Example* functions for canonical usage patterns.
//
// # Stability
//
// ygo follows semantic versioning. The v1.x public API is considered
// stable: new functionality lands as minor releases; bug fixes as patch
// releases; breaking changes are deferred to v2.
package http
