package websocket

import (
	"context"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// failedJoinWriter holds a rejected request before it releases its room reference.
type failedJoinWriter struct {
	*httptest.ResponseRecorder
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (w *failedJoinWriter) WriteHeader(code int) {
	w.once.Do(func() {
		close(w.entered)
		<-w.release
	})
	w.ResponseRecorder.WriteHeader(code)
}

func TestUnit_Server_FailedJoinReapsExistingEagerRoom(t *testing.T) {
	for _, failure := range []string{"room-cap", "server-cap", "upgrade"} {
		for _, mode := range []string{"eager", "negative", "idle", "in-use"} {
			t.Run(failure+"/"+mode, func(t *testing.T) {
				s := NewServerWithPersistence(NewMemoryPersistence())
				if failure == "room-cap" {
					s.MaxPeersPerRoom = 1
				}
				if failure == "server-cap" {
					s.MaxConnections = 1
				}
				switch mode {
				case "idle":
					s.RoomIdleTimeout = time.Hour
				case "negative":
					s.RoomIdleTimeout = -time.Second
				}
				left := make(chan struct{})
				s.OnLastPeer = func(context.Context, string) { close(left) }
				var unloaded atomic.Int32
				s.OnUnloadDocument = func(context.Context, string) { unloaded.Add(1) }
				ts := httptest.NewServer(s)
				t.Cleanup(ts.Close)
				conn := dialRoomInternal(t, ts, "room")
				s.rmu.RLock()
				rm := s.rooms["room"]
				s.rmu.RUnlock()
				if mode == "in-use" {
					if _, _, err := s.getOrCreateRoom(context.Background(), "room"); err != nil {
						t.Fatal(err)
					}
				}
				w := &failedJoinWriter{
					ResponseRecorder: httptest.NewRecorder(),
					entered:          make(chan struct{}),
					release:          make(chan struct{}),
				}
				var releaseOnce sync.Once
				release := func() { releaseOnce.Do(func() { close(w.release) }) }
				done := make(chan struct{})
				go func() {
					s.ServeHTTP(w, httptest.NewRequest("GET", "/room", nil))
					close(done)
				}()
				t.Cleanup(func() {
					release()
					<-done
					if mode == "in-use" {
						s.releaseInflight(rm)
					}
					_ = s.CloseRoom("room", true)
					_ = s.Shutdown(context.Background())
				})
				select {
				case <-w.entered:
				case <-time.After(2 * time.Second):
					t.Fatal("join did not reach its rejection")
				}
				_ = conn.Close()
				select {
				case <-left:
				case <-time.After(2 * time.Second):
					t.Fatal("last peer did not leave")
				}
				if s.GetDoc("room") != rm.doc {
					t.Fatal("room disappeared while the rejected join still held it")
				}
				release()
				select {
				case <-done:
				case <-time.After(2 * time.Second):
					t.Fatal("failed join did not finish")
				}
				keep := mode == "idle" || mode == "in-use"
				if keep {
					if s.GetDoc("room") != rm.doc || unloaded.Load() != 0 {
						t.Fatal("failed join evicted an idle-resident or in-use room")
					}
				} else {
					if s.GetDoc("room") != nil || unloaded.Load() != 1 {
						t.Fatal("failed join left an empty eager room resident")
					}
					select {
					case <-rm.persistDone:
					default:
						t.Fatal("evicted room's persistence worker did not exit")
					}
				}
			})
		}
	}
}
