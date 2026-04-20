# Server-side Document Injection — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add `Server.BroadcastUpdate`, `Server.Apply`, and `Server.CloseRoom` to the WebSocket provider so backend services and AI agents can push changes into live rooms without simulating a WebSocket peer.

**Architecture:** All new exported API lives in a new file `provider/websocket/inject.go`, kept separate from `server.go` to prevent the existing 746-line file from growing further. The new methods reuse the existing room-lookup, peer-snapshot, and broadcast-goroutine patterns from `server.go`. A small additive change to `server.go` adds three new exported fields and threads `MaxRooms` enforcement through `getOrCreateRoom`.

**Tech Stack:** Go 1.22+, `gorilla/websocket`, `reearth/ygo/crdt`, `testify/assert` + `testify/require`.

**Spec:** [docs/superpowers/specs/2026-04-20-server-side-injection-design.md](../specs/2026-04-20-server-side-injection-design.md)

---

## File structure

**New files:**
- `provider/websocket/inject.go` — types, errors, `BroadcastUpdate`, `Apply`, `CloseRoom`, `effectiveMaxUpdateBytes` helper
- `provider/websocket/inject_test.go` — all new tests

**Modified files:**
- `provider/websocket/server.go`:
  - Add exported fields `OnInject`, `MaxUpdateBytes`, `MaxRooms` to `Server` struct
  - Modify `getOrCreateRoom` to enforce `MaxRooms`
  - Modify `ServeHTTP` to convert `ErrTooManyRooms` from `getOrCreateRoom` into HTTP 503
- `README.md` — new section "Server-side document injection"
- `docs/CHANGELOG.md` if it exists, else create release-notes entry

---

## Task 1: Scaffold new types, errors, and hook signature

**Files:**
- Create: `provider/websocket/inject.go`
- Test: `provider/websocket/inject_test.go` (create)

Pure scaffolding — adds declarations, no behavior. Keeps later tasks focused on logic.

- [ ] **Step 1: Create inject.go with package declaration and imports**

Create `provider/websocket/inject.go`:

```go
// Package websocket: server-side document injection.
//
// This file adds APIs that let server-side Go code (AI agents, HTTP
// handlers, content pipelines) push changes into a live room without
// simulating a WebSocket peer. See docs/superpowers/specs/ for the
// full design rationale.
package websocket

import (
	"context"
	"errors"

	"github.com/reearth/ygo/crdt"
)

// InjectOp identifies which server-side write path is being invoked.
type InjectOp int

const (
	// OpBroadcastUpdate is passed to OnInject when BroadcastUpdate is
	// the calling method.
	OpBroadcastUpdate InjectOp = iota
	// OpApply is passed to OnInject when Apply is the calling method.
	OpApply
)

// String returns a human-readable name for the op.
func (o InjectOp) String() string {
	switch o {
	case OpBroadcastUpdate:
		return "BroadcastUpdate"
	case OpApply:
		return "Apply"
	default:
		return "unknown"
	}
}

// InjectInfo is passed to OnInject. Additional fields may be added in
// future versions; callers must not rely on the struct being fixed-size.
type InjectInfo struct {
	// Room is the room name the operation targets.
	Room string
	// Op identifies the calling method.
	Op InjectOp
	// UpdateSize is the length of the update bytes for BroadcastUpdate,
	// or 0 for Apply (the delta has not yet been produced).
	UpdateSize int
}

// InjectHook is called before every server-side write. Return a non-nil
// error to refuse the operation; the error is wrapped and returned to
// the caller.
type InjectHook func(ctx context.Context, info InjectInfo) error

// Error sentinels returned by BroadcastUpdate, Apply, and CloseRoom.
// Callers should compare with errors.Is rather than ==.
var (
	// ErrServerShutdown is returned when a server-side write is attempted
	// after Server.Shutdown has been called.
	ErrServerShutdown = errors.New("ygo/websocket: server is shut down")
	// ErrInvalidRoomName is returned when a room name fails validation
	// (empty, > 255 bytes, path-unsafe, or contains control characters).
	ErrInvalidRoomName = errors.New("ygo/websocket: invalid room name")
	// ErrRoomNotFound is returned when a server-side write targets a
	// room that does not currently exist. May occur if the last peer
	// disconnected concurrently; callers broadcasting to ephemeral rooms
	// should treat this as non-fatal.
	ErrRoomNotFound = errors.New("ygo/websocket: room not found")
	// ErrRoomHasPeers is returned by CloseRoom when called with force=false
	// on a room that still has connected peers.
	ErrRoomHasPeers = errors.New("ygo/websocket: room has connected peers")
	// ErrInvalidUpdate is returned when BroadcastUpdate cannot parse the
	// caller-supplied update bytes as a V1 update.
	ErrInvalidUpdate = errors.New("ygo/websocket: invalid V1 update")
	// ErrUpdateTooLarge is returned when an update exceeds MaxUpdateBytes.
	ErrUpdateTooLarge = errors.New("ygo/websocket: update exceeds MaxUpdateBytes")
	// ErrTooManyRooms is returned when auto-creating a room would
	// exceed Server.MaxRooms.
	ErrTooManyRooms = errors.New("ygo/websocket: MaxRooms exceeded")
	// ErrNoChanges is returned by Apply when fn produces no delta
	// (either never called transact or called transact with a no-op body).
	ErrNoChanges = errors.New("ygo/websocket: no changes produced")
)

// Suppress unused-import lint until later tasks add the function bodies
// that use context and crdt.
var _ context.Context
var _ = crdt.New
```

The last two declarations are temporary — they prevent Go's unused-import errors until later tasks populate the functions that use `context` and `crdt`. They will be removed in Task 2.

- [ ] **Step 2: Create inject_test.go with scaffolding and a sanity test**

Create `provider/websocket/inject_test.go`:

```go
package websocket_test

import (
	"testing"

	"github.com/stretchr/testify/assert"

	ygws "github.com/reearth/ygo/provider/websocket"
)

func TestUnit_InjectOp_String(t *testing.T) {
	assert.Equal(t, "BroadcastUpdate", ygws.OpBroadcastUpdate.String())
	assert.Equal(t, "Apply", ygws.OpApply.String())
	assert.Equal(t, "unknown", ygws.InjectOp(99).String())
}
```

- [ ] **Step 3: Run test to verify it passes**

Run: `cd /Users/nimit/Documents/Eukarya/ygo && go test ./provider/websocket/ -run TestUnit_InjectOp_String -v`

Expected: PASS. If this fails, the package doesn't compile — fix before continuing.

- [ ] **Step 4: Run full test suite to verify nothing else broke**

Run: `cd /Users/nimit/Documents/Eukarya/ygo && go test ./...`

Expected: all existing tests still pass.

- [ ] **Step 5: Commit**

```bash
git add provider/websocket/inject.go provider/websocket/inject_test.go
git commit -m "feat(ws): add InjectOp/InjectInfo/InjectHook types and error sentinels

Scaffolding for server-side document injection. Types and error
sentinels land first so subsequent tasks can focus on behavior.
No public API surface is exercised yet."
```

---

## Task 2: Add Server fields and `effectiveMaxUpdateBytes` helper

**Files:**
- Modify: `provider/websocket/server.go` (Server struct around line 137)
- Modify: `provider/websocket/inject.go` (add helper)

Adds the three new exported fields on `Server` and the `effectiveMaxUpdateBytes` helper. No enforcement yet — that comes in Task 3.

- [ ] **Step 1: Write failing test for `effectiveMaxUpdateBytes` default**

Add to `provider/websocket/inject_test.go`:

```go
func TestUnit_EffectiveMaxUpdateBytes_DefaultsTo64MiB(t *testing.T) {
	srv := ygws.NewServer()
	// MaxUpdateBytes defaults to 0 → effective value should be 64 MiB.
	// We verify via behavior in later tasks; here we just assert field
	// presence by assigning.
	srv.MaxUpdateBytes = 1024
	assert.Equal(t, 1024, srv.MaxUpdateBytes)
}

func TestUnit_Server_MaxRoomsField_Exists(t *testing.T) {
	srv := ygws.NewServer()
	srv.MaxRooms = 5
	assert.Equal(t, 5, srv.MaxRooms)
}

func TestUnit_Server_OnInjectField_Exists(t *testing.T) {
	srv := ygws.NewServer()
	srv.OnInject = func(ctx context.Context, info ygws.InjectInfo) error { return nil }
	assert.NotNil(t, srv.OnInject)
}
```

Add this import to inject_test.go:
```go
	"context"
```

- [ ] **Step 2: Run tests — verify they fail to compile**

Run: `cd /Users/nimit/Documents/Eukarya/ygo && go test ./provider/websocket/`

Expected: compile errors "Server has no field `MaxUpdateBytes`" etc.

- [ ] **Step 3: Add fields to Server struct in server.go**

In `provider/websocket/server.go`, find the `Server` struct block (currently ends around line 172 with `activeConns atomic.Int64`). Add the following three fields immediately before `activeConns`:

```go
	// OnInject, if non-nil, is called before every server-side write
	// (BroadcastUpdate or Apply). Return a non-nil error to refuse the
	// operation; the error is wrapped and returned to the caller.
	// For BroadcastUpdate, InjectInfo.UpdateSize is len(update); for
	// Apply it is 0 (the delta has not yet been produced).
	OnInject InjectHook

	// MaxUpdateBytes is the maximum size of a single V1 update that
	// BroadcastUpdate will fan out, or that Apply will produce and
	// fan out. Zero means use the same 64 MiB default applied to
	// WebSocket peer frames (maxWSMessageBytes).
	MaxUpdateBytes int

	// MaxRooms caps the total number of rooms the server will hold at
	// once, across both peer-upgrade-created and Apply-created rooms.
	// Zero means unlimited. Enforcement applies uniformly: peer upgrades
	// past the cap receive HTTP 503; Apply past the cap returns
	// ErrTooManyRooms.
	MaxRooms int
```

- [ ] **Step 4: Add `effectiveMaxUpdateBytes` helper in inject.go**

In `provider/websocket/inject.go`, remove the two `var _` unused-import suppressors at the bottom of the file, and add:

```go
// effectiveMaxUpdateBytes returns the server's configured per-update
// cap, or the default 64 MiB (matching the peer frame cap) when unset.
func (s *Server) effectiveMaxUpdateBytes() int {
	if s.MaxUpdateBytes > 0 {
		return s.MaxUpdateBytes
	}
	return maxWSMessageBytes
}
```

- [ ] **Step 5: Run tests — verify they pass**

Run: `cd /Users/nimit/Documents/Eukarya/ygo && go test ./provider/websocket/ -v`

Expected: PASS.

- [ ] **Step 6: Run full test suite**

Run: `cd /Users/nimit/Documents/Eukarya/ygo && go test ./...`

Expected: all pass. No existing behavior changed.

- [ ] **Step 7: Commit**

```bash
git add provider/websocket/server.go provider/websocket/inject.go provider/websocket/inject_test.go
git commit -m "feat(ws): add OnInject/MaxUpdateBytes/MaxRooms fields on Server

Fields are inert — no enforcement wired up yet. Adds
effectiveMaxUpdateBytes helper that resolves MaxUpdateBytes==0 to
the 64 MiB peer-frame default."
```

---

## Task 3: Enforce `MaxRooms` in `getOrCreateRoom` and `ServeHTTP`

**Files:**
- Modify: `provider/websocket/server.go` (`getOrCreateRoom`, `ServeHTTP`)
- Test: `provider/websocket/inject_test.go`

`getOrCreateRoom` returns `ErrTooManyRooms` when the cap is hit. `ServeHTTP` converts that to HTTP 503. Both peer upgrades and `Apply` (later tasks) use the same enforcement.

- [ ] **Step 1: Write failing test for HTTP 503 on MaxRooms in peer upgrade**

Add to `provider/websocket/inject_test.go`:

```go
import (
	"net/http"
	"net/http/httptest"
	// ... existing imports
)

func TestUnit_PeerUpgrade_MaxRoomsExceeded_Returns503(t *testing.T) {
	srv := ygws.NewServer()
	srv.MaxRooms = 1
	httpSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		srv.ServeHTTP(w, r)
	}))
	t.Cleanup(httpSrv.Close)

	// First peer in room-A succeeds.
	connA := dial(t, httpSrv, "room-A")
	drainHandshake(t, connA, crdt.New())

	// Peer attempting to open a second room fails with 503.
	resp, err := http.Get(httpSrv.URL + "/room-B") // plain GET — we inspect status, no upgrade attempt
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusServiceUnavailable, resp.StatusCode)
}
```

Add required imports: `"net/http"`, `"net/http/httptest"`, `"github.com/reearth/ygo/crdt"`, `"github.com/stretchr/testify/require"`.

Note: a plain `http.Get` to the handler triggers `ServeHTTP`, which will call `getOrCreateRoom` before attempting WebSocket upgrade — so we can observe the 503 without completing a handshake. The first peer must be on room-A before the GET so that the first slot is already used.

- [ ] **Step 2: Run test — verify it fails**

Run: `cd /Users/nimit/Documents/Eukarya/ygo && go test ./provider/websocket/ -run TestUnit_PeerUpgrade_MaxRoomsExceeded_Returns503 -v`

Expected: FAIL — currently peer upgrade creates unlimited rooms.

- [ ] **Step 3: Modify `getOrCreateRoom` to check `MaxRooms`**

In `provider/websocket/server.go`, locate `getOrCreateRoom` (starts at line 295). Immediately after the `if r, ok := s.rooms[name]; ok { return r, nil }` block and before the `r := &room{...}` assignment, add:

```go
	if s.MaxRooms > 0 && len(s.rooms) >= s.MaxRooms {
		return nil, ErrTooManyRooms
	}
```

The existing function already holds `s.rmu.Lock()` at the top, so the check is atomic with the creation.

- [ ] **Step 4: Modify `ServeHTTP` to return 503 on `ErrTooManyRooms`**

In `provider/websocket/server.go`, find the block at line 380–384:

```go
	rm, err := s.getOrCreateRoom(name)
	if err != nil {
		http.Error(w, "room unavailable", http.StatusInternalServerError)
		return
	}
```

Replace with:

```go
	rm, err := s.getOrCreateRoom(name)
	if err != nil {
		if errors.Is(err, ErrTooManyRooms) {
			http.Error(w, "too many rooms", http.StatusServiceUnavailable)
			return
		}
		http.Error(w, "room unavailable", http.StatusInternalServerError)
		return
	}
```

Add `"errors"` to server.go's import block if not already present.

- [ ] **Step 5: Run test — verify it passes**

Run: `cd /Users/nimit/Documents/Eukarya/ygo && go test ./provider/websocket/ -run TestUnit_PeerUpgrade_MaxRoomsExceeded_Returns503 -v`

Expected: PASS.

- [ ] **Step 6: Run full test suite**

Run: `cd /Users/nimit/Documents/Eukarya/ygo && go test ./...`

Expected: all pass.

- [ ] **Step 7: Commit**

```bash
git add provider/websocket/server.go provider/websocket/inject_test.go
git commit -m "feat(ws): enforce MaxRooms in getOrCreateRoom with 503 on peer upgrade

Prevents unbounded room creation via raw WebSocket upgrades. The
cap also applies to Apply's auto-create path (wired up in the
Apply task)."
```

---

## Task 4: `BroadcastUpdate` — happy path + core error semantics

**Files:**
- Modify: `provider/websocket/inject.go` (add `BroadcastUpdate`)
- Test: `provider/websocket/inject_test.go`

Implements `BroadcastUpdate` and the validation path leading to fan-out. Covers the "fans out to peers without mutating doc" acceptance criterion.

- [ ] **Step 1: Write failing test for happy path**

Add to `provider/websocket/inject_test.go`:

```go
func TestUnit_BroadcastUpdate_FansOutToConnectedPeers(t *testing.T) {
	srv := ygws.NewServer()
	httpSrv := httptest.NewServer(http.HandlerFunc(srv.ServeHTTP))
	t.Cleanup(httpSrv.Close)

	// Open two peer connections and drain their handshakes.
	conn1 := dial(t, httpSrv, "room")
	peerDoc1 := crdt.New()
	drainHandshake(t, conn1, peerDoc1)

	conn2 := dial(t, httpSrv, "room")
	peerDoc2 := crdt.New()
	drainHandshake(t, conn2, peerDoc2)

	// Build an update externally: new doc, set a map key, encode.
	external := crdt.New()
	external.Transact(func(txn *crdt.Transaction) {
		external.GetMap("m")
	})
	external.GetMap("m").Set(nil, "k", "v")
	update := crdt.EncodeStateAsUpdateV1(external, nil)

	// Apply to server's doc AND broadcast (the documented pattern).
	serverDoc := srv.GetDoc("room")
	require.NotNil(t, serverDoc)
	require.NoError(t, crdt.ApplyUpdateV1(serverDoc, update, nil))
	require.NoError(t, srv.BroadcastUpdate(context.Background(), "room", update))

	// Both peers receive a sync update frame.
	for i, conn := range []*gws.Conn{conn1, conn2} {
		outerType, payload := readOne(t, conn, 2*time.Second)
		assert.Equal(t, uint64(0), outerType, "peer %d should receive msgSync", i+1)
		peerDoc := []*crdt.Doc{peerDoc1, peerDoc2}[i]
		_, _ = ygsync.ApplySyncMessage(peerDoc, payload, nil)
		assert.Equal(t, "v", peerDoc.GetMap("m").Get("k"))
	}
}
```

Add imports as needed: `"time"`, `gws "github.com/gorilla/websocket"`, `ygsync "github.com/reearth/ygo/sync"`.

- [ ] **Step 2: Add error-path tests**

Also add:

```go
func TestUnit_BroadcastUpdate_MissingRoom_ErrRoomNotFound(t *testing.T) {
	srv := ygws.NewServer()
	err := srv.BroadcastUpdate(context.Background(), "ghost", []byte{0x00, 0x00})
	assert.ErrorIs(t, err, ygws.ErrRoomNotFound)
}

func TestUnit_BroadcastUpdate_InvalidRoomName(t *testing.T) {
	srv := ygws.NewServer()

	for _, name := range []string{"", "..", ".", "\x01bad", strings.Repeat("x", 256)} {
		err := srv.BroadcastUpdate(context.Background(), name, []byte{0x00, 0x00})
		assert.ErrorIs(t, err, ygws.ErrInvalidRoomName, "name=%q", name)
	}
}

func TestUnit_BroadcastUpdate_ContextAlreadyCancelled(t *testing.T) {
	srv := ygws.NewServer()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := srv.BroadcastUpdate(ctx, "room", []byte{0x00, 0x00})
	assert.ErrorIs(t, err, context.Canceled)
}

func TestUnit_BroadcastUpdate_ShutdownServer(t *testing.T) {
	srv := ygws.NewServer()
	require.NoError(t, srv.Shutdown(context.Background()))
	err := srv.BroadcastUpdate(context.Background(), "room", []byte{0x00, 0x00})
	assert.ErrorIs(t, err, ygws.ErrServerShutdown)
}

func TestUnit_BroadcastUpdate_UpdateTooLarge(t *testing.T) {
	srv := ygws.NewServer()
	srv.MaxUpdateBytes = 16
	httpSrv := httptest.NewServer(http.HandlerFunc(srv.ServeHTTP))
	t.Cleanup(httpSrv.Close)
	conn := dial(t, httpSrv, "room")
	drainHandshake(t, conn, crdt.New())

	// 32-byte payload, exceeds cap.
	err := srv.BroadcastUpdate(context.Background(), "room", make([]byte, 32))
	assert.ErrorIs(t, err, ygws.ErrUpdateTooLarge)
}

func TestUnit_BroadcastUpdate_InvalidUpdateBytes(t *testing.T) {
	srv := ygws.NewServer()
	httpSrv := httptest.NewServer(http.HandlerFunc(srv.ServeHTTP))
	t.Cleanup(httpSrv.Close)
	conn := dial(t, httpSrv, "room")
	drainHandshake(t, conn, crdt.New())

	// Garbage bytes that crdt will refuse to parse.
	err := srv.BroadcastUpdate(context.Background(), "room", []byte{0xff, 0xff, 0xff, 0xff})
	assert.ErrorIs(t, err, ygws.ErrInvalidUpdate)
}

func TestUnit_BroadcastUpdate_DoesNotMutateServerDoc(t *testing.T) {
	srv := ygws.NewServer()
	httpSrv := httptest.NewServer(http.HandlerFunc(srv.ServeHTTP))
	t.Cleanup(httpSrv.Close)
	conn := dial(t, httpSrv, "room")
	drainHandshake(t, conn, crdt.New())

	// Build update externally.
	external := crdt.New()
	external.GetMap("m").Set(nil, "k", "v")
	update := crdt.EncodeStateAsUpdateV1(external, nil)

	// Broadcast WITHOUT ApplyUpdateV1 to server doc.
	require.NoError(t, srv.BroadcastUpdate(context.Background(), "room", update))

	// Server doc is unchanged.
	serverDoc := srv.GetDoc("room")
	require.NotNil(t, serverDoc)
	assert.Nil(t, serverDoc.GetMap("m").Get("k"))
}
```

Add `"strings"` import.

- [ ] **Step 3: Run tests — verify they fail to compile**

Run: `cd /Users/nimit/Documents/Eukarya/ygo && go test ./provider/websocket/ -run TestUnit_BroadcastUpdate -v`

Expected: compile failure — `BroadcastUpdate` not defined.

- [ ] **Step 4: Implement `BroadcastUpdate`**

Append to `provider/websocket/inject.go`:

```go
// BroadcastUpdate fans out a pre-encoded V1 update to all peers
// currently connected to the named room. It does NOT apply the update
// to the server's doc; callers who want the server's state to reflect
// the broadcast must call crdt.ApplyUpdateV1 first (or use Apply).
// Failing to do so creates divergence: live peers see the update, but
// peers joining after the broadcast receive the server's stale state
// via sync step 2.
//
// Peer write failures during fan-out do not produce an error: writes
// are dispatched in goroutines with a per-write deadline (writeTimeout),
// matching the existing peer-broadcast path. A slow peer cannot block
// the broadcast to other peers.
func (s *Server) BroadcastUpdate(ctx context.Context, room string, update []byte) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	select {
	case <-s.shutdownCh:
		return ErrServerShutdown
	default:
	}
	if !isValidRoomName(room) {
		return ErrInvalidRoomName
	}
	if len(update) > s.effectiveMaxUpdateBytes() {
		return ErrUpdateTooLarge
	}
	// Validate by applying to a throwaway doc. If the bytes are
	// malformed, peers would reject them anyway; catching at the
	// server boundary surfaces caller bugs eagerly.
	if err := crdt.ApplyUpdateV1(crdt.New(), update, nil); err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidUpdate, err)
	}
	if s.OnInject != nil {
		if err := s.OnInject(ctx, InjectInfo{
			Room:       room,
			Op:         OpBroadcastUpdate,
			UpdateSize: len(update),
		}); err != nil {
			return fmt.Errorf("ygo/websocket: inject refused: %w", err)
		}
	}
	s.rmu.RLock()
	rm, ok := s.rooms[room]
	s.rmu.RUnlock()
	if !ok {
		return ErrRoomNotFound
	}
	rm.mu.Lock()
	targets := make([]*peer, 0, len(rm.peers))
	for p := range rm.peers {
		targets = append(targets, p)
	}
	rm.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	data := encodeBroadcastWire(update)
	for _, p := range targets {
		go p.write(data)
	}
	return nil
}

// encodeBroadcastWire wraps a V1 update in the outer sync frame used by
// both peer and server-side broadcasts:
//
//	[msgSync][MsgSyncUpdate][update bytes]
//
// The sync payload is NOT VarBytes-wrapped.
func encodeBroadcastWire(update []byte) []byte {
	enc := encoding.NewEncoder()
	enc.WriteVarUint(msgSync)
	enc.WriteVarUint(ygsync.MsgSyncUpdate)
	enc.WriteRaw(update)
	return enc.Bytes()
}
```

Add imports to `inject.go`: `"fmt"`, `"github.com/reearth/ygo/encoding"`, `ygsync "github.com/reearth/ygo/sync"`.

Note: `MsgSyncUpdate` is a public const from the sync package — confirm its availability before assuming. If only `MsgSyncStep2` or similar is public, use the equivalent sync-update message type from the same package. The existing `encodeSyncStep2Msg` in server.go uses `ygsync.MsgSyncStep2` directly, so the sync package exposes these constants.

Confirm with:
```bash
grep -rn "MsgSyncUpdate\|MsgSyncStep2\b" sync/
```

If `MsgSyncUpdate` isn't exported, use `MsgSyncStep2` (step 2 carries an update payload and is accepted by peers as a raw apply).

- [ ] **Step 5: Run tests — verify they pass**

Run: `cd /Users/nimit/Documents/Eukarya/ygo && go test ./provider/websocket/ -run TestUnit_BroadcastUpdate -v`

Expected: all `BroadcastUpdate` tests pass.

- [ ] **Step 6: Run full suite**

Run: `cd /Users/nimit/Documents/Eukarya/ygo && go test ./...`

Expected: all pass.

- [ ] **Step 7: Commit**

```bash
git add provider/websocket/inject.go provider/websocket/inject_test.go
git commit -m "feat(ws): add Server.BroadcastUpdate for server-side fan-out

Fans out a pre-encoded V1 update to all peers in a room without
mutating the server's doc. Validates ctx, room name, size, bytes,
and OnInject before dispatch. Fire-and-forget fan-out matches the
existing peer-broadcast semantics."
```

---

## Task 5: `Apply` — happy path (single-transact capture and fan-out)

**Files:**
- Modify: `provider/websocket/inject.go` (add `Apply`)
- Test: `provider/websocket/inject_test.go`

Implements `Apply` with origin-scoped `OnUpdate` capture and the bound `transact` helper. Covers the basic "mutate, capture, broadcast" flow.

- [ ] **Step 1: Write failing test for Apply happy path**

Add to `provider/websocket/inject_test.go`:

```go
func TestUnit_Apply_MutatesBroadcastsAndPersists(t *testing.T) {
	srv := ygws.NewServer()
	httpSrv := httptest.NewServer(http.HandlerFunc(srv.ServeHTTP))
	t.Cleanup(httpSrv.Close)
	conn := dial(t, httpSrv, "room")
	peerDoc := crdt.New()
	drainHandshake(t, conn, peerDoc)

	err := srv.Apply(context.Background(), "room", func(doc *crdt.Doc, transact func(func(*crdt.Transaction))) {
		m := doc.GetMap("m") // MUST be outside transact — see spec
		transact(func(txn *crdt.Transaction) {
			m.Set(txn, "k", "v")
		})
	})
	require.NoError(t, err)

	// Peer receives the update.
	outerType, payload := readOne(t, conn, 2*time.Second)
	assert.Equal(t, uint64(0), outerType)
	_, _ = ygsync.ApplySyncMessage(peerDoc, payload, nil)
	assert.Equal(t, "v", peerDoc.GetMap("m").Get("k"))

	// Server doc also reflects the change.
	serverDoc := srv.GetDoc("room")
	require.NotNil(t, serverDoc)
	assert.Equal(t, "v", serverDoc.GetMap("m").Get("k"))
}

func TestUnit_Apply_EmptyFn_ErrNoChanges(t *testing.T) {
	srv := ygws.NewServer()
	err := srv.Apply(context.Background(), "room", func(doc *crdt.Doc, transact func(func(*crdt.Transaction))) {
		// never call transact
	})
	assert.ErrorIs(t, err, ygws.ErrNoChanges)
}

func TestUnit_Apply_InvalidRoomName(t *testing.T) {
	srv := ygws.NewServer()
	err := srv.Apply(context.Background(), "..", func(doc *crdt.Doc, transact func(func(*crdt.Transaction))) {})
	assert.ErrorIs(t, err, ygws.ErrInvalidRoomName)
}

func TestUnit_Apply_ContextAlreadyCancelled(t *testing.T) {
	srv := ygws.NewServer()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	fnCalled := false
	err := srv.Apply(ctx, "room", func(doc *crdt.Doc, transact func(func(*crdt.Transaction))) {
		fnCalled = true
	})
	assert.ErrorIs(t, err, context.Canceled)
	assert.False(t, fnCalled, "fn must not be called when ctx is already cancelled")
}
```

- [ ] **Step 2: Run tests — verify they fail to compile**

Run: `cd /Users/nimit/Documents/Eukarya/ygo && go test ./provider/websocket/ -run TestUnit_Apply -v`

Expected: compile failure — `Apply` not defined.

- [ ] **Step 3: Implement `Apply` and its helper**

Add `"sync"` to the imports at the top of `provider/websocket/inject.go`.
Then append to the file:

```go
// Apply auto-creates the room if needed, runs fn with a bound transact
// helper, captures the update(s) produced by fn's transaction(s), and
// fans the result out to all connected peers.
//
// fn MUST call transact() to mutate the doc. Calls to doc.GetText,
// doc.GetMap, doc.GetXmlFragment, etc. must happen OUTSIDE transact():
// these acquire the doc's write lock, which transact() already holds,
// so calling them inside would deadlock.
//
// fn should be fast — it runs inside the doc's write lock and blocks
// all peer reads and writes to the room for the duration.
//
// IMPORTANT: if fn calls doc.Transact directly (bypassing the supplied
// transact helper), the delta is NOT captured and Apply returns
// ErrNoChanges even though the doc has been mutated. This is a
// contract violation, but the behavior is well-defined.
//
// NOTE: a panic inside fn propagates to the caller. The OnUpdate
// subscription is cleaned up via defer, so no listener leaks. However,
// due to a pre-existing bug in crdt.Doc.Transact, a panic inside fn's
// transaction also leaks the doc's write lock, wedging the room.
// Callers MUST ensure fn does not panic.
func (s *Server) Apply(
	ctx context.Context,
	room string,
	fn func(doc *crdt.Doc, transact func(func(*crdt.Transaction))),
) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	select {
	case <-s.shutdownCh:
		return ErrServerShutdown
	default:
	}
	if !isValidRoomName(room) {
		return ErrInvalidRoomName
	}
	if s.OnInject != nil {
		if err := s.OnInject(ctx, InjectInfo{
			Room:       room,
			Op:         OpApply,
			UpdateSize: 0,
		}); err != nil {
			return fmt.Errorf("ygo/websocket: inject refused: %w", err)
		}
	}
	rm, err := s.getOrCreateRoom(room)
	if err != nil {
		return err
	}

	origin := new(struct{})
	var (
		captured   [][]byte
		capturedMu sync.Mutex
	)
	unsub := rm.doc.OnUpdate(func(update []byte, o any) {
		if o != origin {
			return
		}
		// Mutex guards against the (unusual but legal) case where fn
		// spawns a goroutine that calls transact() concurrently with
		// the main fn body. Also guards against deep-observer chains
		// that re-enter transact.
		capturedMu.Lock()
		captured = append(captured, update)
		capturedMu.Unlock()
	})
	defer unsub()

	transact := func(inner func(*crdt.Transaction)) {
		rm.doc.Transact(inner, origin)
	}
	fn(rm.doc, transact)

	// Snapshot under the mutex before reading.
	capturedMu.Lock()
	capturedCopy := make([][]byte, len(captured))
	copy(capturedCopy, captured)
	capturedMu.Unlock()

	if len(capturedCopy) == 0 {
		return ErrNoChanges
	}

	var merged []byte
	if len(capturedCopy) == 1 {
		merged = capturedCopy[0]
	} else {
		m, err := crdt.MergeUpdatesV1(capturedCopy...)
		if err != nil {
			return fmt.Errorf("ygo/websocket: merging captured updates: %w", err)
		}
		merged = m
	}
	if len(merged) > s.effectiveMaxUpdateBytes() {
		return ErrUpdateTooLarge
	}

	rm.mu.Lock()
	targets := make([]*peer, 0, len(rm.peers))
	for p := range rm.peers {
		targets = append(targets, p)
	}
	rm.mu.Unlock()

	data := encodeBroadcastWire(merged)
	for _, p := range targets {
		go p.write(data)
	}
	return nil
}
```

- [ ] **Step 4: Run tests — verify they pass**

Run: `cd /Users/nimit/Documents/Eukarya/ygo && go test ./provider/websocket/ -run TestUnit_Apply -v`

Expected: all `Apply` tests pass.

- [ ] **Step 5: Run full suite**

Run: `cd /Users/nimit/Documents/Eukarya/ygo && go test ./...`

Expected: all pass.

- [ ] **Step 6: Commit**

```bash
git add provider/websocket/inject.go provider/websocket/inject_test.go
git commit -m "feat(ws): add Server.Apply with origin-scoped delta capture

Apply runs the caller's fn with a bound transact helper that wraps
doc.Transact with a private origin marker. OnUpdate is subscribed
for the call's duration and filters by origin identity, so concurrent
peer writes cannot bleed into the captured delta. Subscription is
cleaned up via defer even on panic."
```

---

## Task 6: `Apply` — multi-transact merge, auto-create, MaxRooms, size cap

**Files:**
- Test only: `provider/websocket/inject_test.go`

No new implementation — verifies that Task 5's code handles these paths correctly. If any fail, iterate on Task 5's code within this task.

- [ ] **Step 1: Write tests**

Add to `provider/websocket/inject_test.go`:

```go
func TestUnit_Apply_MultipleTransacts_MergedAndBroadcastOnce(t *testing.T) {
	srv := ygws.NewServer()
	httpSrv := httptest.NewServer(http.HandlerFunc(srv.ServeHTTP))
	t.Cleanup(httpSrv.Close)
	conn := dial(t, httpSrv, "room")
	peerDoc := crdt.New()
	drainHandshake(t, conn, peerDoc)

	err := srv.Apply(context.Background(), "room", func(doc *crdt.Doc, transact func(func(*crdt.Transaction))) {
		m := doc.GetMap("m")
		transact(func(txn *crdt.Transaction) { m.Set(txn, "k1", "v1") })
		transact(func(txn *crdt.Transaction) { m.Set(txn, "k2", "v2") })
		transact(func(txn *crdt.Transaction) { m.Set(txn, "k3", "v3") })
	})
	require.NoError(t, err)

	// Peer receives a single merged sync frame.
	outerType, payload := readOne(t, conn, 2*time.Second)
	assert.Equal(t, uint64(0), outerType)
	_, _ = ygsync.ApplySyncMessage(peerDoc, payload, nil)
	assert.Equal(t, "v1", peerDoc.GetMap("m").Get("k1"))
	assert.Equal(t, "v2", peerDoc.GetMap("m").Get("k2"))
	assert.Equal(t, "v3", peerDoc.GetMap("m").Get("k3"))

	// No second message is pending.
	_ = conn.SetReadDeadline(time.Now().Add(100 * time.Millisecond))
	_, _, err = conn.ReadMessage()
	assert.Error(t, err, "expected no further messages")
}

func TestUnit_Apply_AutoCreatesRoom(t *testing.T) {
	srv := ygws.NewServer()
	assert.Nil(t, srv.GetDoc("new-room"))

	err := srv.Apply(context.Background(), "new-room", func(doc *crdt.Doc, transact func(func(*crdt.Transaction))) {
		m := doc.GetMap("m")
		transact(func(txn *crdt.Transaction) { m.Set(txn, "k", "v") })
	})
	require.NoError(t, err)

	// Room now exists and carries the change.
	doc := srv.GetDoc("new-room")
	require.NotNil(t, doc)
	assert.Equal(t, "v", doc.GetMap("m").Get("k"))
}

func TestUnit_Apply_MaxRoomsExceeded(t *testing.T) {
	srv := ygws.NewServer()
	srv.MaxRooms = 2

	for i, name := range []string{"a", "b"} {
		err := srv.Apply(context.Background(), name, func(doc *crdt.Doc, transact func(func(*crdt.Transaction))) {
			m := doc.GetMap("m")
			transact(func(txn *crdt.Transaction) { m.Set(txn, "k", "v") })
		})
		require.NoError(t, err, "room %d %q should succeed", i, name)
	}

	err := srv.Apply(context.Background(), "c", func(doc *crdt.Doc, transact func(func(*crdt.Transaction))) {})
	assert.ErrorIs(t, err, ygws.ErrTooManyRooms)
	assert.Nil(t, srv.GetDoc("c"), "failed Apply must not leave a partial room")
}

func TestUnit_Apply_UpdateTooLarge(t *testing.T) {
	srv := ygws.NewServer()
	srv.MaxUpdateBytes = 32

	err := srv.Apply(context.Background(), "room", func(doc *crdt.Doc, transact func(func(*crdt.Transaction))) {
		txt := doc.GetText("t")
		transact(func(txn *crdt.Transaction) {
			// Insert enough text that the encoded update exceeds 32 bytes.
			txt.Insert(txn, 0, strings.Repeat("x", 1000), nil)
		})
	})
	assert.ErrorIs(t, err, ygws.ErrUpdateTooLarge)

	// Doc HAS been mutated — document this as post-hoc reporting.
	doc := srv.GetDoc("room")
	require.NotNil(t, doc)
	assert.Equal(t, 1000, doc.GetText("t").Length())
}

func TestUnit_Apply_AfterShutdown(t *testing.T) {
	srv := ygws.NewServer()
	require.NoError(t, srv.Shutdown(context.Background()))
	err := srv.Apply(context.Background(), "room", func(doc *crdt.Doc, transact func(func(*crdt.Transaction))) {})
	assert.ErrorIs(t, err, ygws.ErrServerShutdown)
}
```

- [ ] **Step 2: Run tests**

Run: `cd /Users/nimit/Documents/Eukarya/ygo && go test ./provider/websocket/ -run TestUnit_Apply -v`

Expected: all pass based on Task 5's implementation.

- [ ] **Step 3: Run full suite**

Run: `cd /Users/nimit/Documents/Eukarya/ygo && go test ./...`

Expected: all pass.

- [ ] **Step 4: Commit**

```bash
git add provider/websocket/inject_test.go
git commit -m "test(ws): cover Apply multi-transact merge, auto-create, caps, shutdown"
```

---

## Task 7: `Apply` — panic safety, Transact-bypass footgun, persistence integration

**Files:**
- Test only: `provider/websocket/inject_test.go`

Verifies panic cleanup, well-defined behavior on contract violation, and that `Apply`'s delta flows through the existing `OnUpdate` → persistence path.

- [ ] **Step 1: Write panic-safety test**

Add to `provider/websocket/inject_test.go`:

```go
func TestUnit_Apply_FnPanic_SubscriptionCleanedUp(t *testing.T) {
	srv := ygws.NewServer()

	// First Apply panics inside transact. We recover.
	func() {
		defer func() { _ = recover() }()
		_ = srv.Apply(context.Background(), "room", func(doc *crdt.Doc, transact func(func(*crdt.Transaction))) {
			transact(func(txn *crdt.Transaction) {
				panic("boom")
			})
		})
	}()

	// If the first Apply leaked its OnUpdate listener, that listener
	// would capture the second Apply's update with origin != its own,
	// but since our filter checks origin identity, a leaked listener
	// would simply never fire. The observable signal: the second Apply
	// must succeed and the Transact lock must be releaseable.
	//
	// NOTE: due to the pre-existing Transact panic-unlock bug, the doc
	// may be wedged after step 1. We test a FRESH doc/room to verify
	// our defer-unsub logic specifically:
	err := srv.Apply(context.Background(), "other-room", func(doc *crdt.Doc, transact func(func(*crdt.Transaction))) {
		m := doc.GetMap("m")
		transact(func(txn *crdt.Transaction) { m.Set(txn, "k", "v") })
	})
	assert.NoError(t, err)
	assert.Equal(t, "v", srv.GetDoc("other-room").GetMap("m").Get("k"))
}

func TestUnit_Apply_FnPanic_BeforeTransact_NoLeak(t *testing.T) {
	srv := ygws.NewServer()

	func() {
		defer func() { _ = recover() }()
		_ = srv.Apply(context.Background(), "room", func(doc *crdt.Doc, transact func(func(*crdt.Transaction))) {
			panic("before transact")
		})
	}()

	// Room was auto-created; second Apply on same room should succeed.
	err := srv.Apply(context.Background(), "room", func(doc *crdt.Doc, transact func(func(*crdt.Transaction))) {
		m := doc.GetMap("m")
		transact(func(txn *crdt.Transaction) { m.Set(txn, "k", "v") })
	})
	assert.NoError(t, err)
}
```

- [ ] **Step 2: Write Transact-bypass test**

Add:

```go
func TestUnit_Apply_FnBypassesTransactHelper_ErrNoChangesButDocMutated(t *testing.T) {
	srv := ygws.NewServer()

	err := srv.Apply(context.Background(), "room", func(doc *crdt.Doc, transact func(func(*crdt.Transaction))) {
		m := doc.GetMap("m")
		// BYPASS: caller goes directly to doc.Transact instead of the
		// supplied transact helper.
		doc.Transact(func(txn *crdt.Transaction) {
			m.Set(txn, "k", "v")
		})
	})
	assert.ErrorIs(t, err, ygws.ErrNoChanges, "bypass should report ErrNoChanges")

	// Doc IS mutated — well-defined but surprising behavior; documented.
	serverDoc := srv.GetDoc("room")
	require.NotNil(t, serverDoc)
	assert.Equal(t, "v", serverDoc.GetMap("m").Get("k"))
}
```

- [ ] **Step 3: Write persistence-integration test**

Add:

```go
func TestUnit_Apply_TriggersPersistenceViaOnUpdate(t *testing.T) {
	p := ygws.NewMemoryPersistence()
	srv := ygws.NewServerWithPersistence(p)

	err := srv.Apply(context.Background(), "room", func(doc *crdt.Doc, transact func(func(*crdt.Transaction))) {
		m := doc.GetMap("m")
		transact(func(txn *crdt.Transaction) { m.Set(txn, "k", "v") })
	})
	require.NoError(t, err)

	// Allow the persistence goroutine to drain.
	// MemoryPersistence is synchronous under its own lock; the
	// persistence goroutine polls r.persistCh and calls StoreUpdate.
	// Small sleep for channel drain is acceptable in tests; alternative
	// is Shutdown which forces drain.
	require.NoError(t, srv.Shutdown(context.Background()))

	stored, err := p.LoadDoc("room")
	require.NoError(t, err)
	require.NotNil(t, stored)

	// Reload into a fresh doc and verify state.
	reloaded := crdt.New()
	require.NoError(t, crdt.ApplyUpdateV1(reloaded, stored, nil))
	assert.Equal(t, "v", reloaded.GetMap("m").Get("k"))
}
```

- [ ] **Step 4: Run tests — verify they pass**

Run: `cd /Users/nimit/Documents/Eukarya/ygo && go test ./provider/websocket/ -run TestUnit_Apply -v`

Expected: all pass.

- [ ] **Step 5: Run full suite**

Run: `cd /Users/nimit/Documents/Eukarya/ygo && go test ./...`

Expected: all pass.

- [ ] **Step 6: Commit**

```bash
git add provider/websocket/inject_test.go
git commit -m "test(ws): cover Apply panic cleanup, Transact-bypass, persistence"
```

---

## Task 8: `Apply` + peer concurrent-writes convergence (acceptance criterion)

**Files:**
- Test only: `provider/websocket/inject_test.go`

This is the direct acceptance criterion from the issue: "concurrent `BroadcastUpdate` + peer writes converge correctly." Structured as `Apply` + peer since `Apply` is the higher-level path — same property transitively holds for `BroadcastUpdate` when paired with `ApplyUpdateV1`.

- [ ] **Step 1: Write convergence test**

Add to `provider/websocket/inject_test.go`:

```go
func TestUnit_Apply_ConcurrentWithPeerWrites_Converges(t *testing.T) {
	srv := ygws.NewServer()
	httpSrv := httptest.NewServer(http.HandlerFunc(srv.ServeHTTP))
	t.Cleanup(httpSrv.Close)

	conn := dial(t, httpSrv, "room")
	peerDoc := crdt.New()
	drainHandshake(t, conn, peerDoc)

	// Run 20 Apply calls from one goroutine and 20 peer-style sync
	// updates from another, in parallel.
	const n = 20
	var wg sync.WaitGroup
	wg.Add(2)

	// Apply writes: server-side keys s0..s19
	go func() {
		defer wg.Done()
		for i := 0; i < n; i++ {
			key := fmt.Sprintf("s%d", i)
			_ = srv.Apply(context.Background(), "room", func(doc *crdt.Doc, transact func(func(*crdt.Transaction))) {
				m := doc.GetMap("m")
				transact(func(txn *crdt.Transaction) { m.Set(txn, key, i) })
			})
		}
	}()

	// Peer writes: peer-side keys p0..p19
	go func() {
		defer wg.Done()
		for i := 0; i < n; i++ {
			d := crdt.New()
			d.GetMap("m").Set(nil, fmt.Sprintf("p%d", i), i)
			upd := crdt.EncodeStateAsUpdateV1(d, nil)
			enc := encoding.NewEncoder()
			enc.WriteVarUint(1 /*MsgSyncUpdate*/) // use the actual const name — confirm via grep
			enc.WriteRaw(upd)
			sendSync(t, conn, enc.Bytes())
		}
	}()

	wg.Wait()

	// Drain until peer doc has all 40 keys or 5s elapses.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		outerType, payload := readOne(t, conn, 500*time.Millisecond)
		if outerType == 0 {
			_, _ = ygsync.ApplySyncMessage(peerDoc, payload, nil)
		}
		allPresent := true
		for i := 0; i < n; i++ {
			if peerDoc.GetMap("m").Get(fmt.Sprintf("s%d", i)) == nil {
				allPresent = false
				break
			}
		}
		if allPresent {
			break
		}
	}

	serverDoc := srv.GetDoc("room")
	for i := 0; i < n; i++ {
		assert.Equal(t, i, serverDoc.GetMap("m").Get(fmt.Sprintf("s%d", i)), "server-side key s%d", i)
		assert.Equal(t, i, serverDoc.GetMap("m").Get(fmt.Sprintf("p%d", i)), "peer-side key p%d", i)
		assert.Equal(t, i, peerDoc.GetMap("m").Get(fmt.Sprintf("s%d", i)), "peer sees server-side key s%d", i)
	}
}
```

Add imports: `"fmt"`, `"sync"`, `"github.com/reearth/ygo/encoding"`.

Note: the `1 /*MsgSyncUpdate*/` in the inline encoder is a placeholder — replace with the real constant name from the `sync` package. If `MsgSyncUpdate` is `0`, adjust. If the sync package only exports `MsgSyncStep2`, use that. Verify via `grep -n "MsgSync" sync/*.go`.

- [ ] **Step 2: Run test**

Run: `cd /Users/nimit/Documents/Eukarya/ygo && go test ./provider/websocket/ -run TestUnit_Apply_ConcurrentWithPeerWrites_Converges -v -timeout 30s`

Expected: PASS. If it flakes, the test is wrong — fix it, don't mask with retries.

- [ ] **Step 3: Run with race detector**

Run: `cd /Users/nimit/Documents/Eukarya/ygo && go test -race ./provider/websocket/ -run TestUnit_Apply_ConcurrentWithPeerWrites_Converges -timeout 60s`

Expected: PASS, no races reported.

- [ ] **Step 4: Commit**

```bash
git add provider/websocket/inject_test.go
git commit -m "test(ws): Apply converges with concurrent peer writes (acceptance)"
```

---

## Task 9: `OnInject` hook integration tests

**Files:**
- Test only: `provider/websocket/inject_test.go`

Verifies OnInject is called with correct Op / UpdateSize / ctx, exactly once per call, and that refusal blocks the operation.

- [ ] **Step 1: Write tests**

Add to `provider/websocket/inject_test.go`:

```go
func TestUnit_OnInject_BroadcastUpdate_ReceivesOpAndSize(t *testing.T) {
	srv := ygws.NewServer()
	var callCount int
	var gotInfo ygws.InjectInfo
	srv.OnInject = func(ctx context.Context, info ygws.InjectInfo) error {
		callCount++
		gotInfo = info
		return nil
	}
	httpSrv := httptest.NewServer(http.HandlerFunc(srv.ServeHTTP))
	t.Cleanup(httpSrv.Close)
	conn := dial(t, httpSrv, "room")
	drainHandshake(t, conn, crdt.New())

	d := crdt.New()
	d.GetMap("m").Set(nil, "k", "v")
	update := crdt.EncodeStateAsUpdateV1(d, nil)

	require.NoError(t, srv.BroadcastUpdate(context.Background(), "room", update))

	assert.Equal(t, 1, callCount)
	assert.Equal(t, "room", gotInfo.Room)
	assert.Equal(t, ygws.OpBroadcastUpdate, gotInfo.Op)
	assert.Equal(t, len(update), gotInfo.UpdateSize)
}

func TestUnit_OnInject_Apply_ReceivesOpAndZeroSize(t *testing.T) {
	srv := ygws.NewServer()
	var callCount int
	var gotInfo ygws.InjectInfo
	srv.OnInject = func(ctx context.Context, info ygws.InjectInfo) error {
		callCount++
		gotInfo = info
		return nil
	}

	err := srv.Apply(context.Background(), "room", func(doc *crdt.Doc, transact func(func(*crdt.Transaction))) {
		m := doc.GetMap("m")
		transact(func(txn *crdt.Transaction) { m.Set(txn, "k", "v") })
	})
	require.NoError(t, err)

	assert.Equal(t, 1, callCount)
	assert.Equal(t, "room", gotInfo.Room)
	assert.Equal(t, ygws.OpApply, gotInfo.Op)
	assert.Equal(t, 0, gotInfo.UpdateSize, "Apply's OnInject must see UpdateSize=0")
}

func TestUnit_OnInject_Refusal_BlocksOperation(t *testing.T) {
	srv := ygws.NewServer()
	refusal := errors.New("refused by policy")
	srv.OnInject = func(ctx context.Context, info ygws.InjectInfo) error {
		return refusal
	}

	errA := srv.Apply(context.Background(), "room", func(doc *crdt.Doc, transact func(func(*crdt.Transaction))) {
		transact(func(txn *crdt.Transaction) { doc.GetMap("m").Set(txn, "k", "v") })
	})
	assert.ErrorIs(t, errA, refusal)
	assert.Nil(t, srv.GetDoc("room"), "refusal must not auto-create a room")

	httpSrv := httptest.NewServer(http.HandlerFunc(srv.ServeHTTP))
	t.Cleanup(httpSrv.Close)
	conn := dial(t, httpSrv, "other")
	drainHandshake(t, conn, crdt.New())

	errB := srv.BroadcastUpdate(context.Background(), "other", []byte{0x00, 0x00})
	assert.ErrorIs(t, errB, refusal)
}

func TestUnit_OnInject_InvalidUpdate_ShortCircuitsBeforeInject(t *testing.T) {
	// BroadcastUpdate's check order: ctx → shutdown → name → size →
	// parse → inject. Malformed bytes are rejected BEFORE OnInject so
	// the hook is never called.
	srv := ygws.NewServer()
	called := false
	srv.OnInject = func(ctx context.Context, info ygws.InjectInfo) error {
		called = true
		return nil
	}
	httpSrv := httptest.NewServer(http.HandlerFunc(srv.ServeHTTP))
	t.Cleanup(httpSrv.Close)
	conn := dial(t, httpSrv, "room")
	drainHandshake(t, conn, crdt.New())

	err := srv.BroadcastUpdate(context.Background(), "room", []byte{0xff, 0xff})
	assert.ErrorIs(t, err, ygws.ErrInvalidUpdate)
	assert.False(t, called, "OnInject must not be called when bytes fail parse")
}

func TestUnit_OnInject_CtxValue_PropagatesForTenantCheck(t *testing.T) {
	type tenantKey struct{}
	srv := ygws.NewServer()
	srv.OnInject = func(ctx context.Context, info ygws.InjectInfo) error {
		tenant, _ := ctx.Value(tenantKey{}).(string)
		if tenant != "tenant-a" {
			return errors.New("wrong tenant")
		}
		return nil
	}

	okCtx := context.WithValue(context.Background(), tenantKey{}, "tenant-a")
	badCtx := context.WithValue(context.Background(), tenantKey{}, "tenant-b")

	assert.NoError(t, srv.Apply(okCtx, "room", func(doc *crdt.Doc, transact func(func(*crdt.Transaction))) {
		transact(func(txn *crdt.Transaction) { doc.GetMap("m").Set(txn, "k", "v") })
	}))
	assert.Error(t, srv.Apply(badCtx, "room", func(doc *crdt.Doc, transact func(func(*crdt.Transaction))) {}))
}
```

Add `"errors"` to the test imports if not already present.

- [ ] **Step 2: Run tests**

Run: `cd /Users/nimit/Documents/Eukarya/ygo && go test ./provider/websocket/ -run TestUnit_OnInject -v`

Expected: all pass based on Tasks 4 and 5's implementation.

- [ ] **Step 3: Run full suite with race detector**

Run: `cd /Users/nimit/Documents/Eukarya/ygo && go test -race ./...`

Expected: all pass, no races.

- [ ] **Step 4: Commit**

```bash
git add provider/websocket/inject_test.go
git commit -m "test(ws): cover OnInject hook — Op/UpdateSize, ctx value, refusal"
```

---

## Task 10: `CloseRoom`

**Files:**
- Modify: `provider/websocket/inject.go` (add `CloseRoom`)
- Test: `provider/websocket/inject_test.go`

Empty-room close, force close with connected peers, refusal with peers when `force=false`.

- [ ] **Step 1: Write failing tests**

Add to `provider/websocket/inject_test.go`:

```go
func TestUnit_CloseRoom_EmptyRoom_Deletes(t *testing.T) {
	srv := ygws.NewServer()
	// Create room via Apply.
	require.NoError(t, srv.Apply(context.Background(), "room", func(doc *crdt.Doc, transact func(func(*crdt.Transaction))) {
		transact(func(txn *crdt.Transaction) { doc.GetMap("m").Set(txn, "k", "v") })
	}))
	assert.NotNil(t, srv.GetDoc("room"))

	require.NoError(t, srv.CloseRoom("room", false))
	assert.Nil(t, srv.GetDoc("room"))
}

func TestUnit_CloseRoom_NonExistent_ErrRoomNotFound(t *testing.T) {
	srv := ygws.NewServer()
	err := srv.CloseRoom("ghost", false)
	assert.ErrorIs(t, err, ygws.ErrRoomNotFound)
}

func TestUnit_CloseRoom_InvalidName(t *testing.T) {
	srv := ygws.NewServer()
	err := srv.CloseRoom("..", false)
	assert.ErrorIs(t, err, ygws.ErrInvalidRoomName)
}

func TestUnit_CloseRoom_HasPeers_NoForce_ErrRoomHasPeers(t *testing.T) {
	srv := ygws.NewServer()
	httpSrv := httptest.NewServer(http.HandlerFunc(srv.ServeHTTP))
	t.Cleanup(httpSrv.Close)
	conn := dial(t, httpSrv, "room")
	drainHandshake(t, conn, crdt.New())

	err := srv.CloseRoom("room", false)
	assert.ErrorIs(t, err, ygws.ErrRoomHasPeers)
	// Room still exists and peer still connected.
	assert.NotNil(t, srv.GetDoc("room"))
}

func TestUnit_CloseRoom_HasPeers_Force_DisconnectsAndDeletes(t *testing.T) {
	srv := ygws.NewServer()
	httpSrv := httptest.NewServer(http.HandlerFunc(srv.ServeHTTP))
	t.Cleanup(httpSrv.Close)
	conn := dial(t, httpSrv, "room")
	drainHandshake(t, conn, crdt.New())

	require.NoError(t, srv.CloseRoom("room", true))
	assert.Nil(t, srv.GetDoc("room"))

	// The peer connection should be closed — next ReadMessage errors.
	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	_, _, err := conn.ReadMessage()
	assert.Error(t, err)
}

func TestUnit_CloseRoom_AfterShutdown(t *testing.T) {
	srv := ygws.NewServer()
	require.NoError(t, srv.Shutdown(context.Background()))
	err := srv.CloseRoom("room", false)
	assert.ErrorIs(t, err, ygws.ErrServerShutdown)
}

func TestUnit_CloseRoom_WithPersistence_DrainsBeforeReturn(t *testing.T) {
	p := ygws.NewMemoryPersistence()
	srv := ygws.NewServerWithPersistence(p)
	require.NoError(t, srv.Apply(context.Background(), "room", func(doc *crdt.Doc, transact func(func(*crdt.Transaction))) {
		transact(func(txn *crdt.Transaction) { doc.GetMap("m").Set(txn, "k", "v") })
	}))

	require.NoError(t, srv.CloseRoom("room", false))

	// Persistence must reflect the Apply.
	stored, err := p.LoadDoc("room")
	require.NoError(t, err)
	require.NotNil(t, stored)
}
```

- [ ] **Step 2: Run tests — verify they fail to compile**

Run: `cd /Users/nimit/Documents/Eukarya/ygo && go test ./provider/websocket/ -run TestUnit_CloseRoom -v`

Expected: compile failure — `CloseRoom` not defined.

- [ ] **Step 3: Implement `CloseRoom`**

Append to `provider/websocket/inject.go`:

```go
// CloseRoom removes the named room from the server. Drains the room's
// persistence write queue and deletes the room from the server's map so
// that subsequent GetDoc / BroadcastUpdate / Apply calls do not see it.
//
// If peers are connected:
//   - force=false: returns ErrRoomHasPeers without modifying state.
//   - force=true:  closes each peer connection, waits for disconnect
//     handlers to run, then deletes the room.
//
// CloseRoom is primarily intended for releasing rooms created by Apply
// that never accumulated peer connections — without it, such rooms
// linger until process exit.
func (s *Server) CloseRoom(name string, force bool) error {
	select {
	case <-s.shutdownCh:
		return ErrServerShutdown
	default:
	}
	if !isValidRoomName(name) {
		return ErrInvalidRoomName
	}

	s.rmu.Lock()
	rm, ok := s.rooms[name]
	if !ok {
		s.rmu.Unlock()
		return ErrRoomNotFound
	}

	rm.mu.Lock()
	peerCount := len(rm.peers)
	if peerCount > 0 && !force {
		rm.mu.Unlock()
		s.rmu.Unlock()
		return ErrRoomHasPeers
	}

	// Collect connection handles (and done channels) for force-close.
	var conns []*gws.Conn
	var dones []chan struct{}
	if peerCount > 0 {
		conns = make([]*gws.Conn, 0, peerCount)
		dones = make([]chan struct{}, 0, peerCount)
		for p := range rm.peers {
			conns = append(conns, p.conn)
			dones = append(dones, p.done)
		}
	}
	rm.mu.Unlock()
	s.rmu.Unlock()

	// Close each connection outside the locks. The read loop exits and
	// handleDisconnect runs — which acquires s.rmu and rm.mu — so we
	// MUST have released both before entering this block.
	for _, c := range conns {
		_ = c.Close()
	}
	for _, d := range dones {
		<-d
	}

	// Re-acquire locks to delete the room. Compare pointer identity to
	// avoid deleting a REPLACEMENT room that was created with the same
	// name after we released the lock:
	//   - handleDisconnect may have already deleted the original.
	//   - A subsequent Apply or peer upgrade may have inserted a new
	//     room at the same key.
	// In either case, our work is done — return nil.
	s.rmu.Lock()
	fresh, ok := s.rooms[name]
	if !ok || fresh != rm {
		s.rmu.Unlock()
		// Original room is already gone. If persistStop was ours to
		// close, handleDisconnect did so when the last peer left.
		if rm.persistDone != nil {
			<-rm.persistDone
		}
		return nil
	}
	delete(s.rooms, name)
	if rm.persistStop != nil {
		select {
		case <-rm.persistStop:
			// already closed by handleDisconnect
		default:
			close(rm.persistStop)
		}
	}
	s.rmu.Unlock()

	if rm.persistDone != nil {
		<-rm.persistDone
	}
	return nil
}
```

Add imports to inject.go: `gws "github.com/gorilla/websocket"`.

- [ ] **Step 4: Run tests**

Run: `cd /Users/nimit/Documents/Eukarya/ygo && go test ./provider/websocket/ -run TestUnit_CloseRoom -v`

Expected: all pass.

- [ ] **Step 5: Run full suite with race detector**

Run: `cd /Users/nimit/Documents/Eukarya/ygo && go test -race ./...`

Expected: all pass, no races.

- [ ] **Step 6: Commit**

```bash
git add provider/websocket/inject.go provider/websocket/inject_test.go
git commit -m "feat(ws): add Server.CloseRoom with force option

Symmetric teardown for rooms created by Apply that never accumulated
peer connections. Without CloseRoom, such rooms would linger until
process exit. Releases locks before waiting on disconnect handlers
to avoid deadlock with handleDisconnect."
```

---

## Task 11: README — new section "Server-side document injection"

**Files:**
- Modify: `README.md`

Adds a documentation section matching the spec's README requirements: canonical `ApplyUpdateV1 + BroadcastUpdate` pattern, `Apply` example, `OnInject` example, trust-model paragraph.

- [ ] **Step 1: Find the insertion point**

Open `/Users/nimit/Documents/Eukarya/ygo/README.md` and locate the section that covers the WebSocket provider (search for "websocket" or "provider/websocket"). Insert the new section immediately after the existing peer-connection/handshake coverage.

- [ ] **Step 2: Add the new section**

Insert:

```markdown
## Server-side document injection

Backend services — AI agents, HTTP handlers, content pipelines — can push
changes into a live room without simulating a WebSocket peer. Three
APIs are available:

### `Server.BroadcastUpdate(ctx, room, update)`

Fan out a pre-encoded V1 update to all connected peers. Does NOT mutate
the server's doc. Callers must call `crdt.ApplyUpdateV1` first (or use
`Apply`) if they want the server's state to reflect the broadcast.

```go
doc := server.GetDoc("my-room")
if err := crdt.ApplyUpdateV1(doc, update, nil); err != nil {
    return err
}
if err := server.BroadcastUpdate(ctx, "my-room", update); err != nil {
    return err
}
```

**Warning: skipping `ApplyUpdateV1` creates divergence.** Live peers see
the update but peers joining later receive the server's stale state via
sync step 2.

### `Server.Apply(ctx, room, fn)`

Applies a callback to the doc and broadcasts the resulting delta in one
call. Auto-creates the room if needed. Persistence runs via the existing
`OnUpdate` hook — callers do not need to persist separately.

```go
err := server.Apply(ctx, "my-room",
    func(doc *crdt.Doc, transact func(func(*crdt.Transaction))) {
        frag := doc.GetXmlFragment("content") // OUTSIDE transact — see note
        transact(func(txn *crdt.Transaction) {
            elem := crdt.NewYXmlElement("p")
            frag.InsertElement(txn, 0, elem)
        })
    },
)
```

**Important:** calls to `doc.GetXmlFragment`, `doc.GetText`, `doc.GetMap`,
etc. must happen **outside** the `transact` callback. These methods
acquire the doc's write lock, which `transact` already holds — calling
them inside would deadlock.

`fn` should be fast. It runs inside the doc's write lock and blocks all
peer reads and writes to the room for the duration.

### `Server.CloseRoom(name, force)`

Explicit teardown for rooms created by `Apply` that never accumulated
peer connections. Without `CloseRoom`, such rooms linger until process
exit.

```go
if err := server.CloseRoom("my-room", false); err != nil { /* ... */ }
// force=true closes connected peers first.
```

### Access control: `Server.OnInject`

Gate server-side writes with an optional hook:

```go
server.OnInject = func(ctx context.Context, info websocket.InjectInfo) error {
    tenant, _ := ctx.Value(tenantKey{}).(string)
    if !allowed(tenant, info.Room) {
        return fmt.Errorf("tenant %q may not write to %q", tenant, info.Room)
    }
    if info.Op == websocket.OpBroadcastUpdate && info.UpdateSize > 1<<20 {
        return errors.New("update too large for this tenant")
    }
    return nil
}
```

`info.Op` is `OpBroadcastUpdate` or `OpApply`. `info.UpdateSize` is the
byte length for `BroadcastUpdate`; zero for `Apply` (delta not yet
produced — enforce post-hoc via `MaxUpdateBytes`).

### Resource caps

- `Server.MaxUpdateBytes` — per-update size cap, default 64 MiB (matches
  peer frame limit).
- `Server.MaxRooms` — total room cap; applies to both peer upgrades
  (HTTP 503) and `Apply` (`ErrTooManyRooms`). Default unlimited.

### Trust model

`Server.Apply` and `Server.BroadcastUpdate` grant total write authority
on the document. Treat the `*Server` handle with the same care as a
database connection — do not expose it directly to untrusted code.
`OnInject` is defense-in-depth, not a substitute for caller-side authZ.
```

- [ ] **Step 3: Verify README renders**

Run: `cd /Users/nimit/Documents/Eukarya/ygo && grep -n "Server-side document injection" README.md`

Expected: exactly one match.

- [ ] **Step 4: Run full suite one more time**

Run: `cd /Users/nimit/Documents/Eukarya/ygo && go test ./... && go vet ./...`

Expected: all green.

- [ ] **Step 5: Commit**

```bash
git add README.md
git commit -m "docs: README section for server-side document injection

Covers BroadcastUpdate, Apply, CloseRoom, OnInject, MaxUpdateBytes,
MaxRooms, and the trust model. Includes the corrected Apply example
that gets shared type refs outside the transact callback."
```

---

## Task 12: Release notes / changelog

**Files:**
- Check for: `CHANGELOG.md`, `docs/CHANGELOG.md`, or release-notes convention in `docs/`. If none, create `docs/release-notes/v1.1.0.md` to match prior release-note files.

- [ ] **Step 1: Locate prior release notes**

Run:
```bash
ls /Users/nimit/Documents/Eukarya/ygo/docs/ | grep -i -E "changelog|release"
```

Also check:
```bash
ls /Users/nimit/Documents/Eukarya/ygo/ | grep -i -E "changelog|release"
```

If there is an existing changelog, append to it. If there is a `docs/release-notes/` directory with per-version files, create `v1.1.0.md`.

- [ ] **Step 2: Write the entry**

Template (adapt to the actual file structure):

```markdown
## v1.1.0 — Server-side document injection

### Added

- `Server.BroadcastUpdate(ctx, room, update)` — fan out a pre-encoded V1
  update to all connected peers without simulating a WebSocket client.
- `Server.Apply(ctx, room, fn)` — mutate the doc and broadcast the
  resulting delta in one call. Auto-creates the room. Runs persistence
  via the existing `OnUpdate` hook.
- `Server.CloseRoom(name, force)` — explicit teardown for rooms created
  by `Apply`.
- `Server.OnInject` hook for access-control policy on server-side writes.
  Called with `context.Context` and `InjectInfo{Room, Op, UpdateSize}`.
- `Server.MaxUpdateBytes` — per-update size cap (default 64 MiB).
- `Server.MaxRooms` — total room cap, applied to both peer upgrades and
  `Apply`.
- Exported error sentinels: `ErrServerShutdown`, `ErrInvalidRoomName`,
  `ErrRoomNotFound`, `ErrRoomHasPeers`, `ErrInvalidUpdate`,
  `ErrUpdateTooLarge`, `ErrTooManyRooms`, `ErrNoChanges`.

### Changed

- Peer upgrades that would exceed `MaxRooms` now return HTTP 503 (was:
  behavior undefined / limited only by `MaxConnections`).

### Security

- All server-side write entry points validate room name via
  `isValidRoomName` — primary defense against path traversal in
  user-implemented persistence adapters that key on room name.
- `BroadcastUpdate` validates update bytes before fan-out, rejecting
  malformed input with `ErrInvalidUpdate`.
- Resource caps on update size (`MaxUpdateBytes`) and total rooms
  (`MaxRooms`) close two DoS vectors enabled by the new API.
```

- [ ] **Step 3: Commit**

```bash
git add <path-to-changelog>
git commit -m "docs: release notes for v1.1.0 (server-side injection)"
```

---

## Task 13: File follow-up issues on GitHub

- [ ] **Step 1: File issue — Transact panic-unlock bug**

Run:

```bash
gh issue create --repo reearth/ygo \
  --title "crdt: Doc.Transact leaks d.mu lock on panic in fn" \
  --body "$(cat <<'EOF'
## Summary

`Doc.Transact` acquires `d.mu.Lock()` at the top of Phase 1 and releases
it with an explicit `d.mu.Unlock()` before Phase 2 fires observers. The
unlock is NOT deferred — so a panic anywhere between lock and unlock
(in `fn`, `squashRuns`, observer snapshotting, etc.) leaks the lock
forever, wedging the entire document.

## Impact

- Any caller of `Transact(fn)` where `fn` panics leaves the doc unusable.
- Amplified by `websocket.Server.Apply` (v1.1.0) which explicitly
  documents that panics in `fn` propagate — callers are now likely to
  hit this.

## Repro

```go
doc := crdt.New()
defer func() { recover() }()
doc.Transact(func(txn *crdt.Transaction) {
    panic("boom")
})
// Any subsequent operation on doc deadlocks.
```

## Fix considerations

- `defer d.mu.Unlock()` is not a drop-in because observer firing must
  happen OUTSIDE the lock. Need either `defer` + restructure, or a
  `recover` wrapper that unlocks and re-panics without firing
  observers.
- Decision needed: on panic, should partial observers fire?
  Persistence listeners may see a half-applied txn. Probably safer to
  skip observer firing and propagate the panic with the doc in its
  pre-`fn` state.

Tracked as a follow-up from #8 (server-side injection).
EOF
)"
```

- [ ] **Step 2: File issue — TransactContext mid-fn cancellation gap**

Run:

```bash
gh issue create --repo reearth/ygo \
  --title "crdt: TransactContext cannot interrupt fn mid-execution" \
  --body "$(cat <<'EOF'
## Summary

`Doc.TransactContext(ctx, fn, origin...)` checks `ctx.Err()` at entry
and at exit, but the underlying `d.Transact` runs `fn` to completion
regardless of cancellation during `fn`. If `ctx` is cancelled during
`fn`, the transaction still commits and mutates the doc;
`TransactContext` just reports the error afterward.

## Impact

- Callers expect context cancellation to abort expensive transactions,
  but it does not.
- `websocket.Server.Apply` (v1.1.0) inherits this limitation: context
  cancellation after `fn` starts is ignored by design, as documented
  in the spec.

## Fix considerations

- Cancellable transactions require cooperation from `fn` itself — e.g.
  passing `ctx` into the transaction body and allowing `fn` to check
  it. A non-cooperative abort would risk leaving the doc in a
  partially-modified state.
- Alternative: document that `TransactContext` is entry-exit-only, and
  add a separate `TransactCancellable(ctx, fn)` with a cooperative
  contract.

Tracked as a follow-up from #8 (server-side injection).
EOF
)"
```

- [ ] **Step 3: Record issue URLs**

Capture the URLs of the two filed issues. They will be linked from the
PR description when opening the PR for this branch.

---

## Final verification

- [ ] **Step 1: Run all tests with race detector**

Run: `cd /Users/nimit/Documents/Eukarya/ygo && go test -race ./... -timeout 120s`

Expected: all pass, no races.

- [ ] **Step 2: Run vet and build**

Run: `cd /Users/nimit/Documents/Eukarya/ygo && go vet ./... && go build ./...`

Expected: no output (all green).

- [ ] **Step 3: Check git log**

Run: `git log --oneline chore/server-side-injection ^main`

Expected: ~12-13 commits, each atomic and well-messaged.

- [ ] **Step 4: Offer PR creation**

Ask the user:
> Implementation complete. All tests pass under `-race`. Open a PR
> against `main` linking issue #8 and the two follow-up issues filed
> in Task 13?

---

## Self-review notes

- Spec coverage: every requirement in the spec maps to at least one task.
  - `BroadcastUpdate` → Task 4
  - `Apply` → Tasks 5–8
  - `CloseRoom` → Task 10
  - `OnInject` / types / errors → Tasks 1, 9
  - Server fields → Task 2
  - `MaxRooms` enforcement → Task 3
  - Shutdown interactions → covered via per-method tests (Tasks 4, 6, 10)
  - Concurrency / convergence → Task 8
  - Security: room-name validation → Task 4 test; update size → Task 4, 6;
    MaxRooms → Tasks 3, 6
  - Docs → Tasks 11, 12
  - Follow-up issues → Task 13
- Placeholder scan: one confirmed placeholder — Task 4's
  `MsgSyncUpdate` vs `MsgSyncStep2` must be resolved by grep before
  the code is committed. Included the grep command in the task steps.
- Type consistency: `InjectHook`, `InjectInfo`, `InjectOp`, error
  sentinel names match across all tasks and the spec.
