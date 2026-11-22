package websocket

import (
	"context"
	"testing"
)

// backgroundLoader is a PersistenceAdapterLoadContext that loads nothing and
// keeps the context of its last load.
type backgroundLoader struct{ ctx context.Context }

func (a *backgroundLoader) LoadDoc(string) ([]byte, error)   { return nil, nil }
func (a *backgroundLoader) StoreUpdate(string, []byte) error { return nil }

func (a *backgroundLoader) LoadDocContext(ctx context.Context, _ string) ([]byte, error) {
	a.ctx = ctx
	return nil, nil
}

// A caller that passes no context loads the room with context.Background().
func TestUnit_GetOrCreateRoom_ANilContextLoadsWithBackground(t *testing.T) {
	adapter := &backgroundLoader{}
	s := NewServerWithPersistence(adapter)
	t.Cleanup(func() { _ = s.Shutdown(context.Background()) })

	var none context.Context
	r, _, err := s.getOrCreateRoom(none, "room")
	if err != nil {
		t.Fatalf("the room did not load: %v", err)
	}
	s.releaseInflight(r)
	if adapter.ctx != context.Background() {
		t.Fatalf("LoadDocContext got context %v, want context.Background()", adapter.ctx)
	}
}
