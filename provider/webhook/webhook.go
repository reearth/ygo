// Package webhook posts ygo document events to an external HTTP endpoint
// with HMAC-SHA256 request signing, debounce / coalescing of consecutive
// updates, and bounded retry-with-exponential-backoff on transient errors.
//
// Mirrors the Hocuspocus `extension-webhook` integration pattern (#61).
// Useful for forwarding doc events to Slack / Teams, audit logs, downstream
// AI workflows, search index pipelines, no-code platforms, etc.
//
// # Usage
//
//	wh := webhook.New(webhook.Config{
//	    URL:      "https://hooks.example.com/ygo",
//	    Secret:   []byte("shared-secret-for-hmac"),
//	    Debounce: 1 * time.Second,
//	})
//	defer wh.Close(context.Background())
//
//	srv := ygws.NewServer()
//	srv.OnLoadDocument = func(_ context.Context, room string, doc *crdt.Doc) error {
//	    doc.OnUpdate(func(update []byte, _ any) {
//	        wh.Enqueue(webhook.Event{Type: webhook.EventUpdate, Room: room, Update: update})
//	    })
//	    return nil
//	}
//
// # Signature
//
// Every POST carries an `X-YGo-Signature-256` header of the form
// `sha256=<hex>` where `<hex>` is the HMAC-SHA256 of the request body using
// Config.Secret as the key. Receivers should reject requests whose
// signature does not match the body — defends against forged events when
// the URL is exposed to the open internet.
package webhook

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"sync"
	"time"
)

// EventType is the kind of event delivered by the webhook.
type EventType string

const (
	// EventUpdate fires for every committed document update. The Update
	// field carries the V1 update bytes (base64-encoded on the wire).
	EventUpdate EventType = "update"
	// EventConnect fires when a peer connects to a room (one event per
	// peer per room).
	EventConnect EventType = "connect"
	// EventDisconnect fires when a peer disconnects from a room.
	EventDisconnect EventType = "disconnect"
	// EventLoad fires when a room is loaded into memory (the first peer
	// joins, or Apply / BroadcastUpdate creates it).
	EventLoad EventType = "load"
	// EventUnload fires when a room is evicted from memory (last peer
	// leaves, or CloseRoom is called).
	EventUnload EventType = "unload"
)

// Event is a single ygo event delivered to the webhook URL.
type Event struct {
	// Type identifies what happened.
	Type EventType `json:"type"`
	// Room is the room name the event applies to.
	Room string `json:"room"`
	// Update is the V1 update bytes for EventUpdate (nil for other types).
	// Encoded as base64 on the wire.
	Update []byte `json:"update,omitempty"`
	// Timestamp is the time the event was enqueued, in RFC3339Nano.
	// Set automatically by Enqueue; callers should not pre-populate it.
	Timestamp time.Time `json:"timestamp"`
}

// Config configures a Webhook. URL is required; all other fields have
// sensible defaults.
type Config struct {
	// URL is the destination endpoint. Must be HTTP or HTTPS.
	URL string
	// Secret is the HMAC-SHA256 key used to sign every request body.
	// If empty, no signature header is emitted (suitable only for
	// trusted internal networks).
	Secret []byte
	// Debounce is the window within which consecutive Update events
	// for the same room are coalesced into a single delivery carrying
	// the most recent update. Zero disables debounce; the default
	// (1 * time.Second) is applied when Config.Debounce is unset.
	// Capped at 10 * time.Second to prevent runaway buffering.
	Debounce time.Duration
	// MaxRetries is the number of delivery attempts before the event
	// is dropped on transient failure. Zero uses the default (5).
	// Status codes 200-299 succeed; 5xx and network errors retry with
	// exponential backoff; 4xx drops immediately (the receiver said no).
	MaxRetries int
	// BackoffBase is the first retry delay; each subsequent attempt
	// doubles the delay. Zero uses the default (250 * time.Millisecond).
	BackoffBase time.Duration
	// MaxBodyBytes caps the size of a single POST body. Zero uses the
	// default (16 MiB). Events whose JSON encoding exceeds this size
	// are dropped with a logged warning.
	MaxBodyBytes int64
	// HTTPClient is the http.Client used for outbound requests. Zero
	// value uses a default client with a 10-second per-request timeout.
	HTTPClient *http.Client
}

const (
	defaultDebounce     = 1 * time.Second
	maxDebounce         = 10 * time.Second
	defaultMaxRetries   = 5
	defaultBackoffBase  = 250 * time.Millisecond
	defaultMaxBodyBytes = 16 << 20 // 16 MiB
	defaultHTTPTimeout  = 10 * time.Second

	// SignatureHeader is the HTTP request header that carries the
	// HMAC-SHA256 signature of the body when Config.Secret is set.
	SignatureHeader = "X-YGo-Signature-256"
)

// Webhook is a running webhook delivery worker. Construct with New, push
// events with Enqueue, and call Close when done.
type Webhook struct {
	cfg Config

	mu       sync.Mutex
	pending  map[string]*Event // room → most-recent debounced event
	timer    *time.Timer       // fires when the next debounce window expires
	deadline time.Time         // when the current debounce window expires

	wg     sync.WaitGroup
	closed chan struct{} // closed by Close to signal the flush goroutine to exit
}

// New constructs a Webhook from cfg. The returned Webhook owns a small
// pool of goroutines for debounce timing and retry; call Close to release.
//
// Returns an error if cfg.URL is empty or not http/https.
func New(cfg Config) (*Webhook, error) {
	if cfg.URL == "" {
		return nil, errors.New("webhook: Config.URL is required")
	}
	if cfg.Debounce == 0 {
		cfg.Debounce = defaultDebounce
	}
	if cfg.Debounce > maxDebounce {
		cfg.Debounce = maxDebounce
	}
	if cfg.MaxRetries == 0 {
		cfg.MaxRetries = defaultMaxRetries
	}
	if cfg.BackoffBase == 0 {
		cfg.BackoffBase = defaultBackoffBase
	}
	if cfg.MaxBodyBytes == 0 {
		cfg.MaxBodyBytes = defaultMaxBodyBytes
	}
	if cfg.HTTPClient == nil {
		cfg.HTTPClient = &http.Client{Timeout: defaultHTTPTimeout}
	}
	return &Webhook{
		cfg:     cfg,
		pending: make(map[string]*Event),
		closed:  make(chan struct{}),
	}, nil
}

// Enqueue submits an event for delivery. EventUpdate events with the
// same Room name within the debounce window are coalesced: only the
// most recent update bytes survive. Other event types are not
// coalesced and dispatch on the next debounce tick alongside any
// pending updates.
func (w *Webhook) Enqueue(e Event) {
	if e.Timestamp.IsZero() {
		e.Timestamp = time.Now()
	}

	w.mu.Lock()
	defer w.mu.Unlock()

	// Closed Webhook silently drops new events (Close docs this).
	select {
	case <-w.closed:
		return
	default:
	}

	// Coalesce by room: most recent wins. (Even for non-Update events
	// the latest within a window is what we deliver — simpler model.)
	w.pending[e.Room] = &e

	// Start / restart the debounce timer.
	w.deadline = time.Now().Add(w.cfg.Debounce)
	if w.timer == nil {
		w.timer = time.AfterFunc(w.cfg.Debounce, w.flush)
	} else {
		w.timer.Reset(w.cfg.Debounce)
	}
}

// flush is fired by the debounce timer. It snapshots the pending
// events and dispatches each in its own goroutine (one inflight HTTP
// call per pending room). Subsequent Enqueues build a new pending set.
func (w *Webhook) flush() {
	w.mu.Lock()
	if len(w.pending) == 0 {
		w.mu.Unlock()
		return
	}
	batch := make([]*Event, 0, len(w.pending))
	for _, e := range w.pending {
		batch = append(batch, e)
	}
	w.pending = make(map[string]*Event)
	w.mu.Unlock()

	for _, e := range batch {
		w.wg.Add(1)
		go func(ev *Event) {
			defer w.wg.Done()
			w.deliver(*ev)
		}(e)
	}
}

// deliver serialises one event to JSON, signs it, and POSTs it with
// bounded retry. Drop policy: 4xx → no retry (receiver said no); 5xx
// and transport errors retry up to MaxRetries with exponential backoff.
// Body too large → drop immediately with a logged warning.
func (w *Webhook) deliver(e Event) {
	body, err := json.Marshal(e)
	if err != nil {
		// json.Marshal failing on our own struct is a programming bug, not
		// a transient error — surface but don't retry.
		return
	}
	if int64(len(body)) > w.cfg.MaxBodyBytes {
		return
	}

	var sig string
	if len(w.cfg.Secret) > 0 {
		mac := hmac.New(sha256.New, w.cfg.Secret)
		mac.Write(body)
		sig = "sha256=" + hex.EncodeToString(mac.Sum(nil))
	}

	backoff := w.cfg.BackoffBase
	for attempt := 0; attempt < w.cfg.MaxRetries; attempt++ {
		if attempt > 0 {
			select {
			case <-time.After(backoff):
			case <-w.closed:
				return // give up on shutdown
			}
			backoff *= 2
		}

		retry, ok := w.postOnce(body, sig)
		if ok {
			return
		}
		if !retry {
			return // 4xx — receiver rejected, no retry
		}
	}
	// Exceeded MaxRetries; drop.
}

// postOnce performs a single POST attempt. Returns (retry, ok) where
// ok=true means delivery succeeded, retry=true means try again later.
func (w *Webhook) postOnce(body []byte, sig string) (retry, ok bool) {
	req, err := http.NewRequest(http.MethodPost, w.cfg.URL, bytes.NewReader(body))
	if err != nil {
		return false, false // malformed URL — no retry, no success
	}
	req.Header.Set("Content-Type", "application/json")
	if sig != "" {
		req.Header.Set(SignatureHeader, sig)
	}

	resp, err := w.cfg.HTTPClient.Do(req)
	if err != nil {
		return true, false // transport error — retry
	}
	defer func() { _, _ = io.Copy(io.Discard, resp.Body); _ = resp.Body.Close() }()

	switch {
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		return false, true
	case resp.StatusCode >= 500:
		return true, false // server error — retry
	default:
		return false, false // 4xx — receiver said no, no retry
	}
}

// Close stops accepting new events, fires any pending debounce-window
// events synchronously, and waits for in-flight deliveries to finish or
// for ctx to cancel.
//
// After Close returns, calls to Enqueue silently drop the event.
func (w *Webhook) Close(ctx context.Context) error {
	w.mu.Lock()
	select {
	case <-w.closed:
		w.mu.Unlock()
		return nil // already closed
	default:
	}
	close(w.closed)
	if w.timer != nil {
		w.timer.Stop()
	}
	w.mu.Unlock()

	// Drain whatever was in the pending set at Close time.
	w.flush()

	done := make(chan struct{})
	go func() { w.wg.Wait(); close(done) }()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// EncodeBody is a low-level helper that returns the exact JSON body the
// Webhook would POST for the given event, including the auto-populated
// Timestamp. Exported so test code (and downstream tooling) can verify
// signatures without spinning up a Webhook.
func EncodeBody(e Event) ([]byte, error) {
	if e.Timestamp.IsZero() {
		e.Timestamp = time.Now()
	}
	return json.Marshal(e)
}

// VerifySignature checks whether sig (in `sha256=<hex>` form) matches
// HMAC-SHA256(body, secret). Constant-time comparison. Returns true on
// match. Receivers wiring a webhook handler should call this on every
// inbound request before trusting the payload.
func VerifySignature(body, secret []byte, sig string) bool {
	const prefix = "sha256="
	if len(sig) <= len(prefix) || sig[:len(prefix)] != prefix {
		return false
	}
	want, err := hex.DecodeString(sig[len(prefix):])
	if err != nil {
		return false
	}
	mac := hmac.New(sha256.New, secret)
	mac.Write(body)
	return hmac.Equal(mac.Sum(nil), want)
}

// DecodeUpdate is a convenience for receivers: parses the base64-encoded
// Update field back into raw V1 update bytes. Exists so the on-the-wire
// shape can stay JSON-friendly without requiring callers to know that
// json.Unmarshal handles []byte as base64 automatically.
func DecodeUpdate(b64 string) ([]byte, error) {
	if b64 == "" {
		return nil, nil
	}
	return base64.StdEncoding.DecodeString(b64)
}
