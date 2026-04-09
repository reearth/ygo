package crdt

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/reearth/ygo/encoding"
)

// Single-client case: ALL predecessors are GC'd. The parent type name is
// permanently lost from the Yjs wire format. The y-websocket JS server
// has the same limitation. The server must accept the update without error
// to keep real-time sync alive for YText/YArray data in the same document.
func TestYjsCompat_GCdYMapOrigin_SingleClient_NoError(t *testing.T) {
	enc := encoding.NewEncoder()

	enc.WriteVarUint(1)  // 1 client group
	enc.WriteVarUint(5)  // 5 structs
	enc.WriteVarUint(42) // clientID
	enc.WriteVarUint(0)  // startClock

	for i := 0; i < 4; i++ {
		enc.WriteUint8(0)   // GC struct
		enc.WriteVarUint(1) // length=1
	}

	info := wireAny | flagHasOrigin | flagHasParentSub
	enc.WriteUint8(info)
	enc.WriteVarUint(42) // origin client
	enc.WriteVarUint(3)  // origin clock
	enc.WriteVarString("title")
	enc.WriteVarUint(1)
	enc.WriteAny("Hello")

	enc.WriteVarUint(0) // empty delete set

	// Must not error — matching Yjs behavior.
	doc := New(WithClientID(99))
	require.NoError(t, ApplyUpdateV1(doc, enc.Bytes(), nil))

	// Re-encode must produce a valid update (orphaned items → GC structs).
	state := EncodeStateAsUpdateV1(doc, nil)
	doc2 := New(WithClientID(100))
	require.NoError(t, ApplyUpdateV1(doc2, state, nil))
}

// Multi-client case: another client has items with explicit parent info
// for the same map type. The orphaned item's parent can be resolved.
func TestYjsCompat_GCdYMapOrigin_MultiClient_Resolved(t *testing.T) {
	enc := encoding.NewEncoder()
	enc.WriteVarUint(2) // 2 client groups

	// Client 42: GC'd predecessors + active item (orphaned)
	enc.WriteVarUint(5)
	enc.WriteVarUint(42)
	enc.WriteVarUint(0)
	for i := 0; i < 4; i++ {
		enc.WriteUint8(0)
		enc.WriteVarUint(1)
	}
	info := wireAny | flagHasOrigin | flagHasParentSub
	enc.WriteUint8(info)
	enc.WriteVarUint(42)
	enc.WriteVarUint(3)
	enc.WriteVarString("title")
	enc.WriteVarUint(1)
	enc.WriteAny("Hello")

	// Client 100: item in the SAME map type with explicit parent
	enc.WriteVarUint(1)
	enc.WriteVarUint(100)
	enc.WriteVarUint(0)
	infoB := wireAny | flagHasParentSub
	enc.WriteUint8(infoB)
	enc.WriteUint8(1) // named root
	enc.WriteVarString("metadata")
	enc.WriteVarString("author")
	enc.WriteVarUint(1)
	enc.WriteAny("Alice")

	enc.WriteVarUint(0)

	doc := New(WithClientID(99))
	require.NoError(t, ApplyUpdateV1(doc, enc.Bytes(), nil))

	m := doc.GetMap("metadata")
	val, ok := m.Get("author")
	require.True(t, ok)
	assert.Equal(t, "Alice", val)

	val2, ok2 := m.Get("title")
	require.True(t, ok2, "multi-client case should resolve title via parent heuristic")
	assert.Equal(t, "Hello", val2)
}

// Incremental sync scenario: updates arrive one at a time BEFORE GC.
// This is the normal real-time path that works in both y-websocket and ygo.
func TestYjsCompat_YMapIncrementalSync_NoGC(t *testing.T) {
	server := New(WithClientID(99))
	m := server.GetMap("meta")

	// Simulate 5 incremental updates from a Yjs client (before GC)
	client := New(WithClientID(42))
	cm := client.GetMap("meta")

	for _, v := range []string{"H", "He", "Hel", "Hell", "Hello"} {
		var update []byte
		client.OnUpdate(func(u []byte, _ any) { update = u })
		client.Transact(func(txn *Transaction) { cm.Set(txn, "title", v) })

		// Server applies each incremental update (before client GC)
		require.NoError(t, ApplyUpdateV1(server, update, nil))
	}

	val, ok := m.Get("title")
	require.True(t, ok)
	assert.Equal(t, "Hello", val)

	// Late-joiner gets re-encoded state
	state := EncodeStateAsUpdateV1(server, nil)
	doc2 := New(WithClientID(100))
	require.NoError(t, ApplyUpdateV1(doc2, state, nil))

	val2, ok2 := doc2.GetMap("meta").Get("title")
	require.True(t, ok2)
	assert.Equal(t, "Hello", val2)
}
