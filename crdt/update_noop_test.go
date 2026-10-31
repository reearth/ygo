package crdt

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// noOpFixture is a document holding ["y"] after "x" was deleted, and the V1
// updates that built it.
type noOpFixture struct {
	insert, delete, full []byte
}

func newNoOpFixture(t *testing.T) noOpFixture {
	t.Helper()
	src := newTestDoc(1)
	arr := src.GetArray("a")
	var f noOpFixture
	src.Transact(func(txn *Transaction) { arr.Push(txn, []any{"x", "y"}) })
	f.insert = EncodeStateAsUpdateV1(src, nil)
	unsub := src.OnUpdate(func(u []byte, _ any) { f.delete = u })
	src.Transact(func(txn *Transaction) { arr.Delete(txn, 0, 1) })
	unsub()
	f.full = EncodeStateAsUpdateV1(src, nil)
	return f
}

// countUpdates applies the V1 update in format f and returns how many times
// OnUpdate fired.
func countUpdates(t *testing.T, doc *Doc, f skipGapFormat, update []byte) (int, error) {
	t.Helper()
	n := 0
	unsub := doc.OnUpdate(func([]byte, any) { n++ })
	defer unsub()
	u, err := f.conv(update)
	require.NoError(t, err)
	err = f.apply(doc, u, "remote")
	return n, err
}

func TestUnit_Doc_OnUpdate_ApplyThatChangesNothingFiresNothing(t *testing.T) {
	f := newNoOpFixture(t)
	for _, format := range skipGapFormats {
		for _, tc := range []struct {
			name   string
			update []byte
		}{
			{"re-sent insert", f.insert},
			{"re-sent delete", f.delete},
			{"re-sent full state", f.full},
			{"delete of clocks the document lacks", deleteOnlyUpdateV1(9, DeleteRange{Clock: 0, Len: 1})},
		} {
			t.Run(tc.name+" "+format.name, func(t *testing.T) {
				doc := newTestDoc(2)
				require.NoError(t, ApplyUpdateV1(doc, f.full, "remote"))
				for range 2 {
					n, err := countUpdates(t, doc, format, tc.update)
					require.NoError(t, err)
					assert.Zero(t, n, "Yjs emits no update event for an apply that changes nothing")
				}
				assert.Equal(t, []any{"y"}, doc.GetArray("a").ToSlice())
			})
		}
	}
}

func TestUnit_Doc_OnUpdate_ApplyThatChangesSomethingFires(t *testing.T) {
	f := newNoOpFixture(t)
	for _, format := range skipGapFormats {
		for _, tc := range []struct {
			name           string
			before, update []byte
		}{
			{"insert", nil, f.insert},
			{"delete only", f.insert, f.delete},
		} {
			t.Run(tc.name+" "+format.name, func(t *testing.T) {
				doc := newTestDoc(2)
				if tc.before != nil {
					require.NoError(t, ApplyUpdateV1(doc, tc.before, "remote"))
				}
				n, err := countUpdates(t, doc, format, tc.update)
				require.NoError(t, err)
				assert.Equal(t, 1, n)
			})
		}
	}
}

func TestUnit_Doc_OnUpdate_ParkedDeleteFiresWhenItApplies(t *testing.T) {
	f := newNoOpFixture(t)
	for _, format := range skipGapFormats {
		t.Run(format.name, func(t *testing.T) {
			doc := newTestDoc(2)
			n, err := countUpdates(t, doc, format, f.delete)
			require.NoError(t, err)
			assert.Zero(t, n, "a parked delete changes nothing yet")
			n, err = countUpdates(t, doc, format, f.insert)
			require.NoError(t, err)
			assert.Equal(t, 1, n)
			assert.Equal(t, []any{"y"}, doc.GetArray("a").ToSlice())
		})
	}
}

func TestUnit_Doc_OnUpdate_RefusedApply(t *testing.T) {
	f := newNoOpFixture(t)
	// full minus its last byte: the structs decode and integrate, and the
	// delete set does not decode.
	structsThenBadDeleteSet := f.full[:len(f.full)-1]
	for _, tc := range []struct {
		name   string
		update []byte
		want   int
	}{
		{"nothing integrated", f.full[:1], 0},
		{"structs integrated", structsThenBadDeleteSet, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			doc := newTestDoc(2)
			n, err := countUpdates(t, doc, skipGapFormats[0], tc.update)
			require.Error(t, err)
			assert.Equal(t, tc.want, n, "Yjs emits the update event only for what the apply changed")
		})
	}
}

func TestUnit_Doc_OnUpdate_TransactionThatChangesNothingFiresNothing(t *testing.T) {
	for _, tc := range []struct {
		name string
		run  func(doc *Doc)
	}{
		{"empty", func(doc *Doc) { doc.Transact(func(*Transaction) {}) }},
		{"read only", func(doc *Doc) {
			arr := doc.GetArray("a")
			doc.Transact(func(*Transaction) { _ = arr.Len() })
		}},
		{"returns an error", func(doc *Doc) {
			_ = doc.TransactE(func(*Transaction) error { return assert.AnError })
		}},
		{"panics", func(doc *Doc) {
			defer func() { _ = recover() }()
			doc.Transact(func(*Transaction) { panic("before any change") })
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			doc := newTestDoc(1)
			updates, afters := 0, 0
			doc.OnUpdate(func([]byte, any) { updates++ })
			doc.OnAfterTransaction(func(*Transaction) { afters++ })
			tc.run(doc)
			assert.Zero(t, updates, "Yjs emits no update event for a transaction that changes nothing")
			assert.Equal(t, 1, afters, "Yjs emits afterTransaction for every transaction")
		})
	}
}
