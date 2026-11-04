package websocket

import (
	"context"
	"errors"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/reearth/ygo/crdt"
)

func TestUnit_Server_FailedJoinDoesNotFlushActiveRoom(t *testing.T) {
	for _, kind := range []string{"apply", "peer", "room-cap", "server-cap"} {
		t.Run(kind, func(t *testing.T) {
			a := newBlockingAdapter()
			s := NewServerWithPersistence(a)
			s.PersistCoalesceWindow = time.Hour
			s.PersistCoalesceMaxWait = time.Hour
			if kind == "room-cap" {
				s.MaxPeersPerRoom = 1
			}
			if kind == "server-cap" {
				s.MaxConnections = 1
			}
			if err := s.Apply(context.Background(), "room", func(doc *crdt.Doc, transact func(func(*crdt.Transaction))) {
				text := doc.GetText("text")
				transact(func(txn *crdt.Transaction) { text.Insert(txn, 0, "pending", nil) })
			}); err != nil {
				t.Fatal(err)
			}
			leave, inside := make(chan struct{}), make(chan struct{})
			applied := make(chan error, 1)
			var stopOnce sync.Once
			stop := func() { stopOnce.Do(func() { close(leave) }) }
			ts := httptest.NewServer(s)
			t.Cleanup(ts.Close)
			if kind == "apply" {
				go func() {
					applied <- s.Apply(context.Background(), "room", func(*crdt.Doc, func(func(*crdt.Transaction))) {
						close(inside)
						<-leave
					})
				}()
				<-inside
			} else {
				conn := dialRoomInternal(t, ts, "room")
				stop = func() { stopOnce.Do(func() { _ = conn.Close() }) }
			}
			w := httptest.NewRecorder()
			done := make(chan struct{})
			go func() {
				s.ServeHTTP(w, httptest.NewRequest("GET", "/room", nil))
				close(done)
			}()
			t.Cleanup(func() {
				close(a.release)
				stop()
				<-done
				if kind == "apply" {
					if err := <-applied; !errors.Is(err, ErrNoChanges) {
						t.Errorf("Apply returned %v", err)
					}
				}
				_ = s.CloseRoom("room", true)
				_ = s.Shutdown(context.Background())
			})
			select {
			case <-a.blocked:
				t.Fatal("rejected join forced a persistence flush for an active room")
			case <-done:
			case <-time.After(2 * time.Second):
				t.Fatal("rejected join did not return")
			}
			want := 400
			if kind == "room-cap" || kind == "server-cap" {
				want = 503
			}
			if w.Code != want {
				t.Fatalf("status = %d, want %d", w.Code, want)
			}
			select {
			case <-a.blocked:
				t.Fatal("rejected join flushed the pending batch")
			default:
			}
		})
	}
}
