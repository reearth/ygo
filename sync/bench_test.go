package sync_test

import (
	"strings"
	"testing"

	"github.com/reearth/ygo/crdt"
	"github.com/reearth/ygo/sync"
)

// buildDoc creates a doc with ~n single-character insertions into a YText.
// GetText is called outside the transaction, as required.
func buildDoc(clientID crdt.ClientID, n int) *crdt.Doc {
	doc := crdt.New(crdt.WithClientID(clientID))
	txt := doc.GetText("t")
	doc.Transact(func(txn *crdt.Transaction) {
		txt.Insert(txn, 0, strings.Repeat("a", n), nil)
	})
	return doc
}

// BenchmarkEncodeSyncStep1 measures encoding a step-1 message from a doc that
// contains ~100 items.
func BenchmarkEncodeSyncStep1(b *testing.B) {
	doc := buildDoc(1, 100)

	b.ResetTimer()
	b.ReportAllocs()

	for i := 0; i < b.N; i++ {
		_ = sync.EncodeSyncStep1(doc)
	}
}

// BenchmarkApplySyncMessage_Step1 measures the full round-trip cost of
// receiving a step-1 message and producing a step-2 reply.
func BenchmarkApplySyncMessage_Step1(b *testing.B) {
	// docA is the "remote" peer with ~100 items of content.
	docA := buildDoc(1, 100)
	// docB is the local peer; it is empty so it always requests everything.
	docB := buildDoc(2, 0)

	// Pre-build the step-1 message from docB (the sender).
	step1 := sync.EncodeSyncStep1(docB)

	b.ResetTimer()
	b.ReportAllocs()

	for i := 0; i < b.N; i++ {
		// docA receives step-1 and generates a step-2 reply.
		reply, err := sync.ApplySyncMessage(docA, step1, nil)
		if err != nil || reply == nil {
			b.Fatal("unexpected error or nil reply")
		}
	}
}

// BenchmarkApplySyncMessage_Update measures applying a MsgUpdate that wraps a
// 1000-character document update.
func BenchmarkApplySyncMessage_Update(b *testing.B) {
	const contentLen = 1000

	// Build a source doc and capture its full state as a raw V1 update.
	srcDoc := buildDoc(1, contentLen)
	rawUpdate := crdt.EncodeStateAsUpdateV1(srcDoc, nil)
	updateMsg := sync.EncodeUpdate(rawUpdate)

	b.ResetTimer()
	b.ReportAllocs()

	for i := 0; i < b.N; i++ {
		// Each iteration applies the update to a fresh empty doc so the
		// integration work is representative.
		dest := crdt.New(crdt.WithClientID(2))
		_, err := sync.ApplySyncMessage(dest, updateMsg, nil)
		if err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkFullHandshake measures the complete two-peer handshake:
// A sends step-1 to B, B replies with step-2, A applies step-2.
func BenchmarkFullHandshake(b *testing.B) {
	// Pre-build fixture docs outside the measured loop.
	// docA has content; docB starts empty.
	// We'll reset docB each iteration by re-creating it, but pre-encode
	// step-1 from a template and recreate docs per iteration.
	//
	// To keep allocation noise low we re-build docB inside the loop (it is
	// empty, so creation is cheap) and reuse docA (its step-2 generation is
	// stateless/idempotent for the same state vector from an empty docB).

	// A permanent "full" docA and a step-1 from an empty peer so step-2 from
	// A always carries all content.
	docA := buildDoc(1, 100)
	emptyPeerStep1 := sync.EncodeSyncStep1(crdt.New(crdt.WithClientID(2)))

	b.ResetTimer()
	b.ReportAllocs()

	for i := 0; i < b.N; i++ {
		// B sends its (empty) state vector to A.
		step1B := emptyPeerStep1

		// A generates the step-2 reply containing all its content.
		step2A, err := sync.EncodeSyncStep2(docA, step1B)
		if err != nil {
			b.Fatal(err)
		}

		// A fresh empty docB applies the step-2 to complete the handshake.
		docB := crdt.New(crdt.WithClientID(2))
		_, err = sync.ApplySyncMessage(docB, step2A, nil)
		if err != nil {
			b.Fatal(err)
		}
	}
}
