package http_test

import (
	"bytes"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
)

// #50 — AuthFunc returning false rejects both GET and POST with 401, before any
// document is read or mutated.
func TestUnit_AuthFunc_RejectsWith401(t *testing.T) {
	srv := newTestServer()
	srv.AuthFunc = func(r *http.Request) bool { return false }

	rr := doGET(t, srv, "room1", "")
	assert.Equal(t, http.StatusUnauthorized, rr.Code)

	rr = doPOST(t, srv, "room1", []byte{0x00, 0x00})
	assert.Equal(t, http.StatusUnauthorized, rr.Code)

	// Rejected requests must not have created the document.
	assert.Nil(t, srv.GetDoc("room1"))
}

// #50 — AuthFunc returning true lets the request through.
func TestUnit_AuthFunc_AllowsWhenTrue(t *testing.T) {
	srv := newTestServer()
	called := false
	srv.AuthFunc = func(r *http.Request) bool { called = true; return true }

	rr := doGET(t, srv, "room1", "")
	assert.Equal(t, http.StatusOK, rr.Code)
	assert.True(t, called, "AuthFunc should be invoked")
}

// #50 — crafted room names are rejected with 400, matching the WebSocket
// provider's rule (shared via internal/roomname).
func TestUnit_InvalidRoomName_Returns400(t *testing.T) {
	srv := newTestServer()
	for _, bad := range []string{".", ".."} {
		assert.Equal(t, http.StatusBadRequest, doGET(t, srv, bad, "").Code, "GET room=%q", bad)
		assert.Equal(t, http.StatusBadRequest, doPOST(t, srv, bad, []byte{0x00}).Code, "POST room=%q", bad)
	}
}

// #50 — a POST body larger than MaxUpdateBytes is rejected with 413 before the
// whole payload is buffered.
func TestUnit_OversizeBody_Returns413(t *testing.T) {
	srv := newTestServer()
	srv.MaxUpdateBytes = 8

	rr := doPOST(t, srv, "room1", bytes.Repeat([]byte{0x00}, 64))
	assert.Equal(t, http.StatusRequestEntityTooLarge, rr.Code)
}
