// Package main is the server for the collaborative editor example.
//
// Run with:
//
//	go run ./examples/collab-editor/server
//
// It starts an HTTP server on :8080 that:
//   - Serves the browser client from examples/collab-editor/client/
//   - Handles WebSocket connections at /yjs/{room} using the ygo provider
//
// Two browser tabs opened at http://localhost:8080 will share the same document
// (the default room named "default"). Append a URL hash to switch rooms:
//
//	http://localhost:8080/#team-notes   ← joins room "team-notes"
//
// The hash is consumed entirely in the browser; the server only sees the room
// name once the y-websocket JS library constructs the WebSocket URL.
package main

import (
	"log"
	"net/http"
	"os"
	"path/filepath"

	"github.com/reearth/ygo/provider/websocket"
)

func main() {
	// ── WebSocket provider ────────────────────────────────────────────────────
	//
	// NewServer creates a hub that:
	//   1. Manages one *crdt.Doc per room name.
	//   2. Performs the RFC 6455 WebSocket upgrade itself (no external library).
	//   3. Speaks the y-protocols sync + awareness wire format so that the
	//      standard y-websocket JS client works without modification.
	wsSrv := websocket.NewServer()

	// ── Locate the client directory ───────────────────────────────────────────
	//
	// When the binary is built and run from an arbitrary working directory we
	// need a stable path to the static files. We try two approaches:
	//
	//   1. Relative to the executable path — works for `go build` + run.
	//   2. Relative to the current working directory — works for `go run`.
	//
	// os.Executable() returns the path of the currently running binary.
	// filepath.EvalSymlinks resolves any symlinks (e.g. on macOS temp paths).
	clientDir := resolveClientDir()
	log.Printf("Serving client files from: %s", clientDir)

	// ── HTTP routes ───────────────────────────────────────────────────────────
	//
	// We use a fresh ServeMux rather than the default mux so this example does
	// not accidentally share handlers with any other code in the process.
	mux := http.NewServeMux()

	// /yjs/{room} — WebSocket endpoint.
	//
	// Go 1.22 introduced named wildcards in ServeMux patterns. The {room}
	// segment matches any single path component and makes the captured value
	// available via r.PathValue("room"). This lets us run all rooms on one
	// handler without regex or manual parsing.
	//
	// The y-websocket JS library constructs the WebSocket URL as:
	//
	//   serverUrl + '/' + encodeURIComponent(roomName)
	//
	// So if serverUrl is "ws://localhost:8080/yjs" and roomName is "default",
	// the browser connects to ws://localhost:8080/yjs/default — which matches
	// this pattern.
	mux.Handle("/yjs/{room}", wsSrv)

	// / — Static file server for the single-page client.
	//
	// http.FileServer serves index.html for "/" because the browser requests
	// exactly that path when the user opens http://localhost:8080.
	mux.Handle("/", http.FileServer(http.Dir(clientDir)))

	addr := ":8080"
	log.Printf("Collaborative editor running at http://localhost%s", addr)
	log.Printf("Open two tabs:")
	log.Printf("  Tab 1: http://localhost%s", addr)
	log.Printf("  Tab 2: http://localhost%s", addr)
	log.Printf("Both tabs share the same document. Append #room-name to the URL to use a different room.")

	if err := http.ListenAndServe(addr, mux); err != nil {
		log.Fatalf("ListenAndServe: %v", err)
	}
}

// resolveClientDir returns the best available path to the client/ directory.
//
// Strategy:
//
//  1. Check if the directory exists at the path relative to the executable.
//     This is the correct path when running a compiled binary from any directory.
//
//  2. Fall back to a path relative to the current working directory.
//     This works when using `go run ./examples/collab-editor/server` from the
//     repository root because `go run` sets cwd to the module root.
func resolveClientDir() string {
	// Attempt 1: derive from executable location.
	if exe, err := os.Executable(); err == nil {
		// EvalSymlinks resolves macOS /var → /private/var and similar.
		if real, err := filepath.EvalSymlinks(exe); err == nil {
			// The server binary lives at …/collab-editor/server/server (when built),
			// so the client is two directories up then into client/.
			candidate := filepath.Join(filepath.Dir(real), "..", "client")
			if fi, err := os.Stat(candidate); err == nil && fi.IsDir() {
				abs, _ := filepath.Abs(candidate)
				return abs
			}
		}
	}

	// Attempt 2: relative to cwd — matches `go run` from repo root.
	fallback := filepath.Join("examples", "collab-editor", "client")
	abs, err := filepath.Abs(fallback)
	if err != nil {
		return fallback
	}
	return abs
}
