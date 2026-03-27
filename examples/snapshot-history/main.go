// Package main demonstrates document versioning using ygo snapshots.
//
// Run with: go run ./examples/snapshot-history
//
// Snapshots capture the full state of a Yjs document at a point in time:
// the state vector (which items existed) and the delete set (which were deleted).
// They are compact (just two state vectors — no item content is duplicated) and
// can be encoded/decoded for storage in a database or file.
//
// Key operations demonstrated:
//
//	crdt.CaptureSnapshot(doc)           — take a point-in-time snapshot
//	crdt.EncodeSnapshot(snap)           — serialise to bytes (store in DB, file, etc.)
//	crdt.DecodeSnapshot(data)           — deserialise
//	crdt.RestoreDocument(doc, snap)     — reconstruct the document at snapshot time
//	crdt.EncodeStateFromSnapshot(doc, snap) — get an update representing the historical state
package main

import (
	"fmt"
	"strings"

	"github.com/reearth/ygo/crdt"
)

// encodedSnap is a stored snapshot with metadata for the version history log.
type encodedSnap struct {
	revision int
	label    string
	data     []byte // encoded snapshot bytes — these can be stored in a DB or file
}

func main() {
	// ── Phase 1: Build a document through multiple revisions ──────────────────

	fmt.Println("=== Phase 1: Building a document through multiple revisions ===")
	fmt.Println()

	// WithGC(false) is REQUIRED for full snapshot restoration.
	// It tells the document to keep all item history even after deletions.
	// Without it, deleted content is freed and cannot be re-read at restore time.
	doc := crdt.New(crdt.WithClientID(1), crdt.WithGC(false))
	article := doc.GetText("article")

	snapshots := make([]encodedSnap, 0, 4)

	// Revision 1: write the initial draft.
	doc.Transact(func(txn *crdt.Transaction) {
		article.Insert(txn, 0, "The quick brown fox", nil)
	})
	fmt.Printf("Revision 1: %q\n", article.ToString())
	// The snapshot is tiny — it only stores two state vectors, not the full content.
	snapshots = append(snapshots, encodedSnap{
		revision: 1,
		label:    "Initial draft",
		data:     crdt.EncodeSnapshot(crdt.CaptureSnapshot(doc)),
	})

	// Revision 2: complete the classic sentence.
	doc.Transact(func(txn *crdt.Transaction) {
		article.Insert(txn, article.Len(), " jumps over the lazy dog", nil)
	})
	fmt.Printf("Revision 2: %q\n", article.ToString())
	snapshots = append(snapshots, encodedSnap{
		revision: 2,
		label:    "Add 'jumps over the lazy dog'",
		data:     crdt.EncodeSnapshot(crdt.CaptureSnapshot(doc)),
	})

	// Revision 3: remove "lazy " — find its position and delete 5 characters.
	lazyPos := strings.Index(article.ToString(), "lazy ")
	doc.Transact(func(txn *crdt.Transaction) {
		article.Delete(txn, lazyPos, 5) // delete "lazy "
	})
	fmt.Printf("Revision 3: %q\n", article.ToString())
	snapshots = append(snapshots, encodedSnap{
		revision: 3,
		label:    "Delete 'lazy '",
		data:     crdt.EncodeSnapshot(crdt.CaptureSnapshot(doc)),
	})

	// Revision 4: append a closing remark.
	doc.Transact(func(txn *crdt.Transaction) {
		article.Insert(txn, article.Len(), " — a classic pangram.", nil)
	})
	fmt.Printf("Revision 4: %q\n", article.ToString())
	snapshots = append(snapshots, encodedSnap{
		revision: 4,
		label:    "Append closing remark",
		data:     crdt.EncodeSnapshot(crdt.CaptureSnapshot(doc)),
	})

	fmt.Println()

	// ── Phase 2: Inspect the stored snapshots ─────────────────────────────────

	fmt.Println("=== Phase 2: Examining the stored snapshots ===")
	fmt.Println()
	fmt.Println("Snapshots are small because they only record WHICH items existed (via")
	fmt.Println("clock ranges), not the content of those items. Content stays in the doc.")
	fmt.Println()

	for _, s := range snapshots {
		snap, err := crdt.DecodeSnapshot(s.data)
		if err != nil {
			fmt.Printf("  ERROR decoding revision %d: %v\n", s.revision, err)
			continue
		}

		hasDeletes := len(snap.DeleteSet.Clients()) > 0
		deleteDesc := "no deletions"
		if hasDeletes {
			deleteDesc = fmt.Sprintf("has deletions from %d client(s)", len(snap.DeleteSet.Clients()))
		}

		fmt.Printf("  Rev %d  %-35s  %3d bytes  %d client(s) in SV  %s\n",
			s.revision,
			fmt.Sprintf("(%s)", s.label),
			len(s.data),
			len(snap.StateVector),
			deleteDesc,
		)
	}
	fmt.Println()

	// ── Phase 3: Restore to past revisions ────────────────────────────────────

	fmt.Println("=== Phase 3: Restoring to past revisions ===")
	fmt.Println()
	fmt.Println("RestoreDocument requires the original doc to still have its full history")
	fmt.Println("(GC off). It creates a NEW doc — the original is unchanged.")
	fmt.Println("Items inserted after the snapshot are excluded; only deletions that")
	fmt.Println("existed at snapshot time are applied.")
	fmt.Println()

	// Restore in reverse order to show we can jump to any past revision.
	for i := len(snapshots) - 1; i >= 0; i-- {
		s := snapshots[i]
		snap, err := crdt.DecodeSnapshot(s.data)
		if err != nil {
			fmt.Printf("  ERROR decoding revision %d: %v\n", s.revision, err)
			continue
		}
		restored, err := crdt.RestoreDocument(doc, snap)
		if err != nil {
			fmt.Printf("  ERROR restoring revision %d: %v\n", s.revision, err)
			continue
		}
		restoredText := restored.GetText("article").ToString()
		fmt.Printf("  Restored rev %d: %q\n", s.revision, restoredText)
	}
	fmt.Println()

	// ── Phase 4: Share a historical version with another peer ─────────────────

	fmt.Println("=== Phase 4: Sharing a specific revision with Peer B ===")
	fmt.Println()
	fmt.Println("EncodeStateFromSnapshot produces a standard V1 update that any peer can")
	fmt.Println("apply. This lets you send a peer the document as it was at a specific")
	fmt.Println("point, not the current state.")
	fmt.Println()

	// Share Revision 2 with Peer B.
	snap2, err := crdt.DecodeSnapshot(snapshots[1].data) // index 1 = revision 2
	if err != nil {
		fmt.Printf("  ERROR: %v\n", err)
		return
	}

	historicalUpdate, err := crdt.EncodeStateFromSnapshot(doc, snap2)
	if err != nil {
		fmt.Printf("  ERROR: %v\n", err)
		return
	}
	fmt.Printf("  Historical update size (revision 2): %d bytes\n", len(historicalUpdate))

	peerB := crdt.New(crdt.WithClientID(2))
	if err := crdt.ApplyUpdateV1(peerB, historicalUpdate, nil); err != nil {
		fmt.Printf("  ERROR applying historical update: %v\n", err)
		return
	}

	peerBText := peerB.GetText("article").ToString()
	fmt.Printf("  Peer B received revision 2: %q\n", peerBText)
	fmt.Printf("  Current document state:     %q\n", article.ToString())
	fmt.Println()

	// ── Phase 5: Compare two snapshots (diff) ─────────────────────────────────

	fmt.Println("=== Phase 5: What changed between two snapshots? ===")
	fmt.Println()
	fmt.Println("Comparing state vectors tells you whether one snapshot is strictly")
	fmt.Println("'ahead' of another — any client whose clock advanced has new items.")
	fmt.Println()

	snap1, _ := crdt.DecodeSnapshot(snapshots[0].data) // revision 1
	snap4, _ := crdt.DecodeSnapshot(snapshots[3].data) // revision 4

	docAt1, _ := crdt.RestoreDocument(doc, snap1)
	docAt4, _ := crdt.RestoreDocument(doc, snap4)

	fmt.Printf("  Snapshot 1 text: %q\n", docAt1.GetText("article").ToString())
	fmt.Printf("  Snapshot 4 text: %q\n", docAt4.GetText("article").ToString())
	fmt.Println()

	fmt.Println("  State vector comparison (revision 1 → revision 4):")
	for client, clock4 := range snap4.StateVector {
		clock1 := snap1.StateVector[client]
		if clock4 > clock1 {
			fmt.Printf("    Client %d: clock %d → %d (+%d items added)\n",
				client, clock1, clock4, clock4-clock1)
		}
	}
	fmt.Println()

	// ── Phase 6: GC interaction ────────────────────────────────────────────────

	fmt.Println("=== Phase 6: Garbage collection and snapshot limitations ===")
	fmt.Println()

	// Create a new doc with GC enabled (the default).
	gcDoc := crdt.New(crdt.WithClientID(1), crdt.WithGC(true))
	gcText := gcDoc.GetText("notes")

	gcDoc.Transact(func(txn *crdt.Transaction) {
		gcText.Insert(txn, 0, "Meeting notes: discuss roadmap and Q3 targets.", nil)
	})

	snapBeforeDelete := crdt.CaptureSnapshot(gcDoc)
	fmt.Printf("  Snapshot A (before delete): %d bytes\n", len(crdt.EncodeSnapshot(snapBeforeDelete)))

	gcDoc.Transact(func(txn *crdt.Transaction) {
		// Delete "discuss roadmap and " (20 chars starting at position 16).
		gcText.Delete(txn, 16, 20)
	})
	fmt.Printf("  Text after deletion: %q\n", gcText.ToString())

	snapAfterDelete := crdt.CaptureSnapshot(gcDoc)
	fmt.Printf("  Snapshot B (after delete):  %d bytes\n", len(crdt.EncodeSnapshot(snapAfterDelete)))

	// Run GC — this frees memory for deleted items (replaces their content with
	// ContentDeleted tombstones). After this, the original content bytes are gone.
	crdt.RunGC(gcDoc)
	fmt.Println("  GC has run: deleted item content has been freed.")

	// Attempt to restore to snapshot A (before the delete).
	restoredGC, err := crdt.RestoreDocument(gcDoc, snapBeforeDelete)
	if err != nil {
		fmt.Printf("  Restoration after GC failed: %v\n", err)
	} else {
		restoredText := restoredGC.GetText("notes").ToString()
		// After GC the content bytes of deleted items are gone, so the restored
		// document only contains items that are still live (not freed).
		fmt.Printf("  Restored text (after GC): %q\n", restoredText)
	}
	fmt.Println()
	fmt.Println("  Warning: if GC has run, items deleted AFTER a snapshot can no longer be")
	fmt.Println("  restored to a 'live' state from before that deletion.")
	fmt.Println("  For full history: keep GC disabled (WithGC(false)).")
	fmt.Println("  For memory efficiency: enable GC but accept limited restoration.")
	fmt.Println()

	// ── Summary ───────────────────────────────────────────────────────────────

	fmt.Println("=== Summary ===")
	fmt.Printf("Snapshots let you preserve document history without storing full copies.\n")
	snap4size := len(crdt.EncodeSnapshot(crdt.CaptureSnapshot(doc)))
	docUpdateSize := len(doc.EncodeStateAsUpdate())
	fmt.Printf("Each snapshot is %d bytes (vs %d bytes for the full document update).\n",
		snap4size, docUpdateSize)
	fmt.Println("Use crdt.WithGC(false) on any document where you need full restoration capability.")
}
