// Package main demonstrates provider/client, ygo's embeddable offline-first
// sync client (#165): a *crdt.Doc that is immediately readable and editable —
// connected, disconnected, or never-yet-connected — backed by a local SQLite
// store for durability across restarts and a background dial loop that
// reconciles with a y-websocket server whenever one is reachable.
//
// Start a server first (either works — both speak the same wire protocol):
//
//	go run github.com/reearth/ygo/cmd/ygo-server -addr :1234
//	# or: go run ./examples/collab-editor/server   (then use -url ws://localhost:8080/yjs/offline-demo)
//
// Then run this example against it:
//
//	go run ./examples/offline-client -url ws://localhost:1234/yjs/offline-demo -db /tmp/offline-demo.db
//
// It appends a timestamped tag to a shared YText root once per -interval,
// forever, and logs every connection-state transition (see
// provider/client.Status) so you can watch it dial, sync, and — if you stop
// the server — keep right on editing. There is no error path for "the
// network is unreachable": for an offline-first client that is the ordinary
// case, not a failure (see the provider/client package doc for why). Kill the
// server and watch the edits keep landing in the log with StateDisconnected
// beneath them; bring the server back and the client reconnects on its own
// and the next sync handshake carries every edit made in between — there is
// no separate "resend the offline queue" step, because there is no offline
// queue (see docs/CLIENT.md). Stop this program (Ctrl-C) and run it again
// against the same -db to see content made while the process was down
// survive the restart, server or no server.
//
// Run two copies with the same -url and different -db paths (or in
// different working directories) to watch two independent clients converge
// on one shared document, y-websocket-style.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net/url"
	"os/signal"
	"path"
	"syscall"
	"time"

	"github.com/reearth/ygo/crdt"
	client "github.com/reearth/ygo/provider/client"
)

func main() {
	urlFlag := flag.String("url", "ws://localhost:1234/yjs/offline-demo", "y-websocket server URL; the final path segment names the room")
	dbFlag := flag.String("db", "offline-client.db", "path to this client's local SQLite store (empty = memory-only, no durability across restarts)")
	interval := flag.Duration("interval", 3*time.Second, "how often to make a local edit")
	flag.Parse()

	// The Doc is created and its shared root resolved before the client
	// exists at all — GetText acquires the doc's own lock, so (per the root
	// README's Gotchas section) it must happen outside any Transact, and
	// there is no reason to wait for New/Connect first: an offline-first
	// Doc is usable the instant it is constructed.
	doc := crdt.New()
	text := doc.GetText("notes")

	opts := client.Options{
		URL: *urlFlag,
		Doc: doc,
		// StorePath, not Store: this example has no reason to hold the
		// *client.SQLiteStore handle itself, so it lets New open (and later
		// Close own) the local database for it. An empty -db falls back to
		// StorePath's own zero value, i.e. memory-only durability — see
		// client.Options.Store's doc for what that trades away (content
		// survives disconnects but not a process restart).
		StorePath: *dbFlag,
	}
	c, err := client.New(opts)
	if err != nil {
		log.Fatalf("client.New: %v", err)
	}

	// OnStatus fires from this client's own background dial-loop goroutine
	// for every connection-lifecycle transition (provider/client.State).
	// StateSynced means the Doc has reconciled with the server on the
	// current connection — it does NOT mean any auth token was accepted;
	// this example sets none, so that caveat does not apply here, but see
	// client.Options.Token's "not a confidentiality gate" doc before wiring
	// one up in a real deployment.
	c.OnStatus(func(st client.Status) {
		switch st.State {
		case client.StateConnecting:
			log.Printf("status: connecting to %s", *urlFlag)
		case client.StateConnected:
			log.Printf("status: connected (handshake in flight)")
		case client.StateSynced:
			log.Printf("status: synced — doc reconciled with the server")
		case client.StateDisconnected:
			if st.Err != nil {
				log.Printf("status: disconnected: %v (will keep retrying with backoff)", st.Err)
			} else {
				log.Printf("status: disconnected (clean stop)")
			}
		}
	})

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// Connect hydrates from the store, then blocks for this client's whole
	// sync lifetime — dialing, handshaking, and reconnecting with backoff
	// on its own, indefinitely, until ctx is cancelled or Close is called
	// (see its own doc). It runs on its own goroutine because main's own
	// job below (editing on a ticker) must not wait on it: the Doc is fully
	// usable whether or not a connection ever succeeds.
	connectDone := make(chan error, 1)
	go func() { connectDone <- c.Connect(ctx) }()

	log.Printf("editing room %q every %s — store: %s", roomNameOf(*urlFlag), *interval, storeDescription(*dbFlag))
	log.Printf("press Ctrl-C to stop; edits made while disconnected are carried by the next sync handshake, not lost")

	ticker := time.NewTicker(*interval)
	defer ticker.Stop()

	n := 0
loop:
	for {
		select {
		case <-ctx.Done():
			break loop
		case <-ticker.C:
			n++
			doc.Transact(func(txn *crdt.Transaction) {
				text.Insert(txn, text.Len(), fmt.Sprintf("[%s #%d] ", time.Now().Format("15:04:05"), n), nil)
			})
			// Stats is a cheap, lock-free snapshot safe to poll at any rate.
			// Dropped should stay at zero with a store configured — see
			// docs/CLIENT.md's Stats section for the alerting rule this
			// mirrors from provider/websocket.RelayStats.
			s := c.Stats()
			log.Printf("edit #%d applied (doc length=%d runes) — stats: coalesced=%d awarenessSuperseded=%d hardDrops=%d dropped=%d",
				n, text.Len(), s.Coalesced, s.AwarenessSuperseded, s.HardDrops, s.Dropped)
		}
	}

	log.Printf("shutting down: closing client (store durability first, then a bounded best-effort network drain)")
	if err := c.Close(); err != nil {
		log.Printf("client.Close: %v", err)
	}
	if err := <-connectDone; err != nil {
		log.Printf("Connect returned: %v", err)
	}
	log.Printf("final content (%d runes): %q", text.Len(), text.ToString())
}

// roomNameOf extracts the same room name client.New derives from rawURL
// (its final path segment), purely for this example's own log line — it is
// not involved in anything client.New or Connect actually does. Falls back
// to rawURL itself if it cannot be parsed, since this is diagnostic-only.
func roomNameOf(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil {
		return rawURL
	}
	if room := path.Base(u.Path); room != "" && room != "." && room != "/" {
		return room
	}
	return rawURL
}

// storeDescription renders -db for the startup log line above.
func storeDescription(dbPath string) string {
	if dbPath == "" {
		return "none (memory-only — content will NOT survive a process restart)"
	}
	return dbPath
}
