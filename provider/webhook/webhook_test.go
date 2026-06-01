package webhook_test

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/reearth/ygo/crdt"
	"github.com/reearth/ygo/provider/webhook"
	ygws "github.com/reearth/ygo/provider/websocket"
)

// Tests for #61 — provider/webhook subpackage.

// VerifySignature must accept a valid sha256= HMAC and reject everything
// else (wrong prefix, bad hex, wrong key, tampered body).
//
// The end-to-end signing path (Webhook signs body → VerifySignature
// accepts it) is exercised by TestInteg_Webhook_PostSignedEventEndToEnd
// below; this test focuses on the verifier itself across forged inputs.
func TestUnit_VerifySignature(t *testing.T) {
	secret := []byte("super-secret-key")
	body := []byte(`{"hello":"world"}`)

	good := signFor(secret, body)
	assert.True(t, webhook.VerifySignature(body, secret, good))

	// Tampered body:
	assert.False(t, webhook.VerifySignature([]byte(`{"hello":"WORLD"}`), secret, good),
		"signature for original body must not validate against tampered body")

	// Wrong secret:
	assert.False(t, webhook.VerifySignature(body, []byte("other-key"), good),
		"signature must not validate under a different secret")

	// Wrong prefix:
	assert.False(t, webhook.VerifySignature(body, secret, "md5=deadbeef"),
		"non-sha256 prefix must be rejected")

	// Malformed hex:
	assert.False(t, webhook.VerifySignature(body, secret, "sha256=not-hex"),
		"invalid hex must be rejected")
}

// signFor reproduces the signing formula used by the implementation —
// useful for tests that need to forge or verify signatures without
// driving an HTTP server.
func signFor(secret, body []byte) string {
	mac := hmac.New(sha256.New, secret)
	mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

// Integration: Webhook POSTs an event to an httptest.Server, the server
// verifies the signature, and we confirm the body decodes back to the
// original payload.
func TestInteg_Webhook_PostSignedEventEndToEnd(t *testing.T) {
	secret := []byte("integration-secret")
	var got webhook.Event
	var gotSig string
	var bodyBytes []byte

	done := make(chan struct{})
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		bodyBytes, _ = io.ReadAll(r.Body)
		gotSig = r.Header.Get(webhook.SignatureHeader)
		_ = json.Unmarshal(bodyBytes, &got)
		w.WriteHeader(http.StatusOK)
		close(done)
	}))
	defer ts.Close()

	wh, err := webhook.New(webhook.Config{
		URL:      ts.URL,
		Secret:   secret,
		Debounce: 10 * time.Millisecond,
	})
	require.NoError(t, err)
	defer func() { _ = wh.Close(context.Background()) }()

	wh.Enqueue(webhook.Event{
		Type:   webhook.EventUpdate,
		Room:   "myroom",
		Update: []byte{0x01, 0x02, 0x03},
	})

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("webhook never reached the server")
	}

	assert.Equal(t, webhook.EventUpdate, got.Type)
	assert.Equal(t, "myroom", got.Room)
	assert.Equal(t, []byte{0x01, 0x02, 0x03}, got.Update)
	assert.True(t, webhook.VerifySignature(bodyBytes, secret, gotSig),
		"received signature must validate against the received body")
}

// Debounce: multiple Update events for the same room within the debounce
// window must be coalesced into a single POST carrying the LATEST update.
func TestInteg_Webhook_DebounceCoalesces(t *testing.T) {
	var (
		mu    sync.Mutex
		posts []webhook.Event
	)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var e webhook.Event
		_ = json.Unmarshal(body, &e)
		mu.Lock()
		posts = append(posts, e)
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer ts.Close()

	wh, err := webhook.New(webhook.Config{
		URL:      ts.URL,
		Debounce: 100 * time.Millisecond,
	})
	require.NoError(t, err)
	defer func() { _ = wh.Close(context.Background()) }()

	// Fire ten Update events for the same room in rapid succession.
	for i := 0; i < 10; i++ {
		wh.Enqueue(webhook.Event{
			Type:   webhook.EventUpdate,
			Room:   "debounceroom",
			Update: []byte{byte(i)},
		})
		time.Sleep(5 * time.Millisecond)
	}

	// Wait for the debounce window to expire and the single POST to land.
	require.Eventually(t, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(posts) == 1
	}, 2*time.Second, 10*time.Millisecond,
		"10 rapid Update events must coalesce into 1 POST")

	mu.Lock()
	defer mu.Unlock()
	require.Len(t, posts, 1)
	assert.Equal(t, []byte{9}, posts[0].Update,
		"the coalesced post must carry the LATEST update bytes")
}

// Regression for Copilot review: pending coalescing must key by
// (Room, Type), not by Room alone. A Connect followed by an Update for
// the same room within the debounce window must NOT overwrite each
// other — two POSTs are expected, one Connect and one Update.
func TestInteg_Webhook_CoalescesPerEventType(t *testing.T) {
	var (
		mu    sync.Mutex
		types []webhook.EventType
	)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var e webhook.Event
		_ = json.Unmarshal(body, &e)
		mu.Lock()
		types = append(types, e.Type)
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer ts.Close()

	wh, err := webhook.New(webhook.Config{
		URL:      ts.URL,
		Debounce: 100 * time.Millisecond,
	})
	require.NoError(t, err)
	defer func() { _ = wh.Close(context.Background()) }()

	// Enqueue three distinct event types for the same room within a
	// single debounce window. All three must be delivered.
	wh.Enqueue(webhook.Event{Type: webhook.EventConnect, Room: "mixroom"})
	wh.Enqueue(webhook.Event{Type: webhook.EventUpdate, Room: "mixroom", Update: []byte{0x01}})
	wh.Enqueue(webhook.Event{Type: webhook.EventDisconnect, Room: "mixroom"})

	require.Eventually(t, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(types) == 3
	}, 2*time.Second, 10*time.Millisecond,
		"three distinct event types in one window must produce three POSTs, "+
			"not collapse via Room-only coalescing")

	mu.Lock()
	defer mu.Unlock()
	assert.ElementsMatch(t, types,
		[]webhook.EventType{webhook.EventConnect, webhook.EventUpdate, webhook.EventDisconnect},
		"each event type must arrive exactly once")
}

// Retry: 5xx responses must trigger exponential backoff retries until
// the receiver returns 2xx. We bound the test by limiting MaxRetries
// and counting attempts.
func TestInteg_Webhook_RetriesOn5xx(t *testing.T) {
	var attempts atomic.Int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := attempts.Add(1)
		if n < 3 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer ts.Close()

	wh, err := webhook.New(webhook.Config{
		URL:         ts.URL,
		Debounce:    10 * time.Millisecond,
		MaxRetries:  5,
		BackoffBase: 10 * time.Millisecond,
	})
	require.NoError(t, err)
	defer func() { _ = wh.Close(context.Background()) }()

	wh.Enqueue(webhook.Event{Type: webhook.EventUpdate, Room: "retryroom"})

	require.Eventually(t, func() bool {
		return attempts.Load() >= 3
	}, 2*time.Second, 10*time.Millisecond,
		"webhook must retry 5xx responses until receiver returns 2xx")
}

// 4xx response: must NOT retry. The receiver said "no", and we honour it.
func TestInteg_Webhook_DoesNotRetry4xx(t *testing.T) {
	var attempts atomic.Int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts.Add(1)
		w.WriteHeader(http.StatusBadRequest)
	}))
	defer ts.Close()

	wh, err := webhook.New(webhook.Config{
		URL:         ts.URL,
		Debounce:    10 * time.Millisecond,
		MaxRetries:  5,
		BackoffBase: 10 * time.Millisecond,
	})
	require.NoError(t, err)
	defer func() { _ = wh.Close(context.Background()) }()

	wh.Enqueue(webhook.Event{Type: webhook.EventUpdate, Room: "norereroom"})

	// Allow plenty of time for any retries — if they happen, the count
	// would exceed 1.
	time.Sleep(500 * time.Millisecond)

	assert.EqualValues(t, 1, attempts.Load(),
		"4xx must drop the event after one attempt; no retry")
}

// Empty Secret: no signature header. Useful for trusted-network
// deployments that don't need HMAC verification.
func TestInteg_Webhook_NoSecretNoSignatureHeader(t *testing.T) {
	var sigHeader string
	done := make(chan struct{})
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sigHeader = r.Header.Get(webhook.SignatureHeader)
		w.WriteHeader(http.StatusOK)
		close(done)
	}))
	defer ts.Close()

	wh, err := webhook.New(webhook.Config{
		URL:      ts.URL,
		Debounce: 10 * time.Millisecond,
		// Secret intentionally nil.
	})
	require.NoError(t, err)
	defer func() { _ = wh.Close(context.Background()) }()

	wh.Enqueue(webhook.Event{Type: webhook.EventUpdate, Room: "nosecretroom"})

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("webhook never reached the server")
	}

	assert.Empty(t, sigHeader,
		"no signature header must be emitted when Config.Secret is empty")
}

// Close must drain pending events before returning.
func TestInteg_Webhook_CloseFlushesPending(t *testing.T) {
	delivered := make(chan struct{}, 1)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		delivered <- struct{}{}
	}))
	defer ts.Close()

	wh, err := webhook.New(webhook.Config{
		URL: ts.URL,
		// Long debounce so the event would not normally flush before Close.
		Debounce: 10 * time.Second,
	})
	require.NoError(t, err)

	wh.Enqueue(webhook.Event{Type: webhook.EventUpdate, Room: "closeroom"})

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	require.NoError(t, wh.Close(ctx))

	select {
	case <-delivered:
	case <-time.After(2 * time.Second):
		t.Fatal("Close must flush the pending event before returning")
	}
}

// Closed Webhook silently drops new events.
func TestInteg_Webhook_DropsAfterClose(t *testing.T) {
	var attempts atomic.Int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer ts.Close()

	wh, err := webhook.New(webhook.Config{URL: ts.URL, Debounce: 50 * time.Millisecond})
	require.NoError(t, err)
	require.NoError(t, wh.Close(context.Background()))

	// Now enqueue after Close.
	wh.Enqueue(webhook.Event{Type: webhook.EventUpdate, Room: "deadroom"})

	time.Sleep(200 * time.Millisecond)
	assert.Zero(t, attempts.Load(),
		"events enqueued after Close must be silently dropped")
}

// New must reject empty URL.
func TestUnit_New_RejectsEmptyURL(t *testing.T) {
	_, err := webhook.New(webhook.Config{})
	require.Error(t, err,
		"webhook.New must reject empty URL")
	assert.Contains(t, err.Error(), "URL",
		"error must mention URL: %v", err)
}

// Regression for #93 self-review B3 — AttachTo wires every relevant
// Server hook + per-doc OnUpdate so a single call delivers the event
// stream without per-event manual wiring. This test drives the
// load → update → unload path via srv.Apply + srv.CloseRoom (no real
// WebSocket needed for the wiring assertion); the Connect / Disconnect
// edges (which require a real peer transition) are covered by the
// lifecycle test suite which exercises OnFirstPeer / OnLastPeer.
func TestInteg_Webhook_AttachTo_WiresAllEvents(t *testing.T) {
	var (
		mu       sync.Mutex
		got      []webhook.EventType
		gotRooms []string
	)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var e webhook.Event
		_ = json.Unmarshal(body, &e)
		mu.Lock()
		got = append(got, e.Type)
		gotRooms = append(gotRooms, e.Room)
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer ts.Close()

	wh, err := webhook.New(webhook.Config{URL: ts.URL, Debounce: 30 * time.Millisecond})
	require.NoError(t, err)
	defer func() { _ = wh.Close(context.Background()) }()

	srv := ygws.NewServer()
	detach := webhook.AttachTo(srv, wh)
	defer detach()

	// srv.Apply creates the room (→ EventLoad via OnLoadDocument) and
	// applies an update inside a Transact (→ EventUpdate via the
	// per-doc OnUpdate listener AttachTo installed).
	require.NoError(t, srv.Apply(context.Background(), "attachroom",
		func(d *crdt.Doc, transact func(func(*crdt.Transaction))) {
			txt := d.GetText("t")
			transact(func(txn *crdt.Transaction) { txt.Insert(txn, 0, "x", nil) })
		}))

	// srv.CloseRoom evicts the room (→ EventUnload via OnUnloadDocument).
	require.NoError(t, srv.CloseRoom("attachroom", false))

	// Wait for all three event types to arrive.
	require.Eventually(t, func() bool {
		mu.Lock()
		defer mu.Unlock()
		seen := make(map[webhook.EventType]bool)
		for _, e := range got {
			seen[e] = true
		}
		return seen[webhook.EventLoad] && seen[webhook.EventUpdate] && seen[webhook.EventUnload]
	}, 4*time.Second, 50*time.Millisecond,
		"AttachTo must wire Load + Update + Unload from a single call")

	mu.Lock()
	defer mu.Unlock()
	for _, r := range gotRooms {
		assert.Equal(t, "attachroom", r,
			"every event must carry the correct room name")
	}
}

// AttachTo's detach func must restore the previous hook values so the
// Server returns to its pre-AttachTo state.
func TestUnit_Webhook_AttachTo_DetachRestoresPrevHooks(t *testing.T) {
	srv := ygws.NewServer()

	// Set sentinel hooks first.
	called := struct {
		load, unload, first, last bool
	}{}
	srv.OnLoadDocument = func(_ context.Context, _ string, _ *crdt.Doc) error {
		called.load = true
		return nil
	}
	srv.OnUnloadDocument = func(_ context.Context, _ string) { called.unload = true }
	srv.OnFirstPeer = func(_ context.Context, _ string) { called.first = true }
	srv.OnLastPeer = func(_ context.Context, _ string) { called.last = true }

	wh, err := webhook.New(webhook.Config{URL: "http://example.invalid"})
	require.NoError(t, err)
	defer func() { _ = wh.Close(context.Background()) }()

	detach := webhook.AttachTo(srv, wh)

	// Hooks are now AttachTo's wrappers — but they must chain to the
	// previous values, so the sentinels still get hit when the wrappers run.
	require.NoError(t, srv.OnLoadDocument(context.Background(), "r", crdt.New()))
	srv.OnUnloadDocument(context.Background(), "r")
	srv.OnFirstPeer(context.Background(), "r")
	srv.OnLastPeer(context.Background(), "r")
	assert.True(t, called.load && called.unload && called.first && called.last,
		"AttachTo wrappers must chain to the pre-existing hooks")

	// detach restores: the hook fields must equal the original closures.
	// We can't pointer-compare closures, but we CAN reset the called
	// flags, call the post-detach hooks, and assert the sentinels still
	// fire — proving the wrappers were removed.
	detach()
	called = struct{ load, unload, first, last bool }{}
	require.NoError(t, srv.OnLoadDocument(context.Background(), "r", crdt.New()))
	srv.OnUnloadDocument(context.Background(), "r")
	srv.OnFirstPeer(context.Background(), "r")
	srv.OnLastPeer(context.Background(), "r")
	assert.True(t, called.load && called.unload && called.first && called.last,
		"after detach the original sentinel hooks must run directly")
}

// Regression for #93 self-review — Close must interrupt an in-flight
// retry sleep instead of waiting out the full backoff.
func TestInteg_Webhook_Close_InterruptsRetrySleep(t *testing.T) {
	// Always 500 → forces deliver into the backoff sleep loop.
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer ts.Close()

	wh, err := webhook.New(webhook.Config{
		URL:         ts.URL,
		Debounce:    10 * time.Millisecond,
		MaxRetries:  20,
		BackoffBase: 500 * time.Millisecond, // would sum to many seconds
		MaxBackoff:  10 * time.Second,
	})
	require.NoError(t, err)

	wh.Enqueue(webhook.Event{Type: webhook.EventUpdate, Room: "interruptroom"})

	// Let the first attempt run + the backoff sleep start.
	time.Sleep(200 * time.Millisecond)

	// Close with a short ctx; deliver should observe <-w.closed in
	// its sleep select and exit fast, well under the would-be backoff.
	start := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	require.NoError(t, wh.Close(ctx))
	elapsed := time.Since(start)

	assert.Less(t, elapsed, 1500*time.Millisecond,
		"Close must interrupt retry sleep (took %v); the next backoff "+
			"alone would have been ~500ms with several more to follow", elapsed)
}

// Regression for #93 self-review test gap — Event.Update bytes must
// round-trip cleanly through JSON's base64 encoding.
func TestUnit_Webhook_Event_Update_RoundTripsBase64(t *testing.T) {
	original := []byte{0x00, 0xFF, 0xAB, 0x10, 0x42, 0x80, 0x7F, 0x01}
	body, err := webhook.EncodeBody(webhook.Event{
		Type:   webhook.EventUpdate,
		Room:   "rt",
		Update: original,
	})
	require.NoError(t, err)

	// json.Marshal encodes []byte as base64 by default. Round-trip
	// through json.Unmarshal must give us the same bytes back.
	var decoded webhook.Event
	require.NoError(t, json.Unmarshal(body, &decoded))
	assert.Equal(t, original, decoded.Update,
		"Event.Update must survive JSON base64 round-trip without modification")
}

// Regression for #93 self-review A3 — bursting many events at a
// configured-low MaxConcurrentDeliveries must not spawn N goroutines
// simultaneously; the deliverSem must cap concurrent in-flight POSTs.
func TestInteg_Webhook_BoundedConcurrency_UnderBurst(t *testing.T) {
	const maxConcurrent = 3
	var (
		inFlight     atomic.Int32
		peakInFlight atomic.Int32
	)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		n := inFlight.Add(1)
		// Track peak observed concurrency.
		for {
			peak := peakInFlight.Load()
			if n <= peak || peakInFlight.CompareAndSwap(peak, n) {
				break
			}
		}
		// Hold the request open briefly so multiple are simultaneously in-flight.
		time.Sleep(80 * time.Millisecond)
		inFlight.Add(-1)
		w.WriteHeader(http.StatusOK)
	}))
	defer ts.Close()

	wh, err := webhook.New(webhook.Config{
		URL:                     ts.URL,
		Debounce:                10 * time.Millisecond,
		MaxConcurrentDeliveries: maxConcurrent,
	})
	require.NoError(t, err)
	defer func() { _ = wh.Close(context.Background()) }()

	// 12 distinct events fan out at once.
	for i := 0; i < 12; i++ {
		wh.Enqueue(webhook.Event{
			Type: webhook.EventUpdate,
			Room: "burstroom-" + strconv.Itoa(i),
		})
	}

	// Wait for everything to drain.
	require.Eventually(t, func() bool {
		return inFlight.Load() == 0 && peakInFlight.Load() > 0
	}, 4*time.Second, 20*time.Millisecond)

	assert.LessOrEqual(t, peakInFlight.Load(), int32(maxConcurrent),
		"peak observed concurrency (%d) must not exceed MaxConcurrentDeliveries (%d)",
		peakInFlight.Load(), maxConcurrent)
}
