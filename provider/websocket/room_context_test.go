package websocket

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/reearth/ygo/crdt"
)

// roomWaitContext reports when an operation selects on cancellation.
type roomWaitContext struct {
	context.Context
	waiting chan struct{}
	once    sync.Once
}

func (c *roomWaitContext) Done() <-chan struct{} {
	c.once.Do(func() { close(c.waiting) })
	return c.Context.Done()
}

// loadingContextRoom holds the real loader until the test releases persistence.
func loadingContextRoom(t *testing.T) (*Server, *room, func()) {
	t.Helper()
	a := newBlockingLoadAdapter()
	a.block("room")
	s := NewServerWithPersistence(a)
	loaded := make(chan struct{})
	go func() {
		rm, _, _ := s.getOrCreateRoom(context.Background(), "room")
		if rm != nil {
			s.releaseInflight(rm)
		}
		close(loaded)
	}()
	select {
	case <-a.enteredCh("room"):
	case <-time.After(2 * time.Second):
		t.Fatal("loader did not enter persistence")
	}
	var once sync.Once
	release := func() { once.Do(func() { a.release("room") }) }
	t.Cleanup(func() {
		release()
		<-loaded
		_ = s.CloseRoom("room", true)
	})
	s.rmu.RLock()
	rm := s.rooms["room"]
	s.rmu.RUnlock()
	return s, rm, release
}

func TestUnit_Apply_CancelsExistingRoomLoadWithoutMutation(t *testing.T) {
	s, rm, release := loadingContextRoom(t)
	base, cancel := context.WithCancel(context.Background())
	defer cancel()
	ctx := &roomWaitContext{Context: base, waiting: make(chan struct{})}
	var called atomic.Bool
	done := make(chan error, 1)
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		done <- s.Apply(ctx, "room", func(doc *crdt.Doc, transact func(func(*crdt.Transaction))) {
			called.Store(true)
			text := doc.GetText("text")
			transact(func(txn *crdt.Transaction) { text.Insert(txn, 0, "late", nil) })
		})
	}()
	t.Cleanup(func() { release(); <-finished })
	select {
	case <-ctx.waiting:
	case <-time.After(2 * time.Second):
		t.Fatal("Apply does not select on cancellation while loading")
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Apply returned %v, want context.Canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("cancelled Apply waited for load")
	}
	if called.Load() {
		t.Fatal("cancelled Apply invoked its callback")
	}
	rm.mu.Lock()
	inflight := rm.inflight
	rm.mu.Unlock()
	if inflight != 1 {
		t.Fatalf("in-flight count = %d, want only the loader's reference", inflight)
	}
}

func TestUnit_CloseRoomContext_CancelsLoadingRoomWithoutClosing(t *testing.T) {
	for _, deadline := range []bool{false, true} {
		t.Run(map[bool]string{false: "cancel", true: "deadline"}[deadline], func(t *testing.T) {
			s, rm, _ := loadingContextRoom(t)
			var unloaded atomic.Int32
			s.OnUnloadDocument = func(context.Context, string) { unloaded.Add(1) }
			base, cancel := context.WithCancel(context.Background())
			want := context.Canceled
			if deadline {
				cancel()
				base, cancel = context.WithTimeout(context.Background(), 50*time.Millisecond)
				want = context.DeadlineExceeded
			}
			defer cancel()
			ctx := &roomWaitContext{Context: base, waiting: make(chan struct{})}
			done := make(chan error, 1)
			go func() { done <- s.CloseRoomContext(ctx, "room", true) }()
			select {
			case <-ctx.waiting:
			case <-time.After(2 * time.Second):
				t.Fatal("close did not select on cancellation")
			}
			if !deadline {
				cancel()
			}
			select {
			case err := <-done:
				if !errors.Is(err, want) {
					t.Fatalf("close returned %v, want %v", err, want)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("cancelled close waited for load")
			}
			s.rmu.RLock()
			resident := s.rooms["room"]
			s.rmu.RUnlock()
			if resident != rm || unloaded.Load() != 0 {
				t.Fatal("cancelled close changed or unloaded the loading room")
			}
		})
	}
}

func TestUnit_CloseRoomContext_CancelledReadyRoomDoesNotClose(t *testing.T) {
	s := NewServer()
	if err := s.Apply(context.Background(), "room", func(*crdt.Doc, func(func(*crdt.Transaction))) {}); !errors.Is(err, ErrNoChanges) {
		t.Fatal(err)
	}
	doc := s.GetDoc("room")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := s.CloseRoomContext(ctx, "room", true); !errors.Is(err, context.Canceled) {
		t.Fatalf("close returned %v, want context.Canceled", err)
	}
	if s.GetDoc("room") != doc {
		t.Fatal("cancelled close removed the ready room")
	}
	if err := s.CloseRoom("room", false); err != nil {
		t.Fatal(err)
	}
}

func TestUnit_CloseRoomContext_CompletesNormalClose(t *testing.T) {
	for _, legacy := range []bool{false, true} {
		t.Run(map[bool]string{false: "context", true: "legacy"}[legacy], func(t *testing.T) {
			s, _, release := loadingContextRoom(t)
			var unloaded atomic.Int32
			s.OnUnloadDocument = func(context.Context, string) { unloaded.Add(1) }
			ctx := &roomWaitContext{Context: context.Background(), waiting: make(chan struct{})}
			done := make(chan error, 1)
			go func() {
				if legacy {
					done <- s.CloseRoom("room", false)
				} else {
					done <- s.CloseRoomContext(ctx, "room", false)
				}
			}()
			if !legacy {
				<-ctx.waiting
			}
			release()
			select {
			case err := <-done:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("close did not complete after load")
			}
			if s.GetDoc("room") != nil || unloaded.Load() != 1 {
				t.Fatal("close did not remove the room and unload once")
			}
		})
	}
}

func TestUnit_Apply_CancellingOneWaiterPreservesSharedLoad(t *testing.T) {
	s, rm, release := loadingContextRoom(t)
	base, cancel := context.WithCancel(context.Background())
	defer cancel()
	cancelCtx := &roomWaitContext{Context: base, waiting: make(chan struct{})}
	keepCtx := &roomWaitContext{Context: context.Background(), waiting: make(chan struct{})}
	cancelDone, keepDone := make(chan error, 1), make(chan error, 1)
	var cancelledCallback atomic.Bool
	go func() {
		cancelDone <- s.Apply(cancelCtx, "room", func(*crdt.Doc, func(func(*crdt.Transaction))) {
			cancelledCallback.Store(true)
		})
	}()
	go func() {
		keepDone <- s.Apply(keepCtx, "room", func(doc *crdt.Doc, transact func(func(*crdt.Transaction))) {
			text := doc.GetText("text")
			transact(func(txn *crdt.Transaction) { text.Insert(txn, 0, "kept", nil) })
		})
	}()
	<-cancelCtx.waiting
	<-keepCtx.waiting
	cancel()
	select {
	case err := <-cancelDone:
		if !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("cancelled waiter did not return")
	}
	if cancelledCallback.Load() {
		t.Fatal("cancelled callback ran")
	}
	select {
	case err := <-keepDone:
		t.Fatalf("uncancelled waiter completed before load: %v", err)
	default:
	}
	rm.mu.Lock()
	inflight := rm.inflight
	rm.mu.Unlock()
	if inflight != 2 {
		t.Fatalf("in-flight count = %d, want loader and uncancelled waiter", inflight)
	}
	// Releasing persistence must still admit the other caller on the same room.
	s.rmu.RLock()
	resident := s.rooms["room"]
	s.rmu.RUnlock()
	if resident != rm {
		t.Fatal("cancelled waiter removed the shared load")
	}
	release()
	select {
	case err := <-keepDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("surviving waiter did not complete")
	}
	if s.GetDoc("room") != rm.doc || rm.doc.GetText("text").ToString() != "kept" {
		t.Fatal("surviving waiter did not update the shared room")
	}
}

func TestUnit_RoomContext_CancelRacesLoad(t *testing.T) {
	for _, apply := range []bool{false, true} {
		t.Run(map[bool]string{false: "close", true: "apply"}[apply], func(t *testing.T) {
			for range 100 {
				s, rm, release := loadingContextRoom(t)
				var unloaded atomic.Int32
				s.OnUnloadDocument = func(context.Context, string) { unloaded.Add(1) }
				base, cancel := context.WithCancel(context.Background())
				ctx := &roomWaitContext{Context: base, waiting: make(chan struct{})}
				var called atomic.Bool
				done := make(chan error, 1)
				go func() {
					if apply {
						done <- s.Apply(ctx, "room", func(doc *crdt.Doc, transact func(func(*crdt.Transaction))) {
							called.Store(true)
							text := doc.GetText("text")
							transact(func(txn *crdt.Transaction) { text.Insert(txn, 0, "write", nil) })
						})
					} else {
						done <- s.CloseRoomContext(ctx, "room", true)
					}
				}()
				<-ctx.waiting
				start := make(chan struct{})
				var racers sync.WaitGroup
				racers.Add(2)
				go func() { defer racers.Done(); <-start; cancel() }()
				go func() { defer racers.Done(); <-start; release() }()
				close(start)
				racers.Wait()
				select {
				case err := <-done:
					if err != nil && !errors.Is(err, context.Canceled) {
						t.Fatal(err)
					}
					s.rmu.RLock()
					resident := s.rooms["room"]
					s.rmu.RUnlock()
					if err != nil && (called.Load() || resident != rm || unloaded.Load() != 0) {
						t.Fatal("cancelled operation changed or unloaded the room")
					}
				case <-time.After(2 * time.Second):
					t.Fatal("operation hung during cancellation/load race")
				}
				if err := s.CloseRoom("room", true); err != nil && !errors.Is(err, ErrRoomNotFound) {
					t.Fatal(err)
				}
				if unloaded.Load() != 1 {
					t.Fatalf("unload count = %d, want 1", unloaded.Load())
				}
			}
		})
	}
}

// cancelOnDoneContext makes both select cases ready before admission.
type cancelOnDoneContext struct {
	context.Context
	cancel context.CancelFunc
}

func (c *cancelOnDoneContext) Done() <-chan struct{} {
	c.cancel()
	return c.Context.Done()
}

func TestUnit_Apply_CancellationWinsReadyLoad(t *testing.T) {
	s := NewServer()
	if err := s.Apply(context.Background(), "room", func(*crdt.Doc, func(func(*crdt.Transaction))) {}); !errors.Is(err, ErrNoChanges) {
		t.Fatal(err)
	}
	doc := s.GetDoc("room")
	base, cancel := context.WithCancel(context.Background())
	defer cancel()
	ctx := &cancelOnDoneContext{Context: base, cancel: cancel}
	called := false
	err := s.Apply(ctx, "room", func(*crdt.Doc, func(func(*crdt.Transaction))) { called = true })
	if !errors.Is(err, context.Canceled) || called || s.GetDoc("room") != doc {
		t.Fatalf("cancelled ready Apply: error=%v, callback=%v", err, called)
	}
	s.rmu.RLock()
	rm := s.rooms["room"]
	s.rmu.RUnlock()
	rm.mu.Lock()
	inflight := rm.inflight
	rm.mu.Unlock()
	if inflight != 0 {
		t.Fatalf("in-flight count = %d, want 0", inflight)
	}
	if err := s.CloseRoom("room", false); err != nil {
		t.Fatal(err)
	}
}
