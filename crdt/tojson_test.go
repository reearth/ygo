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
	w := &jsonWriter{}
	w.encoder = json.NewEncoder(&w.full)
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
