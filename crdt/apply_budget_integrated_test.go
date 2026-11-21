package crdt

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Tests for ApplyBudget.Integrated: the callback a budget carries for its one
// apply, called once the apply has integrated and before the commit's first
// observer, with the document lock released, and never for an apply that
// fails or is rolled back.

// integratedRecord is what the observers and the callback of an
// integratedDoc saw.
type integratedRecord struct {
	calls int      // the callback's calls
	text  []string // the text the callback read, at each call
	free  []bool   // whether the document lock was free, at each call
	seen  []string // each observer that ran, with the calls made before it
}

// integratedDoc is a document whose budget, made by budget, carries a
// callback that records into rec, with a text observer, an OnUpdate and an
// OnAfterTransaction observer that each record how many calls the callback
// had had when it ran.
func integratedDoc(t *testing.T, rec *integratedRecord, budget func(integrated func()) ApplyBudget, opts ...DocOption) *Doc {
	t.Helper()
	var doc *Doc
	integrated := func() {
		rec.calls++
		free := doc.mu.TryLock()
		if free {
			doc.mu.Unlock()
		}
		rec.free = append(rec.free, free)
		rec.text = append(rec.text, doc.GetText("t").ToString())
	}
	opts = append([]DocOption{WithClientID(1), WithApplyBudget(func(any) ApplyBudget { return budget(integrated) })}, opts...)
	doc = New(opts...)
	doc.GetText("t").Observe(func(YTextEvent) {
		rec.seen = append(rec.seen, fmt.Sprintf("text observer after %d", rec.calls))
	})
	doc.OnUpdate(func([]byte, any) {
		rec.seen = append(rec.seen, fmt.Sprintf("OnUpdate after %d", rec.calls))
	})
	doc.OnAfterTransaction(func(*Transaction) {
		rec.seen = append(rec.seen, fmt.Sprintf("OnAfterTransaction after %d", rec.calls))
	})
	return doc
}

// withIntegrated is a budget that bounds nothing and carries integrated.
func withIntegrated(integrated func()) ApplyBudget {
	return ApplyBudget{Integrated: integrated}
}

// typedAB is a peer's "ab" in the text "t", as the V1 update a document
// holding nothing takes.
func typedAB() []byte {
	peer := New(WithClientID(2))
	text := peer.GetText("t")
	peer.Transact(func(txn *Transaction) { text.Insert(txn, 0, "ab", nil) })
	return EncodeStateAsUpdateV1(peer, nil)
}

func TestUnit_ApplyBudget_IntegratedRunsOnceBeforeTheFirstObserver(t *testing.T) {
	for _, f := range applyFormats {
		t.Run("an apply that succeeds, "+f.name, func(t *testing.T) {
			var rec integratedRecord
			doc := integratedDoc(t, &rec, withIntegrated)
			require.NoError(t, applyIn(t, doc, f.v2, typedAB()))
			assert.Equal(t, 1, rec.calls, "the callback runs once")
			assert.Equal(t, []string{"ab"}, rec.text, "the callback runs after the update has integrated")
			assert.Equal(t, []bool{true}, rec.free, "the callback runs with the document lock released")
			assert.Equal(t, []string{"text observer after 1", "OnUpdate after 1", "OnAfterTransaction after 1"}, rec.seen,
				"every observer of the commit runs after the callback, in their order")
		})
	}

	t.Run("an apply that changes nothing", func(t *testing.T) {
		var rec integratedRecord
		doc := integratedDoc(t, &rec, withIntegrated)
		update := typedAB()
		require.NoError(t, ApplyUpdateV1(doc, update, "peer"))
		rec.seen = nil
		require.NoError(t, ApplyUpdateV1(doc, update, "peer"))
		assert.Equal(t, 2, rec.calls, "an apply that succeeds calls it, whether or not it changed the document")
		for _, s := range rec.seen {
			assert.Contains(t, s, "after 2", "an observer that runs, runs after the callback")
		}
	})

	t.Run("an apply the budget refuses", func(t *testing.T) {
		var rec integratedRecord
		doc := integratedDoc(t, &rec, func(integrated func()) ApplyBudget {
			return ApplyBudget{Bytes: 1, Integrated: integrated}
		})
		require.ErrorIs(t, ApplyUpdateV1(doc, typedAB(), "peer"), ErrApplyBudget)
		assert.Zero(t, rec.calls, "a refused apply does not call it")
	})

	t.Run("an apply rolled back", func(t *testing.T) {
		var rec integratedRecord
		doc := integratedDoc(t, &rec, func(integrated func()) ApplyBudget {
			return ApplyBudget{Bytes: 1, Integrated: integrated}
		}, WithWholeOrNothingApplies(true))
		require.ErrorIs(t, ApplyUpdateV1(doc, typedAB(), "peer"), ErrApplyBudget)
		assert.Zero(t, rec.calls, "an apply rolled back does not call it")
		assert.Empty(t, rec.seen)
	})

	t.Run("a callback that panics", func(t *testing.T) {
		var rec integratedRecord
		doc := integratedDoc(t, &rec, func(integrated func()) ApplyBudget {
			return withIntegrated(func() {
				integrated()
				panic("the callback's panic")
			})
		})
		require.PanicsWithValue(t, "the callback's panic", func() { _ = ApplyUpdateV1(doc, typedAB(), "peer") })
		assert.Equal(t, []string{"text observer after 1", "OnUpdate after 1", "OnAfterTransaction after 1"}, rec.seen,
			"the observers still run, and the panic is raised after them")
		assert.Equal(t, "ab", doc.GetText("t").ToString(), "the apply committed")
	})

	t.Run("no callback", func(t *testing.T) {
		var rec integratedRecord
		doc := integratedDoc(t, &rec, func(func()) ApplyBudget { return ApplyBudget{} })
		require.NoError(t, ApplyUpdateV1(doc, typedAB(), "peer"))
		assert.Zero(t, rec.calls)
		assert.Equal(t, []string{"text observer after 0", "OnUpdate after 0", "OnAfterTransaction after 0"}, rec.seen,
			"a budget without one commits and fires every observer as before")
	})
}
