package http_test

import (
	"bytes"
	"encoding/base64"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/reearth/ygo/crdt"
	yhttp "github.com/reearth/ygo/provider/http"
)

// helper: build a test server and return its handler plus a convenience
// function that posts a binary update to the given room.
func newTestServer() *yhttp.Server {
	return yhttp.NewServer()
}

// doGET performs a GET /doc/{room}?sv=<base64sv> against the handler.
// Pass an empty sv string to omit the sv query parameter.
func doGET(t *testing.T, h http.Handler, room, svBase64 string) *httptest.ResponseRecorder {
	t.Helper()
	target := "/doc/" + room
	if svBase64 != "" {
		target += "?sv=" + svBase64
	}
	req := httptest.NewRequest(http.MethodGet, target, nil)
	req.SetPathValue("room", room)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	return rr
}

// doPOST performs a POST /doc/{room} with the given binary body.
func doPOST(t *testing.T, h http.Handler, room string, body []byte) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/doc/"+room, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/octet-stream")
	req.SetPathValue("room", room)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	return rr
}

// encodeSVBase64 encodes a doc's state vector as base64 for the sv query param.
func encodeSVBase64(doc *crdt.Doc) string {
	svBytes := crdt.EncodeStateVectorV1(doc)
	return base64.StdEncoding.EncodeToString(svBytes)
}

// ── Unit tests ────────────────────────────────────────────────────────────────

func TestUnit_GET_UnknownRoom_ReturnsEmptyUpdate(t *testing.T) {
	srv := newTestServer()

	rr := doGET(t, srv, "nonexistent-room", "")
	assert.Equal(t, http.StatusOK, rr.Code)
	assert.Equal(t, "application/octet-stream", rr.Header().Get("Content-Type"))

	// The body should be a valid (possibly empty) V1 update — apply it to a fresh doc without error.
	body, err := io.ReadAll(rr.Body)
	require.NoError(t, err)

	doc := crdt.New()
	require.NoError(t, crdt.ApplyUpdateV1(doc, body, nil))
}

func TestUnit_GET_NoSV_ReturnsFullState(t *testing.T) {
	srv := newTestServer()

	// Create a doc, insert some content and POST it.
	srcDoc := crdt.New(crdt.WithClientID(1))
	txt := srcDoc.GetText("content")
	srcDoc.Transact(func(txn *crdt.Transaction) {
		txt.Insert(txn, 0, "hello world", nil)
	})
	update := crdt.EncodeStateAsUpdateV1(srcDoc, nil)

	rr := doPOST(t, srv, "room1", update)
	require.Equal(t, http.StatusNoContent, rr.Code)

	// GET with no sv — should return the full state.
	rr = doGET(t, srv, "room1", "")
	require.Equal(t, http.StatusOK, rr.Code)

	body, err := io.ReadAll(rr.Body)
	require.NoError(t, err)

	dstDoc := crdt.New()
	require.NoError(t, crdt.ApplyUpdateV1(dstDoc, body, nil))
	assert.Equal(t, "hello world", dstDoc.GetText("content").ToString())
}

func TestUnit_GET_WithSV_ReturnsDiff(t *testing.T) {
	srv := newTestServer()

	// POST initial update (step 1).
	docA := crdt.New(crdt.WithClientID(1))
	txt := docA.GetText("t")
	docA.Transact(func(txn *crdt.Transaction) {
		txt.Insert(txn, 0, "first", nil)
	})
	update1 := crdt.EncodeStateAsUpdateV1(docA, nil)
	rr := doPOST(t, srv, "roomX", update1)
	require.Equal(t, http.StatusNoContent, rr.Code)

	// Capture state vector after step 1.
	svBase64 := encodeSVBase64(docA)

	// POST second update (step 2).
	docA.Transact(func(txn *crdt.Transaction) {
		txt.Insert(txn, 5, " second", nil)
	})
	update2 := crdt.EncodeStateAsUpdateV1(docA, nil) // full state
	// We need to send only the incremental update; re-encode from sv.
	svAfterFirst, err := crdt.DecodeStateVectorV1(func() []byte {
		b, _ := base64.StdEncoding.DecodeString(svBase64)
		return b
	}())
	require.NoError(t, err)
	incUpdate := crdt.EncodeStateAsUpdateV1(docA, svAfterFirst)
	_ = update2

	rr = doPOST(t, srv, "roomX", incUpdate)
	require.Equal(t, http.StatusNoContent, rr.Code)

	// GET with the first sv — should return only the second update's content.
	rr = doGET(t, srv, "roomX", svBase64)
	require.Equal(t, http.StatusOK, rr.Code)

	diff, err := io.ReadAll(rr.Body)
	require.NoError(t, err)

	// Apply diff to a doc that already has step 1 content.
	docB := crdt.New()
	require.NoError(t, crdt.ApplyUpdateV1(docB, update1, nil))
	require.NoError(t, crdt.ApplyUpdateV1(docB, diff, nil))
	assert.Equal(t, "first second", docB.GetText("t").ToString())
}

func TestUnit_POST_AppliesUpdate(t *testing.T) {
	srv := newTestServer()

	doc := crdt.New(crdt.WithClientID(42))
	arr := doc.GetArray("items")
	doc.Transact(func(txn *crdt.Transaction) {
		arr.Push(txn, []any{"a", "b", "c"})
	})
	update := crdt.EncodeStateAsUpdateV1(doc, nil)

	rr := doPOST(t, srv, "room-post", update)
	assert.Equal(t, http.StatusNoContent, rr.Code)

	// Verify via GET.
	rr = doGET(t, srv, "room-post", "")
	require.Equal(t, http.StatusOK, rr.Code)

	body, err := io.ReadAll(rr.Body)
	require.NoError(t, err)

	dstDoc := crdt.New()
	require.NoError(t, crdt.ApplyUpdateV1(dstDoc, body, nil))
	assert.Equal(t, []any{"a", "b", "c"}, dstDoc.GetArray("items").ToSlice())
}

func TestUnit_POST_InvalidBody_Returns400(t *testing.T) {
	srv := newTestServer()

	garbage := []byte{0xFF, 0xFE, 0x00, 0x01, 0x02, 0x03}
	rr := doPOST(t, srv, "room-bad", garbage)
	assert.Equal(t, http.StatusBadRequest, rr.Code)
}

func TestUnit_UnknownMethod_Returns405(t *testing.T) {
	srv := newTestServer()

	req := httptest.NewRequest(http.MethodPut, "/doc/some-room", nil)
	req.SetPathValue("room", "some-room")
	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)

	assert.Equal(t, http.StatusMethodNotAllowed, rr.Code)
}

// ── Integration tests ─────────────────────────────────────────────────────────

func TestInteg_TwoPeer_HTTPSync(t *testing.T) {
	srv := yhttp.NewServer()

	// Step 1: Peer A creates a doc locally, inserts "hello".
	peerA := crdt.New(crdt.WithClientID(100))
	txtA := peerA.GetText("doc")
	peerA.Transact(func(txn *crdt.Transaction) {
		txtA.Insert(txn, 0, "hello", nil)
	})

	// Step 2: POST A's full update to the server.
	updateA := crdt.EncodeStateAsUpdateV1(peerA, nil)
	rr := doPOST(t, srv, "shared", updateA)
	require.Equal(t, http.StatusNoContent, rr.Code)

	// Step 3: GET from the server with an empty state vector.
	emptyDoc := crdt.New()
	svBase64 := encodeSVBase64(emptyDoc)
	rr = doGET(t, srv, "shared", svBase64)
	require.Equal(t, http.StatusOK, rr.Code)

	serverUpdate, err := io.ReadAll(rr.Body)
	require.NoError(t, err)

	// Step 4: Apply the response to Peer B.
	peerB := crdt.New(crdt.WithClientID(200))
	require.NoError(t, crdt.ApplyUpdateV1(peerB, serverUpdate, nil))

	// Step 5: Assert B has "hello".
	assert.Equal(t, "hello", peerB.GetText("doc").ToString())
}

func TestInteg_IncrementalSync(t *testing.T) {
	srv := yhttp.NewServer()

	// Step 1: POST initial state.
	docSrc := crdt.New(crdt.WithClientID(1))
	txt := docSrc.GetText("t")
	docSrc.Transact(func(txn *crdt.Transaction) {
		txt.Insert(txn, 0, "initial", nil)
	})
	initialUpdate := crdt.EncodeStateAsUpdateV1(docSrc, nil)

	rr := doPOST(t, srv, "inc-room", initialUpdate)
	require.Equal(t, http.StatusNoContent, rr.Code)

	// Step 2: Capture server's state vector via GET (full state applied to a temp doc, then encode its SV).
	rr = doGET(t, srv, "inc-room", "")
	require.Equal(t, http.StatusOK, rr.Code)

	fullStateBytes, err := io.ReadAll(rr.Body)
	require.NoError(t, err)

	tempDoc := crdt.New()
	require.NoError(t, crdt.ApplyUpdateV1(tempDoc, fullStateBytes, nil))
	svBase64 := encodeSVBase64(tempDoc)

	// Step 3: POST an incremental update.
	docSrc.Transact(func(txn *crdt.Transaction) {
		txt.Insert(txn, 7, " incremental", nil)
	})
	svCaptured, err := crdt.DecodeStateVectorV1(func() []byte {
		b, _ := base64.StdEncoding.DecodeString(svBase64)
		return b
	}())
	require.NoError(t, err)
	incrUpdate := crdt.EncodeStateAsUpdateV1(docSrc, svCaptured)

	rr = doPOST(t, srv, "inc-room", incrUpdate)
	require.Equal(t, http.StatusNoContent, rr.Code)

	// Step 4: GET with the captured SV.
	rr = doGET(t, srv, "inc-room", svBase64)
	require.Equal(t, http.StatusOK, rr.Code)

	diffBytes, err := io.ReadAll(rr.Body)
	require.NoError(t, err)

	// Step 5: Verify diff only contains the incremental content.
	// Apply initial state to a doc, then apply the diff.
	verifyDoc := crdt.New()
	require.NoError(t, crdt.ApplyUpdateV1(verifyDoc, initialUpdate, nil))
	require.NoError(t, crdt.ApplyUpdateV1(verifyDoc, diffBytes, nil))
	assert.Equal(t, "initial incremental", verifyDoc.GetText("t").ToString())
}
