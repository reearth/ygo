// Package http provides a REST HTTP handler for Yjs document synchronisation.
//
// Endpoints:
//
//	GET  /doc/{room}?sv=<base64-state-vector>  — returns a binary update diff
//	POST /doc/{room}                            — applies a binary update body
package http
