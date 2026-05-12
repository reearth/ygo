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
