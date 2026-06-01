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
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/reearth/ygo/provider/webhook"
)

// Tests for #61 — provider/webhook subpackage.

// VerifySignature must accept a valid sha256= HMAC and reject everything
// else (wrong prefix, bad hex, wrong key, tampered body).
func TestUnit_VerifySignature(t *testing.T) {
	secret := []byte("super-secret-key")
	body := []byte(`{"hello":"world"}`)

	// Generate the canonical signature via EncodeBody-equivalent path:
	// we know the implementation uses crypto/hmac+sha256, so use the same.
	// The point of the test is to verify that VerifySignature catches both
	// happy and tampered cases.
	wh, err := webhook.New(webhook.Config{URL: "http://example.invalid", Secret: secret})
	require.NoError(t, err)
	defer func() { _ = wh.Close(context.Background()) }()

	// Build a known signature by re-using the public surface: encode an
	// event, sign manually (the test below proves end-to-end correctness).
	// For this unit test we construct the signature directly.
	// Use Enqueue → captured by an httptest server below in the integration
	// test. Here we just test the verifier symmetry:
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
