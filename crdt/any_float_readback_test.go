package crdt

import (
	"math"
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Each value below goes over the wire as Any tag 124: a Go integer because
// WriteAny writes it as lib0 writes the same Number, a Go float64 because
// WriteAny narrows it, and a JavaScript peer writes the same bytes for either.
// A float32 holds each one exactly, but the shortest digits that identify the
// float32 name another number (1099511600000 for 2^40), so a replica reads it
// back as a float64 and prints what the writer and Yjs print. json is
// JSON.stringify's output for the value, from Node.
var tag124Values = []struct {
	name string
	v    any
	f    float64
	json string
}{
	{"int64 2^40", int64(1 << 40), 1 << 40, "1099511627776"},
	{"int64 -2^31", int64(math.MinInt32), math.MinInt32, "-2147483648"},
	{"int64 ms timestamp", int64(1718999973888), 1718999973888, "1718999973888"},
	{"float64 2^40", float64(1 << 40), 1 << 40, "1099511627776"},
	{"float64 -2^31", float64(math.MinInt32), math.MinInt32, "-2147483648"},
	{"float64 ms timestamp", float64(1718999973888), 1718999973888, "1718999973888"},
	{"float64 1.5", 1.5, 1.5, "1.5"},
}

var tag124UpdateVersions = []struct {
	name   string
	encode func(*Doc, StateVector) []byte
	apply  func(*Doc, []byte, any) error
}{
	{"V1", EncodeStateAsUpdateV1, ApplyUpdateV1},
	{"V2", EncodeStateAsUpdateV2, ApplyUpdateV2},
}

// One document writes the value, a second applies its update. The second
// reads it back as a float64, at the top level and inside plain maps and
// arrays, and both print the same JSON.
func TestUnit_Tag124_ReplicaReadsFloat64AndPrintsTheWrittenNumber(t *testing.T) {
	for _, c := range tag124Values {
		for _, version := range tag124UpdateVersions {
			t.Run(c.name+"/"+version.name, func(t *testing.T) {
				writer := newTestDoc(1)
				m, arr := writer.GetMap("m"), writer.GetArray("a")
				writer.Transact(func(txn *Transaction) {
					m.Set(txn, "v", c.v)
					m.Set(txn, "plain", map[string]any{"v": c.v, "list": []any{c.v}})
					arr.Push(txn, []any{c.v, []any{c.v}})
				})
				replica := newTestDoc(2)
				require.NoError(t, version.apply(replica, version.encode(writer, nil), nil))

				got, ok := replica.GetMap("m").Get("v")
				require.True(t, ok)
				assert.IsType(t, float64(0), got)
				assert.InDelta(t, c.f, got, 0)
				plain, ok := replica.GetMap("m").Get("plain")
				require.True(t, ok)
				assert.Equal(t, map[string]any{"v": c.f, "list": []any{c.f}}, plain)
				assert.Equal(t, []any{c.f, []any{c.f}}, replica.GetArray("a").ToSlice())

				wantMap := `{"plain":{"list":[` + c.json + `],"v":` + c.json + `},"v":` + c.json + `}`
				wantArray := `[` + c.json + `,[` + c.json + `]]`
				for name, doc := range map[string]*Doc{"writer": writer, "replica": replica} {
					js, err := doc.GetMap("m").ToJSON()
					require.NoError(t, err)
					assert.Equal(t, wantMap, string(js), "%s YMap.ToJSON", name)
					js, err = doc.GetArray("a").ToJSON()
					require.NoError(t, err)
					assert.Equal(t, wantArray, string(js), "%s YArray.ToJSON", name)
				}
			})
		}
	}
}

// V2 carries a text attribute or embed as lib0 Any, V1 as JSON text. A
// document that receives V2 and re-encodes as V1 passes on the value written.
func TestUnit_Tag124_TextValuesV2ThenV1KeepTheWrittenNumber(t *testing.T) {
	for _, c := range tag124Values {
		t.Run(c.name, func(t *testing.T) {
			writer := newTestDoc(1)
			txt := writer.GetText("t")
			writer.Transact(func(txn *Transaction) {
				txt.Insert(txn, 0, "ab", Attributes{"ts": c.v})
				txt.InsertEmbed(txn, 2, map[string]any{"w": c.v}, nil)
			})
			relay := newTestDoc(2)
			require.NoError(t, ApplyUpdateV2(relay, EncodeStateAsUpdateV2(writer, nil), nil))
			reader := newTestDoc(3)
			require.NoError(t, ApplyUpdateV1(reader, EncodeStateAsUpdateV1(relay, nil), nil))

			want := []Delta{
				{Op: DeltaOpInsert, Insert: "ab", Attributes: Attributes{"ts": c.f}},
				{Op: DeltaOpInsert, Insert: map[string]any{"w": c.f}},
			}
			assert.Equal(t, want, relay.GetText("t").ToDelta(), "V2 relay")
			assert.Equal(t, want, reader.GetText("t").ToDelta(), "V1 reader")
		})
	}
}

// A replica that formats text with a value it already holds from a V2 update
// adds nothing, as in Yjs: it holds the writer's float64, not a float32.
func TestUnit_Tag124_FormatWithTheHeldValueAddsNothing(t *testing.T) {
	writer := newTestDoc(1)
	txt := writer.GetText("t")
	writer.Transact(func(txn *Transaction) { txt.Insert(txn, 0, "abcd", Attributes{"size": 1.5}) })
	replica := newTestDoc(2)
	require.NoError(t, ApplyUpdateV2(replica, EncodeStateAsUpdateV2(writer, nil), nil))

	before := EncodeStateVectorV1(replica)
	rt := replica.GetText("t")
	replica.Transact(func(txn *Transaction) { rt.Format(txn, 1, 2, Attributes{"size": 1.5}) })
	assert.Equal(t, before, EncodeStateVectorV1(replica), "Format added items")
	assert.Equal(t, []Delta{{Op: DeltaOpInsert, Insert: "abcd", Attributes: Attributes{"size": 1.5}}}, rt.ToDelta())
}

// An XML attribute's string rendering names the number written.
func TestUnit_Tag124_XmlAttributeShowsTheWrittenNumber(t *testing.T) {
	for _, c := range tag124Values {
		for _, version := range tag124UpdateVersions {
			t.Run(c.name+"/"+version.name, func(t *testing.T) {
				writer := newTestDoc(1)
				frag := writer.GetXmlFragment("x")
				writer.Transact(func(txn *Transaction) {
					el := NewYXmlElement("e")
					el.SetAttributeValue(txn, "v", c.v)
					frag.InsertElement(txn, 0, el)
				})
				replica := newTestDoc(2)
				require.NoError(t, version.apply(replica, version.encode(writer, nil), nil))

				kids := replica.GetXmlFragment("x").Children()
				require.Len(t, kids, 1)
				el := kids[0].(*YXmlElement)
				got, ok := el.GetAttribute("v")
				require.True(t, ok)
				n, err := strconv.ParseFloat(got, 64)
				require.NoError(t, err)
				assert.InDelta(t, c.f, n, 0, "GetAttribute = %s", got)
				assert.Equal(t, map[string]string{"v": got}, el.GetAttributes())
				assert.Equal(t, `<e v="`+got+`"></e>`, replica.GetXmlFragment("x").ToXML())
			})
		}
	}
}
