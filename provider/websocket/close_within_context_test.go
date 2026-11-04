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

// withinWaitContext reports when the close selects on cancellation.
type withinWaitContext struct {
	context.Context
	waiting chan struct{}
	once    sync.Once
}

func (c *withinWaitContext) Done() <-chan struct{} {
	c.once.Do(func() { close(c.waiting) })
	return c.Context.Done()
}

func TestUnit_CloseRoomWithinContext_CancelsLoadingRoom(t *testing.T) {
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
	<-a.enteredCh("room")
	defer func() {
		a.release("room")
		<-loaded
		_ = s.CloseRoom("room", false)
	}()
	s.rmu.RLock()
	rm := s.rooms["room"]
	s.rmu.RUnlock()
	base, cancel := context.WithCancel(context.Background())
	defer cancel()
	ctx := &withinWaitContext{Context: base, waiting: make(chan struct{})}
	done := make(chan error, 1)
	go func() { done <- s.CloseRoomWithinContext(ctx, "room", rm.doc, true) }()
	select {
	case <-ctx.waiting:
	case <-time.After(2 * time.Second):
		t.Fatal("close did not wait on context")
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("close returned %v, want context.Canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("cancelled close waited for load")
	}
	s.rmu.RLock()
	resident := s.rooms["room"]
	s.rmu.RUnlock()
	if resident != rm {
		t.Fatal("cancelled close removed or replaced the loading room")
	}
}

func TestUnit_CloseRoomWithinContext_PreservesApplyDiscount(t *testing.T) {
	for _, legacy := range []bool{false, true} {
		t.Run(map[bool]string{false: "context", true: "legacy"}[legacy], func(t *testing.T) {
			s := NewServer()
			var unloaded atomic.Int32
			s.OnUnloadDocument = func(context.Context, string) { unloaded.Add(1) }
			err := s.Apply(context.Background(), "room", func(doc *crdt.Doc, _ func(func(*crdt.Transaction))) {
				var closeErr error
				if legacy {
					closeErr = s.CloseRoomWithin("room", doc, false)
				} else {
					closeErr = s.CloseRoomWithinContext(context.Background(), "room", doc, false)
				}
				if closeErr != nil {
					t.Errorf("close inside Apply: %v", closeErr)
				}
			})
			if !errors.Is(err, ErrNoChanges) {
				t.Fatalf("Apply returned %v, want ErrNoChanges", err)
			}
			if s.GetDoc("room") != nil || unloaded.Load() != 1 {
				t.Fatal("close did not remove the room and unload exactly once")
			}
		})
	}
}

func TestUnit_CloseRoomWithinContext_CancelledReadyRoomDoesNotClose(t *testing.T) {
	s := NewServer()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := s.Apply(context.Background(), "room", func(doc *crdt.Doc, _ func(func(*crdt.Transaction))) {
		if err := s.CloseRoomWithinContext(ctx, "room", doc, true); !errors.Is(err, context.Canceled) {
			t.Errorf("close returned %v, want context.Canceled", err)
		}
	})
	if !errors.Is(err, ErrNoChanges) || s.GetDoc("room") == nil {
		t.Fatalf("cancelled close changed room: Apply=%v, doc=%p", err, s.GetDoc("room"))
	}
	if err := s.CloseRoom("room", false); err != nil {
		t.Fatal(err)
	}
}

func TestUnit_CloseRoomWithinContext_CancelRacesLoad(t *testing.T) {
	for range 100 {
		a := newBlockingLoadAdapter()
		a.block("room")
		s := NewServerWithPersistence(a)
		var unloaded atomic.Int32
		s.OnUnloadDocument = func(context.Context, string) { unloaded.Add(1) }
		loaded := make(chan struct{})
		go func() {
			rm, _, _ := s.getOrCreateRoom(context.Background(), "room")
			if rm != nil {
				s.releaseInflight(rm)
			}
			close(loaded)
		}()
		<-a.enteredCh("room")
		s.rmu.RLock()
		rm := s.rooms["room"]
		s.rmu.RUnlock()
		base, cancel := context.WithCancel(context.Background())
		ctx := &withinWaitContext{Context: base, waiting: make(chan struct{})}
		done := make(chan error, 1)
		go func() { done <- s.CloseRoomWithinContext(ctx, "room", rm.doc, true) }()
		<-ctx.waiting
		start := make(chan struct{})
		var racers sync.WaitGroup
		racers.Add(2)
		go func() { defer racers.Done(); <-start; cancel() }()
		go func() { defer racers.Done(); <-start; a.release("room") }()
		close(start)
		racers.Wait()
		var closeErr error
		select {
		case closeErr = <-done:
		case <-time.After(2 * time.Second):
			t.Fatal("close hung during cancellation/load race")
		}
		<-loaded
		if closeErr != nil && !errors.Is(closeErr, context.Canceled) && !errors.Is(closeErr, ErrNotInApply) {
			t.Fatalf("close returned unexpected error: %v", closeErr)
		}
		if closeErr != nil && s.GetDoc("room") != rm.doc {
			t.Fatal("refused close removed the room")
		}
		if err := s.CloseRoom("room", true); err != nil && !errors.Is(err, ErrRoomNotFound) {
			t.Fatal(err)
		}
		if unloaded.Load() != 1 {
			t.Fatalf("unload count = %d, want 1", unloaded.Load())
		}
		rm.mu.Lock()
		inflight := rm.inflight
		rm.mu.Unlock()
		if inflight != 0 {
			t.Fatalf("in-flight references = %d, want 0", inflight)
		}
	}
}
