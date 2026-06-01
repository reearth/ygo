package webhook

import (
	"context"

	"github.com/reearth/ygo/crdt"
	ygws "github.com/reearth/ygo/provider/websocket"
)

// AttachTo wires every relevant Server lifecycle hook to wh so the
// webhook receives one Event per significant happening on every room
// the server hosts:
//
//   - OnLoadDocument   → EventLoad (and registers OnUpdate → EventUpdate
//     for that room's doc, so subsequent updates also flow)
//   - OnUnloadDocument → EventUnload
//   - OnFirstPeer      → EventConnect (first peer in this active session)
//   - OnLastPeer       → EventDisconnect (last peer in this active session)
//
// AttachTo returns an idempotent detach function that unwires the four
// hooks. It does NOT unsubscribe per-doc OnUpdate listeners — those
// stay attached for the lifetime of each doc, which matches Yjs's
// "listeners outlive types" semantics.
//
// AttachTo cooperates with any existing OnLoadDocument the caller has
// already set on srv: it captures the previous value and calls it
// first, propagating any error. This keeps the helper composable with
// application-specific load logic. The other three hooks (OnUnload,
// OnFirstPeer, OnLastPeer) are simple void hooks, so they similarly
// chain to any pre-existing handler.
//
// Typical usage:
//
//	srv := ygws.NewServer()
//	wh, _ := webhook.New(webhook.Config{URL: "https://hooks.example.com"})
//	defer wh.Close(context.Background())
//	detach := webhook.AttachTo(srv, wh)
//	defer detach()
//
// Use AttachTo to satisfy the Hocuspocus `extension-webhook` pattern
// without writing the boilerplate for each event type (#61).
func AttachTo(srv *ygws.Server, wh *Webhook) func() {
	prevLoad := srv.OnLoadDocument
	prevUnload := srv.OnUnloadDocument
	prevFirst := srv.OnFirstPeer
	prevLast := srv.OnLastPeer

	srv.OnLoadDocument = func(ctx context.Context, room string, doc *crdt.Doc) error {
		if prevLoad != nil {
			if err := prevLoad(ctx, room, doc); err != nil {
				return err
			}
		}
		wh.Enqueue(Event{Type: EventLoad, Room: room})
		// Subscribe per-doc update listener once at load time. The
		// listener captures room by value so events emitted later carry
		// the correct room name even after doc is reused.
		doc.OnUpdate(func(update []byte, _ any) {
			wh.Enqueue(Event{Type: EventUpdate, Room: room, Update: update})
		})
		return nil
	}

	srv.OnUnloadDocument = func(ctx context.Context, room string) {
		if prevUnload != nil {
			prevUnload(ctx, room)
		}
		wh.Enqueue(Event{Type: EventUnload, Room: room})
	}

	srv.OnFirstPeer = func(ctx context.Context, room string) {
		if prevFirst != nil {
			prevFirst(ctx, room)
		}
		wh.Enqueue(Event{Type: EventConnect, Room: room})
	}

	srv.OnLastPeer = func(ctx context.Context, room string) {
		if prevLast != nil {
			prevLast(ctx, room)
		}
		wh.Enqueue(Event{Type: EventDisconnect, Room: room})
	}

	return func() {
		srv.OnLoadDocument = prevLoad
		srv.OnUnloadDocument = prevUnload
		srv.OnFirstPeer = prevFirst
		srv.OnLastPeer = prevLast
	}
}
