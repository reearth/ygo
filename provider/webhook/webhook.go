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
// The easiest path is AttachTo, which wires every relevant Server hook
// (OnLoadDocument, OnUnloadDocument, OnFirstPeer, OnLastPeer, and the
// per-doc OnUpdate stream) onto the Webhook in one call:
//
//	wh, _ := webhook.New(webhook.Config{URL: "https://hooks.example.com/ygo"})
//	defer wh.Close(context.Background())
//
//	srv := ygws.NewServer()
//	detach := webhook.AttachTo(srv, wh)
//	defer detach()
//
// For finer control, drive the Webhook directly via Enqueue.
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
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
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
	// Debounce is the window within which consecutive events with the
	// same (Room, Type) are coalesced into a single delivery carrying
	// the most-recent event. Zero applies the default (1 * time.Second);
	// values above 10 * time.Second are capped at the maximum to prevent
	// runaway buffering. There is no way to disable debounce — Enqueue
	// is always at least minimally batched.
	Debounce time.Duration
	// MaxRetries is the number of delivery attempts before the event
	// is dropped on transient failure. Zero uses the default (5).
	// Status codes 200-299 succeed; 5xx and network errors retry with
	// exponential backoff; 4xx drops immediately (the receiver said no).
	MaxRetries int
	// BackoffBase is the first retry delay; each subsequent attempt
	// doubles the delay. Zero uses the default (250 * time.Millisecond).
	BackoffBase time.Duration
	// MaxBackoff caps the per-attempt sleep, defending against very high
	// MaxRetries producing minute-scale waits. Zero uses the default
	// (30 * time.Second). Up to ±20% jitter is added on top of the
	// computed delay to break thundering-herd retry alignment when
	// many concurrent webhooks fail against the same receiver.
	MaxBackoff time.Duration
	// MaxBodyBytes caps the size of a single POST body. Zero uses the
	// default (16 MiB). Events whose JSON encoding exceeds this size
	// are dropped with a logged warning.
	MaxBodyBytes int64
	// MaxConcurrentDeliveries bounds the number of in-flight POSTs at
	// any one time. Zero uses the default (8). A burst of pending
	// events past this cap blocks the dispatch loop until in-flight
	// deliveries finish — preventing unbounded goroutine creation when
	// a slow / dead receiver collects a long queue.
	MaxConcurrentDeliveries int
	// HTTPClient is the http.Client used for outbound requests. Zero
	// value uses a default client with a 10-second per-request timeout.
	HTTPClient *http.Client
	// Logger receives structured log entries when events are dropped
	// for size or retry exhaustion. nil falls back to slog.Default().
	Logger *slog.Logger
}

const (
	defaultDebounce           = 1 * time.Second
	maxDebounce               = 10 * time.Second
	defaultMaxRetries         = 5
	defaultBackoffBase        = 250 * time.Millisecond
	defaultMaxBackoff         = 30 * time.Second
	defaultMaxBodyBytes       = 16 << 20 // 16 MiB
	defaultMaxConcurrent      = 8
	defaultHTTPTimeout        = 10 * time.Second
	jitterFractionDenominator = 5 // ±20% of the computed backoff

	// SignatureHeader is the HTTP request header that carries the
	// HMAC-SHA256 signature of the body when Config.Secret is set.
	SignatureHeader = "X-YGo-Signature-256"
)

// pendingKey identifies a coalescing bucket. Two events in the same
// window collapse only when they target the same room AND have the same
// EventType — without the type discriminator a Connect followed by an
// Update for the same room would lose the Update (or vice-versa).
type pendingKey struct {
	room      string
	eventType EventType
}

// Webhook is a running webhook delivery worker. Construct with New, push
// events with Enqueue, and call Close when done.
//
// Internally a single dispatcher goroutine owns the pending map and the
// debounce timer (avoids the Timer.Reset semantics that previously
// allowed a queued AfterFunc to fire after Close, see #93 self-review
// C1/C2). Delivery goroutines are bounded by Config.MaxConcurrentDeliveries.
type Webhook struct {
	cfg Config

	mu      sync.Mutex
	pending map[pendingKey]*Event

	// wake is signalled (non-blocking) by Enqueue whenever the pending
	// set transitions from empty → non-empty, prompting the dispatcher
	// to (re)start the debounce timer. The dispatcher reads at most one
	// value per debounce tick.
	wake chan struct{}

	// deliverSem bounds in-flight POSTs to MaxConcurrentDeliveries.
	deliverSem chan struct{}

	wg     sync.WaitGroup
	closed chan struct{} // closed by Close to signal teardown
}

// New constructs a Webhook from cfg and starts its dispatcher goroutine.
// Call Close to release.
//
// Returns an error if cfg.URL is empty.
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
	if cfg.MaxBackoff == 0 {
		cfg.MaxBackoff = defaultMaxBackoff
	}
	if cfg.MaxBodyBytes == 0 {
		cfg.MaxBodyBytes = defaultMaxBodyBytes
	}
	if cfg.MaxConcurrentDeliveries <= 0 {
		cfg.MaxConcurrentDeliveries = defaultMaxConcurrent
	}
	if cfg.HTTPClient == nil {
		cfg.HTTPClient = &http.Client{Timeout: defaultHTTPTimeout}
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}

	w := &Webhook{
		cfg:        cfg,
		pending:    make(map[pendingKey]*Event),
		wake:       make(chan struct{}, 1),
		deliverSem: make(chan struct{}, cfg.MaxConcurrentDeliveries),
		closed:     make(chan struct{}),
	}
	w.wg.Add(1)
	go w.dispatchLoop()
	return w, nil
}

// Enqueue submits an event for delivery. Events are coalesced by the
// pair (Room, Type): two events in the same debounce window with the
// same room AND same event type collapse to the most-recent one. A
// Connect followed by an Update for the same room in the same window
// produces two separate POSTs (one Connect, one Update) — only same-
// type same-room events get coalesced, so distinct event categories
// never overwrite each other.
//
// Enqueue on a closed Webhook silently drops the event.
func (w *Webhook) Enqueue(e Event) {
	if e.Timestamp.IsZero() {
		e.Timestamp = time.Now()
	}

	w.mu.Lock()
	select {
	case <-w.closed:
		w.mu.Unlock()
		return
	default:
	}
	w.pending[pendingKey{room: e.Room, eventType: e.Type}] = &e
	w.mu.Unlock()

	// Non-blocking signal: the dispatcher reads at most once per tick.
	select {
	case w.wake <- struct{}{}:
	default:
	}
}

// dispatchLoop is the single goroutine that owns the debounce timer.
// Replacing the previous AfterFunc + Timer.Reset structure with a single
// dedicated goroutine eliminates the race where a queued AfterFunc fires
// after Close returns (#93 self-review C1, C2) — here the only exit path
// is Close → close(w.closed), which both stops any in-flight timer and
// guarantees no further dispatches.
func (w *Webhook) dispatchLoop() {
	defer w.wg.Done()
	var timer *time.Timer
	for {
		// Wait for either a wake (queue non-empty), close, or the
		// debounce timer firing.
		var timerC <-chan time.Time
		if timer != nil {
			timerC = timer.C
		}
		select {
		case <-w.closed:
			if timer != nil {
				timer.Stop()
			}
			// Final drain pass so anything still pending when Close was
			// called gets one last delivery attempt before Close's
			// wg.Wait returns.
			w.drainAndDeliver()
			return
		case <-w.wake:
			// Start the debounce timer if not already running.
			if timer == nil {
				timer = time.NewTimer(w.cfg.Debounce)
			}
		case <-timerC:
			timer = nil
			w.drainAndDeliver()
		}
	}
}

// drainAndDeliver moves all pending events to a local batch and dispatches
// each. Delivery goroutines obey the MaxConcurrentDeliveries semaphore so
// a slow receiver cannot accumulate unbounded goroutines.
func (w *Webhook) drainAndDeliver() {
	w.mu.Lock()
	if len(w.pending) == 0 {
		w.mu.Unlock()
		return
	}
	batch := make([]Event, 0, len(w.pending))
	for _, e := range w.pending {
		batch = append(batch, *e)
	}
	w.pending = make(map[pendingKey]*Event)
	w.mu.Unlock()

	for _, ev := range batch {
		// Acquire a delivery slot. Block on close to avoid spawning new
		// deliveries after Close, but otherwise let bursty workloads back
		// up here rather than at the goroutine count.
		select {
		case w.deliverSem <- struct{}{}:
		case <-w.closed:
			return
		}
		w.wg.Add(1)
		go func(e Event) {
			defer func() {
				<-w.deliverSem
				w.wg.Done()
			}()
			w.deliver(e)
		}(ev)
	}
}

// deliver serialises one event to JSON, signs it, and POSTs it with
// bounded retry. Drop policy: 4xx → no retry (receiver said no); 5xx
// and transport errors retry up to MaxRetries with exponential backoff
// capped at MaxBackoff, with ±20% jitter. Body too large → drop
// immediately with a logged warning.
func (w *Webhook) deliver(e Event) {
	body, err := json.Marshal(e)
	if err != nil {
		w.cfg.Logger.Error("webhook: marshal failed; event dropped",
			"room", e.Room, "type", e.Type, "err", err)
		return
	}
	if int64(len(body)) > w.cfg.MaxBodyBytes {
		w.cfg.Logger.Warn("webhook: event body exceeds MaxBodyBytes; dropped",
			"room", e.Room, "type", e.Type,
			"bodyBytes", len(body), "maxBodyBytes", w.cfg.MaxBodyBytes)
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
			sleep := backoff + jitter(backoff)
			select {
			case <-time.After(sleep):
			case <-w.closed:
				return // abort on shutdown
			}
			backoff *= 2
			if backoff > w.cfg.MaxBackoff {
				backoff = w.cfg.MaxBackoff
			}
		}

		retry, ok := w.postOnce(body, sig)
		if ok {
			return
		}
		if !retry {
			return // 4xx — receiver rejected, no retry
		}
	}
	w.cfg.Logger.Warn("webhook: retry budget exhausted; event dropped",
		"room", e.Room, "type", e.Type,
		"url", w.cfg.URL, "attempts", w.cfg.MaxRetries)
}

// jitter returns a duration in [-d/5, +d/5] sourced from crypto/rand so
// concurrent webhooks don't lock-step their retries against a struggling
// receiver. crypto/rand keeps the jitter unbiased even when called from
// many goroutines simultaneously — math/rand would need a lock or a
// per-goroutine source.
func jitter(d time.Duration) time.Duration {
	if d <= 0 {
		return 0
	}
	span := int64(d) / jitterFractionDenominator
	if span <= 0 {
		return 0
	}
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return 0 // jitter is an optimisation; failure is non-fatal
	}
	u := binary.LittleEndian.Uint64(b[:]) & 0x7FFFFFFFFFFFFFFF // strip sign bit
	return time.Duration(int64(u%uint64(2*span)) - span)
}

// postOnce performs a single POST attempt. Returns (retry, ok) where
// ok=true means delivery succeeded, retry=true means try again later.
func (w *Webhook) postOnce(body []byte, sig string) (retry, ok bool) {
	req, err := http.NewRequest(http.MethodPost, w.cfg.URL, bytes.NewReader(body))
	if err != nil {
		return false, false
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
		return true, false
	default:
		return false, false
	}
}

// Close stops accepting new events, fires any pending events
// synchronously, and waits for in-flight deliveries to finish or for
// ctx to cancel.
//
// After Close returns, calls to Enqueue silently drop the event.
func (w *Webhook) Close(ctx context.Context) error {
	w.mu.Lock()
	select {
	case <-w.closed:
		w.mu.Unlock()
		return nil
	default:
	}
	close(w.closed)
	w.mu.Unlock()

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
// Timestamp. Exported so test code and downstream tooling can verify
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
