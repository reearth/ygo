// Package benchmarks contains the B4 editing-trace benchmarks for ygo.
//
// The B4 suite applies a real-world collaborative-editing trace (260 k
// single-character operations recorded from a live document) to measure
// end-to-end CRDT performance across insert, encode, and decode paths.
//
// # Using the real B4 trace
//
// By default the benchmarks run against a synthetic trace (sequential
// single-character inserts that produce a document of similar size). To run
// against the authentic B4 dataset:
//
//  1. Download editing-trace.json from
//     https://github.com/dmonad/crdt-benchmarks/blob/master/benchmarks/editing-trace.json
//  2. Place it at benchmarks/testdata/editing-trace.json
//  3. Re-run: go test -bench=BenchmarkB4 -benchmem ./benchmarks/
//
// # Targets (see ROADMAP.md Phase 8)
//
//	B4 Apply    < 2 s
//	B4 Encode V1 < 200 ms
//	B4 Encode V2 < 300 ms
//	B4 Decode   < 200 ms
package benchmarks

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/reearth/ygo/crdt"
)

// ── Trace loading ─────────────────────────────────────────────────────────────

// traceOp is one operation from the B4 editing trace.
// The real trace encodes each op as [position, deleteCount, insertText].
type traceOp struct {
	Pos    int
	Delete int
	Insert string
}

// loadTrace returns the B4 editing trace. If the real trace file is present at
// benchmarks/testdata/editing-trace.json it is loaded; otherwise a synthetic
// trace of the same approximate size is generated.
func loadTrace(b *testing.B) []traceOp {
	b.Helper()
	path := filepath.Join("testdata", "editing-trace.json")
	data, err := os.ReadFile(path)
	if err == nil {
		// Real B4 trace: JSON array of [pos, deleteCount, insertText] triples.
		var raw [][]any
		if jsonErr := json.Unmarshal(data, &raw); jsonErr == nil {
			ops := make([]traceOp, 0, len(raw))
			for _, r := range raw {
				if len(r) != 3 {
					continue
				}
				pos, _ := r[0].(float64)
				del, _ := r[1].(float64)
				ins, _ := r[2].(string)
				ops = append(ops, traceOp{Pos: int(pos), Delete: int(del), Insert: ins})
			}
			b.Logf("loaded real B4 trace: %d ops", len(ops))
			return ops
		}
	}
	// Synthetic trace: 260 000 single-character sequential inserts followed by
	// a burst of deletes — representative of end-to-end typing workload.
	const totalOps = 260_000
	ops := make([]traceOp, 0, totalOps)
	for i := 0; i < totalOps-1000; i++ {
		ops = append(ops, traceOp{Pos: i, Insert: "a"})
	}
	for i := 0; i < 1000; i++ {
		ops = append(ops, traceOp{Pos: totalOps - 2 - i, Delete: 1})
	}
	b.Logf("using synthetic B4-equivalent trace: %d ops", len(ops))
	return ops
}

// applyTrace applies ops to the text type "content" inside doc.
func applyTrace(doc *crdt.Doc, ops []traceOp) {
	txt := doc.GetText("content")
	for _, op := range ops {
		doc.Transact(func(txn *crdt.Transaction) {
			if op.Delete > 0 {
				txt.Delete(txn, op.Pos, op.Delete)
			}
			if op.Insert != "" {
				txt.Insert(txn, op.Pos, op.Insert, nil)
			}
		})
	}
}

// ── Benchmarks ────────────────────────────────────────────────────────────────

// BenchmarkB4_Apply measures the time to apply the full B4 editing trace from
// scratch (empty document → final document state).
//
// Target: < 2 s for 260 k ops.
func BenchmarkB4_Apply(b *testing.B) {
	ops := loadTrace(b)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		doc := crdt.New()
		applyTrace(doc, ops)
	}
}

// BenchmarkB4_Encode encodes the final B4 document state as a V1 binary update.
//
// Target: < 200 ms.
func BenchmarkB4_Encode(b *testing.B) {
	ops := loadTrace(b)
	doc := crdt.New()
	applyTrace(doc, ops)

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = crdt.EncodeStateAsUpdateV1(doc, nil)
	}
}

// BenchmarkB4_EncodeV2 encodes the final B4 document state as a V2 binary update.
//
// Target: < 300 ms (V2 trades encoding time for a smaller payload).
func BenchmarkB4_EncodeV2(b *testing.B) {
	ops := loadTrace(b)
	doc := crdt.New()
	applyTrace(doc, ops)

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = crdt.EncodeStateAsUpdateV2(doc, nil)
	}
}

// BenchmarkB4_Decode measures how long it takes to decode and apply the full
// V1 binary update of the B4 document to a fresh document.
//
// Target: < 200 ms.
func BenchmarkB4_Decode(b *testing.B) {
	ops := loadTrace(b)
	doc := crdt.New()
	applyTrace(doc, ops)
	update := crdt.EncodeStateAsUpdateV1(doc, nil)

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		fresh := crdt.New()
		if err := crdt.ApplyUpdateV1(fresh, update, nil); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkB4_EncodeV1_Size and BenchmarkB4_EncodeV2_Size log the payload sizes
// so that "V2 is measurably smaller" can be verified from the bench output.
func BenchmarkB4_Size(b *testing.B) {
	ops := loadTrace(b)
	doc := crdt.New()
	applyTrace(doc, ops)

	v1 := crdt.EncodeStateAsUpdateV1(doc, nil)
	v2 := crdt.EncodeStateAsUpdateV2(doc, nil)
	b.ReportMetric(float64(len(v1)), "v1_bytes")
	b.ReportMetric(float64(len(v2)), "v2_bytes")
	b.ReportMetric(float64(len(v2))*100/float64(len(v1)), "v2_pct_of_v1")
}
