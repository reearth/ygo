package websocket_test

import (
	"fmt"
	"net/http"

	ygws "github.com/reearth/ygo/provider/websocket"
)

// ExampleServer demonstrates the minimal real-time room server.
// Each unique URL path is a separate Yjs document; peers connecting
// to the same path collaborate.
func ExampleServer() {
	srv := ygws.NewServer()
	mux := http.NewServeMux()
	mux.Handle("/rooms/", http.StripPrefix("/rooms", srv))
	// http.ListenAndServe(":8080", mux) — start in production

	_ = mux
	fmt.Println("server ready")
	// Output: server ready
}

// ExampleServer_authorize demonstrates read-only connections (#59). Authorize
// both accepts/rejects the connection and reports whether it is read-only. A
// read-only peer receives document and awareness broadcasts but its inbound
// writes are dropped server-side. When set, Authorize takes precedence over
// AuthFunc.
func ExampleServer_authorize() {
	srv := ygws.NewServer()
	srv.Authorize = func(r *http.Request) (ygws.ConnectionConfig, bool) {
		token := r.URL.Query().Get("token")
		if token == "" {
			return ygws.ConnectionConfig{}, false // reject: 401
		}
		// A viewer token grants observe-only access; an editor token can write.
		return ygws.ConnectionConfig{ReadOnly: token == "viewer"}, true
	}

	_ = srv
	fmt.Println("configured")
	// Output: configured
}
