package websocket_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/reearth/ygo/cluster"
	"github.com/reearth/ygo/crdt"
	ygws "github.com/reearth/ygo/provider/websocket"
)

// loadContextKey names the value a test puts on the context of the call that
// creates a room.
type loadContextKey struct{}

// plainLoader is a PersistenceAdapter without LoadDocContext: every room loads
// the same stored state, and it counts its loads.
type plainLoader struct {
	state []byte
	mu    sync.Mutex
	loads int
}

func (a *plainLoader) LoadDoc(string) ([]byte, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.loads++
	return a.state, nil
}

func (a *plainLoader) StoreUpdate(string, []byte) error { return nil }

func (a *plainLoader) loadCount() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.loads
}

// contextLoader is plainLoader as a PersistenceAdapterLoadContext: LoadDocContext
// records the value its context carries under loadContextKey.
type contextLoader struct {
	plainLoader
	seen []any
}

func (a *contextLoader) LoadDocContext(ctx context.Context, _ string) ([]byte, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.seen = append(a.seen, ctx.Value(loadContextKey{}))
	return a.state, nil
}

func (a *contextLoader) seenValues() []any {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]any(nil), a.seen...)
}

// Each call that creates a room passes its context to the room's load: an
// adapter that implements PersistenceAdapterLoadContext reads the caller's
// value there, and an adapter without it still loads with LoadDoc.
func TestUnit_Server_LoadDocContext_TheLoadGetsTheCallersContext(t *testing.T) {
	stored := crdt.New(crdt.WithClientID(1))
	storedText := stored.GetText("t")
	stored.Transact(func(txn *crdt.Transaction) { storedText.Insert(txn, 0, "stored", nil) })
	state := crdt.EncodeStateAsUpdateV1(stored, nil)

	other := crdt.New(crdt.WithClientID(2))
	otherMap := other.GetMap("m")
	other.Transact(func(txn *crdt.Transaction) { otherMap.Set(txn, "k", 1) })
	update := crdt.EncodeStateAsUpdateV1(other, nil)

	// Each path creates "room" with a call whose context carries value.
	paths := map[string]func(t *testing.T, srv *ygws.Server, value string){
		"Apply": func(t *testing.T, srv *ygws.Server, value string) {
			ctx := context.WithValue(context.Background(), loadContextKey{}, value)
			require.NoError(t, srv.Apply(ctx, "room", func(doc *crdt.Doc, transact func(func(*crdt.Transaction))) {
				text := doc.GetText("t")
				transact(func(txn *crdt.Transaction) { text.Insert(txn, 0, "!", nil) })
			}))
		},
		"Inject": func(t *testing.T, srv *ygws.Server, value string) {
			ctx := context.WithValue(context.Background(), loadContextKey{}, value)
			require.NoError(t, srv.Inject(ctx, cluster.Inbound{Room: "room", Kind: cluster.KindSync, Data: update}))
		},
		"ServeHTTP": func(t *testing.T, srv *ygws.Server, value string) {
			ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				srv.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), loadContextKey{}, value)))
			}))
			t.Cleanup(ts.Close)
			// ServeHTTP loads the room before it upgrades the connection.
			dial(t, ts, "room")
		},
	}
	for name, create := range paths {
		t.Run(name, func(t *testing.T) {
			t.Run("an adapter with LoadDocContext reads the caller's value", func(t *testing.T) {
				adapter := &contextLoader{plainLoader: plainLoader{state: state}}
				srv := ygws.NewServerWithPersistence(adapter)
				t.Cleanup(func() { _ = srv.Shutdown(context.Background()) })

				create(t, srv, name)

				require.Equal(t, []any{name}, adapter.seenValues(), "the values the room's load saw on its context")
				require.Zero(t, adapter.loadCount(), "LoadDoc ran for an adapter that implements LoadDocContext")
				requireStoredText(t, srv)
			})
			t.Run("an adapter without it loads with LoadDoc", func(t *testing.T) {
				adapter := &plainLoader{state: state}
				srv := ygws.NewServerWithPersistence(adapter)
				t.Cleanup(func() { _ = srv.Shutdown(context.Background()) })

				create(t, srv, name)

				require.Equal(t, 1, adapter.loadCount(), "LoadDoc calls")
				requireStoredText(t, srv)
			})
		})
	}
}

// requireStoredText fails t unless srv's "room" holds the stored text.
func requireStoredText(t *testing.T, srv *ygws.Server) {
	t.Helper()
	doc := srv.GetDoc("room")
	require.NotNil(t, doc, "the room is not loaded")
	require.Contains(t, doc.GetText("t").ToString(), "stored", "the room's text")
}
