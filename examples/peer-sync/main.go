// Package main demonstrates the ygo sync protocol without any network transport.
//
// Run with: go run ./examples/peer-sync
//
// The y-protocols sync handshake has three message types:
//
//	sync.MsgSyncStep1 (0) — "here is my state vector; send me what I'm missing"
//	sync.MsgSyncStep2 (1) — "here is everything you're missing"
//	sync.MsgUpdate    (2) — "here is a new incremental change"
//
// A typical two-peer handshake looks like this:
//
//	Peer A                          Peer B
//	  │─── SyncStep1(svA) ────────────▶│  "I have up to clock X"
//	  │◀─── SyncStep2(diff) ──────────│  "Here's what you're missing"
//	  │─── SyncStep1(svA) ◀───────────│  "I have up to clock Y"
//	  │─── SyncStep2(diff) ────────────▶│  "Here's what you're missing"
//	  │                                │
//	  │  (both peers now converged)    │
//	  │                                │
//	  │─── Update(delta) ─────────────▶│  "I just made a new edit"
//	  │◀─── (applied, no reply) ───────│
//
// After the initial handshake, incremental updates flow as MsgUpdate messages.
// The sync package handles all message encoding/decoding; the transport
// (channels here, WebSocket in production) just moves the bytes.
package main

import (
	"fmt"
	"log"

	"github.com/reearth/ygo/crdt"
	"github.com/reearth/ygo/sync"
)

// peer wraps a Yjs document with a name for logging.
type peer struct {
	name string
	doc  *crdt.Doc
}

// send encodes a sync message and "sends" it to the other peer by calling
// their receive method. In production this would write to a WebSocket or TCP conn.
func (p *peer) send(to *peer, msg []byte) {
	fmt.Printf("  %s → %s: %s (%d bytes)\n", p.name, to.name, describeMsg(msg), len(msg))
	reply, err := sync.ApplySyncMessage(to.doc, msg, p.name)
	if err != nil {
		log.Printf("  [error] %s could not apply message: %v", to.name, err)
		return
	}
	if reply != nil {
		// A step-1 message automatically produces a step-2 reply.
		// Send the reply back.
		to.send(p, reply)
	}
}

// describeMsg returns a human-readable label for a sync message.
func describeMsg(msg []byte) string {
	if len(msg) == 0 {
		return "empty"
	}
	switch msg[0] {
	case 0:
		return "SyncStep1 (state-vector)"
	case 1:
		return "SyncStep2 (update diff)"
	case 2:
		return "Update (incremental)"
	default:
		return fmt.Sprintf("unknown(type=%d)", msg[0])
	}
}

// printStateVector prints a human-readable representation of a state vector.
// A state vector maps each ClientID to the highest integrated clock from that client.
func printStateVector(label string, sv crdt.StateVector) {
	if len(sv) == 0 {
		fmt.Printf("  %s: {} (empty — no operations yet)\n", label)
		return
	}
	fmt.Printf("  %s: {", label)
	first := true
	for client, clock := range sv {
		if !first {
			fmt.Print(", ")
		}
		fmt.Printf("client%d: %d", client, clock)
		first = false
	}
	fmt.Println("}")
}

func main() {
	// ─────────────────────────────────────────────────────────────────────────
	// Phase 1 — Both peers start empty
	// ─────────────────────────────────────────────────────────────────────────
	fmt.Println("=== Phase 1: Initial state — both peers are empty ===")
	fmt.Println()

	// Each peer has its own independent in-memory document.
	// WithClientID fixes the ID so the output is deterministic and educational.
	// In a real application you would use crdt.New() which generates a random ID.
	alice := &peer{"Alice", crdt.New(crdt.WithClientID(1))}
	bob := &peer{"Bob", crdt.New(crdt.WithClientID(2))}

	// State vectors are empty because no operations have been applied yet.
	printStateVector("Alice state vector", alice.doc.StateVector())
	printStateVector("Bob   state vector", bob.doc.StateVector())
	fmt.Println()

	// ─────────────────────────────────────────────────────────────────────────
	// Phase 2 — Alice makes edits before sync
	// ─────────────────────────────────────────────────────────────────────────
	fmt.Println("=== Phase 2: Alice makes edits offline ===")
	fmt.Println()

	// Alice inserts text, array items, and a map entry — all while Bob is
	// completely unaware. This models the "offline first" scenario: a user
	// edits a document on a plane, then syncs when they land.
	alice.doc.Transact(func(txn *crdt.Transaction) {
		alice.doc.GetText("shared").Insert(txn, 0, "Hello from Alice!", nil)
		alice.doc.GetArray("nums").Insert(txn, 0, []any{1, 2, 3})
		alice.doc.GetMap("meta").Set(txn, "author", "Alice")
	})

	// Alice's state vector now reflects the three insertions she just made.
	printStateVector("Alice state vector", alice.doc.StateVector())
	// Bob's state vector is still empty — he knows nothing of Alice's edits.
	printStateVector("Bob   state vector (still empty)", bob.doc.StateVector())
	fmt.Println()

	// Peers diverge naturally; the sync protocol reconciles them.
	// The CRDT guarantees that no matter how diverged peers become, applying
	// the same set of operations (in any order) produces the same final state.

	// ─────────────────────────────────────────────────────────────────────────
	// Phase 3 — Bob also makes edits before sync
	// ─────────────────────────────────────────────────────────────────────────
	fmt.Println("=== Phase 3: Bob makes edits offline ===")
	fmt.Println()

	// Bob independently edits the same shared types.
	// Neither peer knows about the other's changes yet — this is the "offline /
	// concurrent" scenario that CRDTs handle. The YATA algorithm will merge
	// both sets of edits deterministically when they finally exchange updates.
	bob.doc.Transact(func(txn *crdt.Transaction) {
		bob.doc.GetText("shared").Insert(txn, 0, "Hello from Bob!", nil)
		bob.doc.GetMap("meta").Set(txn, "topic", "greeting")
	})

	printStateVector("Bob   state vector", bob.doc.StateVector())
	fmt.Println()

	// ─────────────────────────────────────────────────────────────────────────
	// Phase 4 — The sync handshake
	// ─────────────────────────────────────────────────────────────────────────
	fmt.Println("=== Phase 4: Sync handshake ===")
	fmt.Println("Alice and Bob exchange SyncStep1 messages to discover what each other has.")
	fmt.Println()

	// Alice initiates the handshake by broadcasting her state vector.
	// EncodeSyncStep1 serialises the state vector as:
	//   [0x00] [varuint(svLength)] [client₁, clock₁] [client₂, clock₂] ...
	//
	// When Bob receives it, ApplySyncMessage:
	//   1. Decodes Alice's state vector
	//   2. Computes what Alice is missing (everything Bob has that Alice hasn't seen)
	//   3. Returns a SyncStep2 message carrying that diff
	//
	// peer.send() automatically delivers the SyncStep2 reply back to Alice,
	// who applies it — learning all of Bob's offline edits.
	fmt.Println("  [Alice → Bob: initiating handshake]")
	alice.send(bob, sync.EncodeSyncStep1(alice.doc))
	fmt.Println()

	fmt.Println("  After Alice→Bob step-1/step-2:")
	printStateVector("Alice state vector", alice.doc.StateVector())
	printStateVector("Bob   state vector", bob.doc.StateVector())
	fmt.Printf("  Alice text: %q\n", alice.doc.GetText("shared").ToString())
	fmt.Printf("  Bob   text: %q\n", bob.doc.GetText("shared").ToString())
	fmt.Println()

	// Bob initiates his own step-1 so Alice learns what Bob had that she was missing.
	// This completes the bidirectional handshake. After both step-1 messages,
	// each peer has sent its full state vector and received the other's diff.
	fmt.Println("  [Bob → Alice: completing handshake]")
	bob.send(alice, sync.EncodeSyncStep1(bob.doc))
	fmt.Println()

	fmt.Println("  After Bob→Alice step-1/step-2:")
	printStateVector("Alice state vector", alice.doc.StateVector())
	printStateVector("Bob   state vector", bob.doc.StateVector())
	fmt.Printf("  Alice text: %q\n", alice.doc.GetText("shared").ToString())
	fmt.Printf("  Bob   text: %q\n", bob.doc.GetText("shared").ToString())
	fmt.Println()

	// ─────────────────────────────────────────────────────────────────────────
	// Phase 5 — Verify convergence
	// ─────────────────────────────────────────────────────────────────────────
	fmt.Println("=== Phase 5: Convergence check ===")
	fmt.Println()

	aliceText := alice.doc.GetText("shared").ToString()
	bobText := bob.doc.GetText("shared").ToString()

	fmt.Printf("  Alice sees text:  %q\n", aliceText)
	fmt.Printf("  Bob   sees text:  %q\n", bobText)
	fmt.Println()

	if aliceText == bobText {
		fmt.Println("  CONVERGED: both peers have identical text.")
	} else {
		fmt.Println("  DIVERGED: text differs — this would be a CRDT bug.")
	}
	fmt.Println()

	// YATA (Yet Another Transformation Approach) guarantees deterministic
	// conflict resolution for concurrent inserts. When two peers insert at the
	// same position simultaneously, the item from the peer with the lower
	// ClientID is placed first. ClientID 1 (Alice) < ClientID 2 (Bob), so
	// Alice's text precedes Bob's in the merged result.

	aliceNums := alice.doc.GetArray("nums").ToSlice()
	bobNums := bob.doc.GetArray("nums").ToSlice()
	fmt.Printf("  Alice sees nums:  %v\n", aliceNums)
	fmt.Printf("  Bob   sees nums:  %v\n", bobNums)
	fmt.Println()

	aliceMeta := alice.doc.GetMap("meta").Entries()
	bobMeta := bob.doc.GetMap("meta").Entries()
	fmt.Printf("  Alice sees meta:  %v\n", aliceMeta)
	fmt.Printf("  Bob   sees meta:  %v\n", bobMeta)
	fmt.Println()

	if fmt.Sprint(aliceNums) == fmt.Sprint(bobNums) &&
		fmt.Sprint(aliceMeta) == fmt.Sprint(bobMeta) {
		fmt.Println("  CONVERGED: array and map are identical on both peers.")
	} else {
		fmt.Println("  DIVERGED on array or map — this would be a CRDT bug.")
	}
	fmt.Println()

	// ─────────────────────────────────────────────────────────────────────────
	// Phase 6 — Incremental updates after sync
	// ─────────────────────────────────────────────────────────────────────────
	fmt.Println("=== Phase 6: Incremental updates (post-handshake) ===")
	fmt.Println()

	// After the initial handshake, new edits travel as MsgUpdate (type 2)
	// rather than the full step-1/step-2 exchange. This is far more efficient:
	// only the delta since svBeforeEdit is encoded, not the entire document.
	//
	// Capture Alice's state vector just before her edit so we can compute the
	// minimal delta to send to Bob.
	svBeforeEdit := alice.doc.StateVector()

	alice.doc.Transact(func(txn *crdt.Transaction) {
		// Append " ✓" to the end of the shared text.
		text := alice.doc.GetText("shared")
		text.Insert(txn, text.Len(), " ✓", nil)
	})

	// Encode only the new content (the items with clocks > svBeforeEdit).
	// This is a tiny message — just the single inserted string — rather than
	// re-encoding Alice's entire document history.
	delta := crdt.EncodeStateAsUpdateV1(alice.doc, svBeforeEdit)

	// Wrap the raw V1 update in a MsgUpdate envelope (first byte = 0x02).
	msg := sync.EncodeUpdate(delta)
	fmt.Printf("  Alice appended \" ✓\" to shared text\n")
	fmt.Printf("  Incremental update size: %d bytes (vs full state: %d bytes)\n",
		len(msg), len(alice.doc.EncodeStateAsUpdate()))
	fmt.Println()

	alice.send(bob, msg)
	fmt.Println()

	fmt.Printf("  Alice text: %q\n", alice.doc.GetText("shared").ToString())
	fmt.Printf("  Bob   text: %q\n", bob.doc.GetText("shared").ToString())
	fmt.Println()

	// After the initial handshake, incremental updates use MsgUpdate (type 2)
	// rather than the full step-1/step-2 exchange. This is the hot path in
	// production: every keystroke produces one tiny MsgUpdate.

	// ─────────────────────────────────────────────────────────────────────────
	// Phase 7 — Three-peer scenario (relay pattern)
	// ─────────────────────────────────────────────────────────────────────────
	fmt.Println("=== Phase 7: Three peers — relay pattern ===")
	fmt.Println()

	// Charlie is a brand-new peer who knows nothing about Alice or Bob's work.
	// This models a new browser tab joining a room, or a new server replica
	// spinning up and needing to catch up with the existing cluster.
	charlie := &peer{"Charlie", crdt.New(crdt.WithClientID(3))}

	fmt.Printf("  Charlie starts with empty doc\n")
	printStateVector("Charlie state vector", charlie.doc.StateVector())
	fmt.Println()

	// Charlie performs the standard step-1/step-2 handshake with Alice.
	// After this exchange Charlie will have a complete copy of Alice's document.
	fmt.Println("  [Charlie ↔ Alice: handshake]")
	charlie.send(alice, sync.EncodeSyncStep1(charlie.doc))
	alice.send(charlie, sync.EncodeSyncStep1(alice.doc))
	fmt.Println()

	fmt.Printf("  Charlie text after Alice sync: %q\n", charlie.doc.GetText("shared").ToString())
	fmt.Println()

	// Charlie now has Alice's full state. In a relay/hub topology (like a
	// y-websocket server), Charlie would also sync with Bob directly, or the
	// server would relay updates between all connected clients.
	// Here we demonstrate Charlie syncing with Bob independently.
	fmt.Println("  [Charlie ↔ Bob: handshake]")
	charlie.send(bob, sync.EncodeSyncStep1(charlie.doc))
	bob.send(charlie, sync.EncodeSyncStep1(bob.doc))
	fmt.Println()

	// All three peers should now be fully converged.
	fmt.Println("  Final convergence check:")
	fmt.Printf("  Alice   text: %q\n", alice.doc.GetText("shared").ToString())
	fmt.Printf("  Bob     text: %q\n", bob.doc.GetText("shared").ToString())
	fmt.Printf("  Charlie text: %q\n", charlie.doc.GetText("shared").ToString())
	fmt.Println()

	aliceText = alice.doc.GetText("shared").ToString()
	bobText = bob.doc.GetText("shared").ToString()
	charlieText := charlie.doc.GetText("shared").ToString()

	if aliceText == bobText && bobText == charlieText {
		fmt.Println("  All three peers CONVERGED.")
	} else {
		fmt.Println("  Peers DIVERGED — this would be a CRDT bug.")
	}
	fmt.Println()

	// ─────────────────────────────────────────────────────────────────────────
	// Summary
	// ─────────────────────────────────────────────────────────────────────────
	fmt.Println("=== Summary ===")
	fmt.Println("The sync protocol is just three message types over any byte channel.")
	fmt.Println("WebSocket, HTTP, TCP, channels — the transport does not matter.")
	fmt.Println("ygo's sync package handles the encoding; you handle the transport.")
}
