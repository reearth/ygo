// Package main demonstrates HTTP-based Yjs document synchronisation using ygo.
//
// Run with: go run ./examples/http-sync
//
// The HTTP provider exposes two endpoints:
//
//	GET  /doc/{room}?sv=<base64-state-vector>  — returns a binary update diff
//	POST /doc/{room}                            — applies a binary update body
//
// This pull-push pattern works with any HTTP client, including curl:
//
//	# Push an update
//	curl -X POST http://localhost:9090/doc/my-room --data-binary @update.bin
//
//	# Pull everything since a given state vector
//	curl "http://localhost:9090/doc/my-room?sv=$(base64 -i sv.bin)"
package main

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"

	"github.com/reearth/ygo/crdt"
	yhttp "github.com/reearth/ygo/provider/http"
)

func main() {
	// ─────────────────────────────────────────────────────────────────────────
	// Phase 1: Start HTTP server
	// ─────────────────────────────────────────────────────────────────────────
	fmt.Println("=== Phase 1: Start HTTP server ===")

	// yhttp.NewServer() creates an in-memory Yjs document store that handles
	// GET (pull diff) and POST (push update) for any room name.
	// httptest.NewServer binds it to a random local port so this example
	// needs no configuration and leaves no ports open after it exits.
	srv := httptest.NewServer(yhttp.NewServer())
	defer srv.Close()

	fmt.Printf("Server listening at %s\n", srv.URL)
	fmt.Println()

	// Each peer is an independent crdt.Doc with a fixed ClientID so the demo
	// output is reproducible. In production you would use crdt.New() which
	// generates a random 64-bit ClientID.
	peerA := crdt.New(crdt.WithClientID(1))
	peerB := crdt.New(crdt.WithClientID(2))

	fmt.Println("Peer A (ClientID=1) and Peer B (ClientID=2) created.")
	fmt.Println()

	// ─────────────────────────────────────────────────────────────────────────
	// Phase 2: Peer A writes and pushes to server
	// ─────────────────────────────────────────────────────────────────────────
	fmt.Println("=== Phase 2: Peer A inserts content and pushes to server ===")

	// Obtain the shared text type before entering the transaction.
	// GetText (like all GetXxx methods) acquires the document mutex briefly,
	// and Transact also holds the mutex for its duration — so we must NOT call
	// GetText inside a Transact callback to avoid a re-entrant deadlock.
	notesA := peerA.GetText("notes")

	// Peer A makes a local change inside a transaction.
	// The transaction batches all mutations and fires observers once on commit.
	peerA.Transact(func(txn *crdt.Transaction) {
		notesA.Insert(txn, 0, "Hello from Peer A!", nil)
	})

	// Encode the entire Peer A document as a V1 binary update.
	// Passing nil as the state vector means "give me everything".
	// The result is a compact binary blob that is transport-agnostic.
	updateA := crdt.EncodeStateAsUpdateV1(peerA, nil)
	fmt.Printf("Peer A full-state update size: %d bytes\n", len(updateA))

	// POST the binary update to the server's "shared-notes" room.
	// The server applies it to its own in-memory document for that room,
	// making the content available for other peers to pull.
	mustPost(srv.URL+"/doc/shared-notes", updateA)
	fmt.Println("Peer A pushed update to server (room: shared-notes)")
	fmt.Println()

	// ─────────────────────────────────────────────────────────────────────────
	// Phase 3: Peer B pulls from server and converges
	// ─────────────────────────────────────────────────────────────────────────
	fmt.Println("=== Phase 3: Peer B pulls from server and applies the diff ===")

	// Before pulling, Peer B encodes its own state vector — a compact summary
	// of every client ID and the highest clock it has already seen from each.
	// An empty document has an empty state vector (zero bytes after encoding).
	svB := crdt.EncodeStateVectorV1(peerB)
	fmt.Printf("Peer B state vector size: %d bytes (empty doc)\n", len(svB))

	// The ?sv= parameter tells the server "I already have everything up to
	// this state — only send me what's new." This is the bandwidth-saving
	// heart of the Yjs sync protocol: clients never re-download content they
	// already have.
	diff := mustGet(srv.URL + "/doc/shared-notes?sv=" + base64.StdEncoding.EncodeToString(svB))
	fmt.Printf("Received diff from server: %d bytes\n", len(diff))

	// Apply the diff to Peer B's local document. The origin string
	// "http-server" lets observers distinguish remote updates from local ones.
	if err := crdt.ApplyUpdateV1(peerB, diff, "http-server"); err != nil {
		log.Fatalf("Peer B apply failed: %v", err)
	}

	// After applying, Peer B's document should be identical to Peer A's.
	textB := peerB.GetText("notes").ToString()
	fmt.Printf("Peer B GetText(\"notes\") = %q\n", textB)
	fmt.Println()

	// ─────────────────────────────────────────────────────────────────────────
	// Phase 4: Incremental sync — only new content travels
	// ─────────────────────────────────────────────────────────────────────────
	fmt.Println("=== Phase 4: Incremental sync — only new content travels ===")

	// Capture Peer B's state vector NOW, before Peer A's next change.
	// This records what Peer B already knows, so the next pull will return
	// only the delta produced by Peer A's upcoming write.
	svBefore := crdt.EncodeStateVectorV1(peerB)

	// Peer A appends more text. In a real YText this would be a separate
	// insert at the end of the existing content.
	// notesA was obtained earlier (before Phase 2); the same *YText pointer
	// is valid for the lifetime of the document.
	peerA.Transact(func(txn *crdt.Transaction) {
		notesA.Insert(txn, notesA.Len(), " And more from Peer A.", nil)
	})

	// Push Peer A's full state to the server again.
	// (You could also push only the incremental update if you track it via
	// an OnUpdate observer — see the ygo docs for that approach.)
	fullUpdateA2 := crdt.EncodeStateAsUpdateV1(peerA, nil)
	mustPost(srv.URL+"/doc/shared-notes", fullUpdateA2)
	fmt.Printf("Peer A full-state update size after 2nd write: %d bytes\n", len(fullUpdateA2))

	// Peer B pulls using the state vector it captured BEFORE Peer A's second
	// write. The server computes the diff and returns only the new content.
	incrementalDiff := mustGet(srv.URL + "/doc/shared-notes?sv=" + base64.StdEncoding.EncodeToString(svBefore))
	fmt.Printf("Incremental diff size (only new content): %d bytes\n", len(incrementalDiff))
	fmt.Printf("Bandwidth saving: %d bytes vs %d bytes full update\n", len(incrementalDiff), len(fullUpdateA2))

	if err := crdt.ApplyUpdateV1(peerB, incrementalDiff, "http-server"); err != nil {
		log.Fatalf("Peer B incremental apply failed: %v", err)
	}

	fmt.Printf("Peer B GetText(\"notes\") = %q\n", peerB.GetText("notes").ToString())
	fmt.Println()

	// ─────────────────────────────────────────────────────────────────────────
	// Phase 5: Bidirectional sync using YMap
	// ─────────────────────────────────────────────────────────────────────────
	// YText is a single shared sequence — if both peers insert at position 0
	// concurrently, YATA merges both without losing either. But for a clean
	// demo of bidirectional sync we use a YMap where each peer writes to its
	// own named key, making the expected outcome obvious.
	fmt.Println("=== Phase 5: Peer B writes and syncs back ===")

	// Obtain map references before entering transactions (same re-entrancy
	// rule as GetText: GetMap acquires d.mu, which Transact already holds).
	greetMapA := peerA.GetMap("greetings")
	greetMapB := peerB.GetMap("greetings")

	// Peer A writes its greeting into the shared "greetings" map.
	peerA.Transact(func(txn *crdt.Transaction) {
		greetMapA.Set(txn, "peerA", "Hello from Peer A!")
	})
	mustPost(srv.URL+"/doc/shared-notes", crdt.EncodeStateAsUpdateV1(peerA, nil))
	fmt.Println("Peer A set greetings[\"peerA\"] and pushed to server.")

	// Peer B writes its own greeting — this happens without first pulling from
	// the server, simulating a concurrent offline write.
	peerB.Transact(func(txn *crdt.Transaction) {
		greetMapB.Set(txn, "peerB", "And Peer B says hello too!")
	})
	mustPost(srv.URL+"/doc/shared-notes", crdt.EncodeStateAsUpdateV1(peerB, nil))
	fmt.Println("Peer B set greetings[\"peerB\"] and pushed to server.")

	// Now both peers pull the full server state. Because the server has
	// received updates from both peers, a single pull makes both peers
	// converge to the same document state without any conflict.

	// Peer A pulls what it's missing (Peer B's greetings entry).
	svA := crdt.EncodeStateVectorV1(peerA)
	diffForA := mustGet(srv.URL + "/doc/shared-notes?sv=" + base64.StdEncoding.EncodeToString(svA))
	if err := crdt.ApplyUpdateV1(peerA, diffForA, "http-server"); err != nil {
		log.Fatalf("Peer A pull failed: %v", err)
	}

	// Peer B pulls what it's missing (Peer A's greetings entry).
	svB2 := crdt.EncodeStateVectorV1(peerB)
	diffForB := mustGet(srv.URL + "/doc/shared-notes?sv=" + base64.StdEncoding.EncodeToString(svB2))
	if err := crdt.ApplyUpdateV1(peerB, diffForB, "http-server"); err != nil {
		log.Fatalf("Peer B pull failed: %v", err)
	}

	fmt.Println()

	// ─────────────────────────────────────────────────────────────────────────
	// Summary
	// ─────────────────────────────────────────────────────────────────────────
	fmt.Println("=== Summary ===")
	fmt.Println("All changes converged. Both peers see identical content.")
	fmt.Println()

	greetingsA := peerA.GetMap("greetings").Entries()
	greetingsB := peerB.GetMap("greetings").Entries()

	fmt.Printf("Peer A greetings map: %v\n", greetingsA)
	fmt.Printf("Peer B greetings map: %v\n", greetingsB)

	notesTextA := peerA.GetText("notes").ToString()
	notesTextB := peerB.GetText("notes").ToString()

	fmt.Printf("\nPeer A notes text: %q\n", notesTextA)
	fmt.Printf("Peer B notes text: %q\n", notesTextB)

	if notesTextA == notesTextB {
		fmt.Println("\nNotes text: CONVERGED")
	} else {
		fmt.Println("\nNotes text: DIVERGED (unexpected)")
	}

	if fmt.Sprint(greetingsA) == fmt.Sprint(greetingsB) {
		fmt.Println("Greetings map: CONVERGED")
	} else {
		fmt.Println("Greetings map: DIVERGED (unexpected)")
	}
}

// mustPost sends a binary update to the server via HTTP POST.
// It panics on any network or non-2xx error, keeping the example concise.
func mustPost(url string, body []byte) {
	resp, err := http.Post(url, "application/octet-stream", bytes.NewReader(body)) //nolint:noctx
	if err != nil {
		log.Fatalf("POST %s failed: %v", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		b, _ := io.ReadAll(resp.Body)
		log.Fatalf("POST %s returned %d: %s", url, resp.StatusCode, b)
	}
}

// mustGet fetches a binary update from the server via HTTP GET.
// It returns the raw response body (a V1 binary update) or panics.
func mustGet(url string) []byte {
	resp, err := http.Get(url) //nolint:noctx
	if err != nil {
		log.Fatalf("GET %s failed: %v", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		b, _ := io.ReadAll(resp.Body)
		log.Fatalf("GET %s returned %d: %s", url, resp.StatusCode, b)
	}
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		log.Fatalf("GET %s read body failed: %v", url, err)
	}
	return data
}
