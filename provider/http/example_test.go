package http_test

import (
	"fmt"
	"net/http"

	yghttp "github.com/reearth/ygo/provider/http"
)

// ExampleServer demonstrates the HTTP-based sync handler. Useful for
// clients that prefer request/response over a long-lived WebSocket.
func ExampleServer() {
	srv := yghttp.NewServer()
	mux := http.NewServeMux()
	mux.Handle("/sync/", http.StripPrefix("/sync", srv))

	_ = mux
	fmt.Println("server ready")
	// Output: server ready
}
