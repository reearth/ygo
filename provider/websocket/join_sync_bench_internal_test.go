package websocket

import (
	"fmt"
	"strings"
	"testing"

	"github.com/reearth/ygo/crdt"
	"github.com/reearth/ygo/encoding"
	ygsync "github.com/reearth/ygo/sync"
)

// BenchmarkJoinAnswer_FragmentedRoom measures the server's answer to one
// joiner's SyncStep1 in a room of 20,000 characters typed into the middle,
// one struct each; see benchmarkJoinAnswer.
func BenchmarkJoinAnswer_FragmentedRoom(b *testing.B) {
	// A fixed client ID, so that every run answers with the same bytes: a
	// random one is written in four or five bytes in each origin a struct
	// names, which made the state about 307 KB or about 347 KB by chance.
	doc := crdt.New(crdt.WithClientID(3_000_000_000))
	txt := doc.GetText("t")
	for i := range 20000 {
		doc.Transact(func(txn *crdt.Transaction) { txt.Insert(txn, i/2, "a", nil) })
	}
	benchmarkJoinAnswer(b, doc)
}

// BenchmarkJoinAnswer_MixedRoom measures the same answer in a room of 16,000
// small structs of one client, as a server holds them once that client sent
// them: 1,000 XML paragraphs, each with attributes, one written over, and a
// run of text partly in bold, then 3,000 map entries of small values, each
// written twice.
func BenchmarkJoinAnswer_MixedRoom(b *testing.B) {
	writer := crdt.New(crdt.WithClientID(3_000_000_001))
	frag, meta := writer.GetXmlFragment("prosemirror"), writer.GetMap("meta")
	for i := range 1000 {
		writer.Transact(func(txn *crdt.Transaction) {
			p := crdt.NewYXmlElement("paragraph")
			frag.InsertElement(txn, i, p)
			p.SetAttribute(txn, "align", "left")
			p.SetAttributeValue(txn, "indent", i%4)
			p.SetAttribute(txn, "align", "center")
			text := crdt.NewYXmlText()
			p.InsertText(txn, 0, text)
			text.Insert(txn, 0, strings.Repeat("w", 12), nil)
			text.Insert(txn, 12, strings.Repeat("x", 8), crdt.Attributes{"bold": true})
			text.Insert(txn, 20, strings.Repeat("y", 10), nil)
		})
	}
	for round := range 2 {
		writer.Transact(func(txn *crdt.Transaction) {
			for k := range 3000 {
				switch k % 3 {
				case 0:
					meta.Set(txn, fmt.Sprintf("n%d", k), k*(round+1))
				case 1:
					meta.Set(txn, fmt.Sprintf("s%d", k), fmt.Sprintf("v%d-%d", k, round))
				default:
					meta.Set(txn, fmt.Sprintf("b%d", k), round == 1)
				}
			}
		})
	}
	doc := crdt.New(crdt.WithClientID(1))
	if err := crdt.ApplyUpdateV1(doc, crdt.EncodeStateAsUpdateV1(writer, nil), nil); err != nil {
		b.Fatal(err)
	}
	benchmarkJoinAnswer(b, doc)
}

// benchmarkJoinAnswer measures the server's answer to one joiner's SyncStep1
// in a room holding doc: the peer handling the frame and queueing the frames
// that answer it, with no connection and no client, whose decode and apply
// are not the server's to pay. It reports the frames queued and their bytes
// per answer.
//
// It reaches nothing newer than the peer's message handler, so it runs as it
// is at an earlier commit, and benchstat can compare the two runs.
func benchmarkJoinAnswer(b *testing.B, doc *crdt.Doc) {
	step1 := syncFrame(ygsync.EncodeSyncStep1(crdt.New()))
	for _, framing := range []struct {
		name       string
		hocuspocus bool
	}{{"y-websocket", false}, {"hocuspocus", true}} {
		b.Run(framing.name, func(b *testing.B) {
			p := &peer{
				server:            &Server{},
				room:              &room{doc: doc},
				roomName:          "room",
				hocuspocusFraming: framing.hocuspocus,
				writeCh:           make(chan []byte, 64),
			}
			msg := step1
			if framing.hocuspocus {
				msg = encoding.EncodeBytes(func(enc *encoding.Encoder) {
					enc.WriteVarString(p.roomName)
					enc.WriteRaw(step1)
				})
			}

			var frames, bytes int
			b.ReportAllocs()
			b.ResetTimer()
			for range b.N {
				p.handleMessage(msg)
				for len(p.writeCh) > 0 {
					frames++
					bytes += len(<-p.writeCh)
				}
			}
			if frames == 0 {
				b.Fatal("the SyncStep1 was not answered")
			}
			b.ReportMetric(float64(frames)/float64(b.N), "frames/join")
			b.ReportMetric(float64(bytes)/float64(b.N), "frame-B/join")
		})
	}
}
