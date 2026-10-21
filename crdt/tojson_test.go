package crdt

import (
	"encoding/json"
	"errors"
	"math"
	"math/rand/v2"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// jsonTestMarshaler returns JSON with spacing that encoding/json compacts.
type jsonTestMarshaler struct{ label string }

func (m jsonTestMarshaler) MarshalJSON() ([]byte, error) {
	return []byte(`{ "label" : "` + m.label + `" }`), nil
}

type failingJSONMarshaler struct{}

func (failingJSONMarshaler) MarshalJSON() ([]byte, error) {
	return nil, errors.New("refused")
}

// assertToJSONMatchesEncodingJSON checks ToJSON against json.Marshal of the
// same container's ToSlice or Entries, the output ToJSON had before it stopped
// recursing. The comparison is byte for byte rather than JSONEq: key order and
// number formatting are part of what is checked.
func assertToJSONMatchesEncodingJSON(t *testing.T, got []byte, gotErr error, value any) {
	t.Helper()
	want, wantErr := json.Marshal(value)
	if wantErr != nil {
		require.EqualError(t, gotErr, wantErr.Error())
		return
	}
	require.NoError(t, gotErr)
	assert.Equal(t, string(want), string(got))
}

func TestUnit_ToJSON_ValuesMatchEncodingJSON(t *testing.T) {
	for _, c := range []struct {
		name  string
		value any
	}{
		{"nil", nil},
		{"true", true},
		{"false", false},
		{"string", "quote\" slash\\ \b\f\n\r\t\x01\x1f <tag> & \u2028\u2029 é 😀"},
		{"int", -42},
		{"int8", int8(-128)},
		{"int16", int16(-32768)},
		{"int32", int32(math.MinInt32)},
		{"int64", int64(math.MinInt64)},
		{"uint", uint(42)},
		{"uint8", uint8(255)},
		{"uint16", uint16(65535)},
		{"uint32", uint32(math.MaxUint32)},
		{"uint64", uint64(math.MaxUint64)},
		{"float64", 1234.5678},
		{"float64 negative zero", math.Copysign(0, -1)},
		{"float64 exponent", 1e-7},
		{"float32", float32(0.1)},
		{"float32 exponent", float32(1e21)},
		{"bytes", []byte("raw <bytes>")},
		{"json.Number", json.Number("12.50")},
		{"object", map[string]any{"b": 1.5, "a": []any{"x", nil, true}}},
		{"array", []any{1, "two", map[string]any{"three": 3.0}}},
		{"marshaler", jsonTestMarshaler{label: "<m>"}},
		{"time", time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)},
		{"NaN", math.NaN()},
		{"+Inf", math.Inf(1)},
		{"float32 -Inf", float32(math.Inf(-1))},
		{"failing marshaler", failingJSONMarshaler{}},
	} {
		t.Run(c.name, func(t *testing.T) {
			doc := newTestDoc(1)
			arr := doc.GetArray("a")
			m := doc.GetMap("m")
			doc.Transact(func(txn *Transaction) {
				arr.Push(txn, []any{"before", c.value, "after"})
				m.Set(txn, "a", "before")
				m.Set(txn, "v", c.value)
				m.Set(txn, "z", "after")
			})

			got, err := arr.ToJSON()
			assertToJSONMatchesEncodingJSON(t, got, err, arr.ToSlice())
			got, err = m.ToJSON()
			assertToJSONMatchesEncodingJSON(t, got, err, m.Entries())
		})
	}
}

func TestUnit_ToJSON_FloatsMatchEncodingJSON(t *testing.T) {
	values := []any{
		0.0, 1e-6, 9.999999999999999e-7, 1e20, 1e21, 999999999999999900000.0,
		0.1, -2.5, math.MaxFloat64, math.SmallestNonzeroFloat64,
		float32(1e-6), float32(9.99e-7), float32(1e20), float32(1e21),
		float32(math.MaxFloat32), float32(math.SmallestNonzeroFloat32),
	}
	rng := rand.New(rand.NewPCG(1, 2))
	for len(values) < 8000 {
		if f := math.Float64frombits(rng.Uint64()); !math.IsNaN(f) && !math.IsInf(f, 0) {
			values = append(values, f)
		}
		if f := math.Float32frombits(rng.Uint32()); !math.IsNaN(float64(f)) && !math.IsInf(float64(f), 0) {
			values = append(values, f)
		}
		values = append(values, rng.NormFloat64()*1e6, float32(rng.NormFloat64()))
	}

	doc := newTestDoc(1)
	arr := doc.GetArray("a")
	doc.Transact(func(txn *Transaction) { arr.Push(txn, values) })
	got, err := arr.ToJSON()
	assertToJSONMatchesEncodingJSON(t, got, err, arr.ToSlice())
}

// Neighbouring values that encoding/json marshals are encoded as one run;
// values around nested types and fast-path scalars end a run.
func TestUnit_YArray_ToJSON_DeferredRunsMatchEncodingJSON(t *testing.T) {
	for _, c := range []struct {
		name   string
		values []any
	}{
		{"runs", []any{
			map[string]any{"a": 1}, []byte("b"), json.Number("3"), "s",
			jsonTestMarshaler{label: "x"}, []any{"y"}, 1.5, map[string]any{},
		}},
		{"error inside a run", []any{"s", []any{1}, math.NaN(), map[string]any{}}},
		{"failing marshaler inside a run", []any{[]any{1}, []any{2}, failingJSONMarshaler{}}},
	} {
		t.Run(c.name, func(t *testing.T) {
			doc := newTestDoc(1)
			arr := doc.GetArray("a")
			doc.Transact(func(txn *Transaction) {
				arr.Push(txn, c.values)
				nested := NewMapPrelim()
				arr.PushType(txn, nested)
				nested.Set(txn, "k", []any{"v"})
				arr.Push(txn, c.values)
			})
			got, err := arr.ToJSON()
			assertToJSONMatchesEncodingJSON(t, got, err, arr.ToSlice())
		})
	}
}

func TestUnit_AppendJSONString_MatchesEncodingJSON(t *testing.T) {
	ascii := make([]byte, 0x80)
	for i := range ascii {
		ascii[i] = byte(i)
	}
	for _, input := range []string{"", "plain", string(ascii), "\u2028\u2029 é 😀 \u007f"} {
		want, err := json.Marshal(input)
		require.NoError(t, err)
		got, ok := appendJSONString(nil, input)
		require.True(t, ok)
		assert.Equal(t, string(want), string(got))
	}

	// Invalid UTF-8 goes to encoding/json.
	_, ok := appendJSONString(nil, "a\xffb")
	assert.False(t, ok)
	w := newJSONWriter()
	w.writeString("a\xffb")
	got, err := w.finish()
	require.NoError(t, err)
	want, err := json.Marshal("a\xffb")
	require.NoError(t, err)
	assert.Equal(t, string(want), string(got))
}

// encoding/json writes map keys in bytewise order, which ToJSON must keep now
// that it writes objects itself. The insertion order below matches no sort.
// The comparisons are on exact bytes: JSONEq ignores key order.
func TestUnit_YMap_ToJSON_KeysInEncodingJSONOrder(t *testing.T) {
	keys := []string{"é", "b", "aa", "B", "a", "c"}
	const want = `{"B":"B","a":"a","aa":"aa","b":"b","c":"c","nested":{"x":"x","y":"y"},"é":"é"}`
	check := func(name string, got []byte, err error) {
		t.Helper()
		require.NoError(t, err)
		if string(got) != want {
			t.Errorf("%s = %s, want %s", name, got, want)
		}
	}

	doc := newTestDoc(1)
	m := doc.GetMap("m")
	doc.Transact(func(txn *Transaction) {
		for _, key := range keys {
			m.Set(txn, key, key)
		}
		nested := NewMapPrelim()
		m.Set(txn, "nested", nested)
		nested.Set(txn, "y", "y")
		nested.Set(txn, "x", "x")
	})
	got, err := m.ToJSON()
	check("ToJSON", got, err)
	got, err = json.Marshal(m.Entries())
	check("json.Marshal(Entries())", got, err)

	detached := NewMapPrelim()
	for _, key := range keys {
		detached.Set(nil, key, key)
	}
	nested := NewMapPrelim()
	nested.Set(nil, "y", "y")
	nested.Set(nil, "x", "x")
	detached.Set(nil, "nested", nested)
	got, err = detached.ToJSON()
	check("detached ToJSON", got, err)
}

// Moves render a target at the move's position, nested types included.
func TestUnit_YArray_ToJSON_NestedAndMovedMatchesEncodingJSON(t *testing.T) {
	doc := newTestDoc(1)
	root := doc.GetMap("root")
	arr := NewArrayPrelim()
	doc.Transact(func(txn *Transaction) {
		root.Set(txn, "list", arr)
		arr.Push(txn, []any{1.5, "two", nil})
		m := NewMapPrelim()
		arr.PushType(txn, m)
		m.Set(txn, "k", []any{float32(0.25)})
		inner := NewArrayPrelim()
		m.Set(txn, "inner", inner)
		inner.Push(txn, []any{true, 3})
		txt := NewTextPrelim()
		arr.PushType(txn, txt)
		txt.Insert(txn, 0, "hi <b>", nil)
		arr.Push(txn, []any{map[string]any{"z": 1, "a": 2}})
	})
	before := arr.ToSlice()
	doc.Transact(func(txn *Transaction) {
		arr.Move(txn, 3, 0) // the nested map
		arr.Move(txn, 2, 6)
	})
	require.NotEqual(t, before, arr.ToSlice())

	got, err := arr.ToJSON()
	assertToJSONMatchesEncodingJSON(t, got, err, arr.ToSlice())
	got, err = root.ToJSON()
	assertToJSONMatchesEncodingJSON(t, got, err, root.Entries())
}

// docWritingMarshaler writes to its document from MarshalJSON, which
// deadlocks if ToJSON calls it while holding the document's read lock.
type docWritingMarshaler struct {
	doc  *Doc
	side *YMap
}

func (v docWritingMarshaler) MarshalJSON() ([]byte, error) {
	v.doc.Transact(func(txn *Transaction) { v.side.Set(txn, "marshalled", true) })
	return []byte(`"ok"`), nil
}

func TestUnit_ToJSON_MarshalJSONRunsAfterUnlock(t *testing.T) {
	for _, c := range []struct {
		name  string
		store func(doc *Doc, value any) func() ([]byte, error)
		want  string
	}{
		{"YArray", func(doc *Doc, value any) func() ([]byte, error) {
			arr := doc.GetArray("a")
			doc.Transact(func(txn *Transaction) { arr.Push(txn, []any{value}) })
			return arr.ToJSON
		}, `["ok"]`},
		{"YMap", func(doc *Doc, value any) func() ([]byte, error) {
			m := doc.GetMap("m")
			doc.Transact(func(txn *Transaction) { m.Set(txn, "v", value) })
			return m.ToJSON
		}, `{"v":"ok"}`},
	} {
		t.Run(c.name, func(t *testing.T) {
			doc := newTestDoc(1)
			side := doc.GetMap("side")
			toJSON := c.store(doc, docWritingMarshaler{doc: doc, side: side})

			type result struct {
				out []byte
				err error
			}
			done := make(chan result, 1)
			go func() {
				out, err := toJSON()
				done <- result{out, err}
			}()
			select {
			case r := <-done:
				require.NoError(t, r.err)
				assert.Equal(t, c.want, string(r.out))
			case <-time.After(10 * time.Second):
				t.Fatal("ToJSON did not return: MarshalJSON ran while the document lock was held")
			}
			marshalled, ok := side.Get("marshalled")
			require.True(t, ok)
			assert.Equal(t, true, marshalled)
		})
	}
}

func newTestXmlFragment() *YXmlFragment {
	f := &YXmlFragment{}
	f.owner = f
	f.itemMap = make(map[string]*Item)
	return f
}

// Embeds and nested XML types read the same through ToJSON, ToSlice and
// Entries, in arrays and maps.
func TestUnit_ToJSON_EmbedsAndNestedXMLMatchEncodingJSON(t *testing.T) {
	doc := newTestDoc(1)
	arr := doc.GetArray("a")
	m := doc.GetMap("m")
	embed := map[string]any{"image": "x.png", "w": 2.0}
	doc.Transact(func(txn *Transaction) {
		arr.Push(txn, []any{"before"})
		arr.insertContentAfterItem(txn, arr.start, NewContentEmbed(embed), 1)
		text, fragment := NewYXmlText(), newTestXmlFragment()
		arr.PushType(txn, text)
		text.Insert(txn, 0, "a<b", nil)
		arr.PushType(txn, fragment)
		fragment.InsertElement(txn, 0, NewYXmlElement("e"))

		key := "embed"
		(&Item{
			ID:        ID{Client: doc.clientID, Clock: doc.store.NextClock(doc.clientID)},
			Parent:    m.baseType(),
			ParentSub: &key,
			Content:   NewContentEmbed(embed),
		}).integrate(txn, 0)
		mapText, mapFragment := NewYXmlText(), newTestXmlFragment()
		m.Set(txn, "text", mapText)
		mapText.Insert(txn, 0, "c&d", nil)
		m.Set(txn, "fragment", mapFragment)
		mapFragment.InsertText(txn, 0, NewYXmlText())
	})

	assert.Equal(t, []any{"before", embed, "a&lt;b", "<e></e>"}, arr.ToSlice())
	assert.Equal(t, map[string]any{"embed": embed, "text": "c&amp;d", "fragment": ""}, m.Entries())
	got, err := arr.ToJSON()
	assertToJSONMatchesEncodingJSON(t, got, err, arr.ToSlice())
	got, err = m.ToJSON()
	assertToJSONMatchesEncodingJSON(t, got, err, m.Entries())
}

// Values only encoding/json can encode are marshalled in one call and split
// back out, so their encodings may hold commas, brackets and quotes.
func TestUnit_ToJSON_DeferredValuesSplitBackOut(t *testing.T) {
	tricky := []any{
		map[string]any{"a,b": []any{1, "]", "}"}, "q": `x,"y\`},
		[]any{",", map[string]any{}, []any{}},
		jsonTestMarshaler{label: `,]}`},
		[]byte(",[{"),
	}
	doc := newTestDoc(1)
	arr := doc.GetArray("a")
	m := doc.GetMap("m")
	doc.Transact(func(txn *Transaction) {
		arr.Push(txn, tricky)
		arr.Push(txn, []any{1.5})
		arr.Push(txn, tricky)
		for i, value := range tricky {
			m.Set(txn, string(rune('a'+i)), value)
		}
		nested := NewMapPrelim()
		m.Set(txn, "nested", nested)
		nested.Set(txn, "z", tricky[0])
		nested.Set(txn, "y", tricky[2])
	})
	got, err := arr.ToJSON()
	assertToJSONMatchesEncodingJSON(t, got, err, arr.ToSlice())
	got, err = m.ToJSON()
	assertToJSONMatchesEncodingJSON(t, got, err, m.Entries())

	// The error is the first failing value in key order.
	doc.Transact(func(txn *Transaction) {
		m.Set(txn, "0", failingJSONMarshaler{})
		m.Set(txn, "1", math.NaN())
	})
	got, err = m.ToJSON()
	assertToJSONMatchesEncodingJSON(t, got, err, m.Entries())
}

// randomJSONDoc builds a random tree of nested types and plain values in doc,
// returning the value ToSlice or Entries should read.
type randomJSONDoc struct {
	rng *rand.Rand
}

var randomJSONKeys = []string{"a", "B", "aa", "é", "a,b", `q"`, "", "z", " ", "k1", "k10", "k2"}

func (g randomJSONDoc) leaf() any {
	switch g.rng.IntN(12) {
	case 0:
		return nil
	case 1:
		return g.rng.IntN(2) == 0
	case 2:
		return g.rng.NormFloat64() * 1e6
	case 3:
		return int64(g.rng.IntN(1000) - 500)
	case 4:
		return randomJSONKeys[g.rng.IntN(len(randomJSONKeys))] + "<&>"
	case 5:
		return map[string]any{"x": g.rng.Float64(), "s": ",]"}
	case 6:
		return []any{"y", int64(g.rng.IntN(9)), map[string]any{}}
	case 7:
		return json.Number("12.50")
	case 8:
		return []byte("raw,")
	case 9:
		return jsonTestMarshaler{label: "m,"}
	case 10:
		return float32(g.rng.Float32())
	default:
		return "plain"
	}
}

func (g randomJSONDoc) fillArray(txn *Transaction, a *YArray, depth int) []any {
	want := []any{}
	for range g.rng.IntN(6) {
		switch g.rng.IntN(4) {
		case 0, 1:
			run := make([]any, 1+g.rng.IntN(3))
			for i := range run {
				run[i] = g.leaf()
			}
			a.Push(txn, run)
			want = append(want, run...)
		default:
			want = append(want, g.nested(txn, depth, func(st sharedType) { a.PushType(txn, st) }))
		}
	}
	return want
}

func (g randomJSONDoc) fillMap(txn *Transaction, m *YMap, depth int) map[string]any {
	want := map[string]any{}
	for range g.rng.IntN(6) {
		key := randomJSONKeys[g.rng.IntN(len(randomJSONKeys))]
		if g.rng.IntN(2) == 0 {
			value := g.leaf()
			m.Set(txn, key, value)
			want[key] = value
			continue
		}
		want[key] = g.nested(txn, depth, func(st sharedType) { m.Set(txn, key, st) })
	}
	return want
}

// nested attaches a nested type through attach and fills it.
func (g randomJSONDoc) nested(txn *Transaction, depth int, attach func(sharedType)) any {
	kind := g.rng.IntN(3)
	if depth == 0 {
		kind = 2
	}
	switch kind {
	case 0:
		a := NewArrayPrelim()
		attach(a)
		return g.fillArray(txn, a, depth-1)
	case 1:
		m := NewMapPrelim()
		attach(m)
		return g.fillMap(txn, m, depth-1)
	default:
		text := NewTextPrelim()
		attach(text)
		text.Insert(txn, 0, "t,<x>", nil)
		return "t,<x>"
	}
}

func TestUnit_ToJSON_RandomTreesMatchEncodingJSON(t *testing.T) {
	for seed := range uint64(300) {
		g := randomJSONDoc{rng: rand.New(rand.NewPCG(seed, 7))}
		doc := newTestDoc(1)
		arr := doc.GetArray("a")
		m := doc.GetMap("m")
		var wantArray []any
		var wantMap map[string]any
		doc.Transact(func(txn *Transaction) {
			wantArray = g.fillArray(txn, arr, 4)
			wantMap = g.fillMap(txn, m, 4)
		})

		require.Equal(t, wantArray, arr.ToSlice(), "seed %d", seed)
		require.Equal(t, wantMap, m.Entries(), "seed %d", seed)
		got, err := arr.ToJSON()
		assertToJSONMatchesEncodingJSON(t, got, err, wantArray)
		got, err = m.ToJSON()
		assertToJSONMatchesEncodingJSON(t, got, err, wantMap)
	}
}

// Readers hold the read lock while they walk the document; run under -race.
func TestUnit_ToJSON_ConcurrentWithWriter(t *testing.T) {
	doc := newTestDoc(1)
	arr := doc.GetArray("a")
	m := doc.GetMap("m")
	nested := NewMapPrelim()
	doc.Transact(func(txn *Transaction) { m.Set(txn, "nested", nested) })

	stop := make(chan struct{})
	writerDone := make(chan struct{})
	go func() {
		defer close(writerDone)
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			doc.Transact(func(txn *Transaction) {
				arr.Push(txn, []any{i, map[string]any{"i": i}})
				nested.Set(txn, "k", i)
				if arr.Len() > 50 {
					arr.Delete(txn, 0, 10)
				}
			})
		}
	}()

	for range 200 {
		_, err := arr.ToJSON()
		require.NoError(t, err)
		_, err = m.ToJSON()
		require.NoError(t, err)
		_ = arr.ToSlice()
		_ = m.Entries()
	}
	close(stop)
	<-writerDone
}

// A detached map's staged entries read like attached ones, nested types
// included.
func TestUnit_YMap_Entries_Detached(t *testing.T) {
	m := NewMapPrelim()
	inner := NewArrayPrelim()
	inner.Push(nil, []any{1.5, "x"})
	m.Set(nil, "plain", "v")
	m.Set(nil, "inner", inner)
	m.Set(nil, "text", NewTextPrelim())
	assert.Equal(t, map[string]any{"plain": "v", "inner": []any{1.5, "x"}, "text": ""}, m.Entries())
}

// toJSONValue reads a nested type given directly, as text deltas do for an
// embedded type, and nil for a type it cannot read.
func TestUnit_ToJSONValue_LeavesAndUnreadable(t *testing.T) {
	doc := newTestDoc(1)
	text := NewTextPrelim()
	m := doc.GetMap("m")
	doc.Transact(func(txn *Transaction) {
		m.Set(txn, "t", text)
		text.Insert(txn, 0, "hi", nil)
	})
	assert.Equal(t, "hi", toJSONValue(&ContentType{Type: &text.abstractType}))

	raw := &rawType{}
	raw.owner = raw
	assert.Nil(t, toJSONValue(&ContentType{Type: &raw.abstractType}))
	assert.Nil(t, toJSONValue(&ContentType{}))
	assert.Nil(t, toJSONValue(nil))
	assert.Equal(t, nestedJSONEntry{}, nestedJSONEntryForContent(&ContentType{}))

	// Content a map does not render is skipped.
	_, ok := nestedJSONEntryForMapItem(&Item{Content: NewContentFormat("bold", true)})
	assert.False(t, ok)
}

// An item with no values renders nothing between its neighbours.
func TestUnit_JSONArrayCursor_SkipsEmptyItem(t *testing.T) {
	last := &Item{Content: NewContentAny("v")}
	first := &Item{Content: NewContentAny(), Right: last}
	cursor := jsonArrayCursor{t: &abstractType{}, item: first}
	entry, ok := cursor.next()
	require.True(t, ok)
	assert.Equal(t, "v", entry.value)
	_, ok = cursor.next()
	assert.False(t, ok)
}

func TestUnit_AppendJSONScalar_PlainInt(t *testing.T) {
	got, ok := appendJSONScalar(nil, -42)
	require.True(t, ok)
	assert.Equal(t, "-42", string(got))
}

// The splitter returns the whole input when no comma ends the first value.
func TestUnit_NextJSONValueEnd(t *testing.T) {
	for data, want := range map[string]int{
		`"a,b"`:         5,
		`"a\",b",1`:     7,
		`{"k":[1,2]},3`: 11,
		`[[],{}],"x"`:   7,
		`"\\",[1]`:      4,
	} {
		assert.Equal(t, want, nextJSONValueEnd([]byte(data)), data)
	}
}
