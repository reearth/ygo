# Server-side document injection for AI agents and backend APIs

**Issue:** [reearth/ygo#8](https://github.com/reearth/ygo/issues/8)
**Branch:** `chore/server-side-injection`
**Status:** Draft — awaiting user review
**Author:** Nimit Bhandari

## Background

Today the only way to push content into a live ygo document from server-side
Go code is to connect as a WebSocket peer. `Server.GetDoc` returns the room's
`*crdt.Doc` and callers can apply updates via `crdt.ApplyUpdateV1`, but those
updates are not broadcast to connected peers — only updates that arrive
through a peer connection are fanned out.

This locks out an increasingly common class of callers: AI agents, content
pipelines, HTTP request handlers, and other backend services that need to
modify a shared document and have the change propagate to connected
collaborators in real time. Today they must simulate a WebSocket client
connection to themselves, which is wasteful and error-prone.

This spec adds a first-class server-side injection surface to the WebSocket
provider.

## Goals

- Let server-side callers push changes to a named room and have them reach
  every connected peer, without simulating a WebSocket client.
- Give embedders a hook to enforce access control on server-side writes,
  suitable for multi-tenant agentic frameworks.
- Preserve the existing persistence, broadcast, and connection-lifecycle
  guarantees exactly — the new API sits alongside the peer path, it does
  not replace or subtly alter it.
- Bound the resource impact of the new API: no unbounded update sizes,
  no unbounded room creation, no subscription leaks.

## Non-goals

- Mid-transaction cancellation: the underlying `crdt.Doc.Transact` cannot
  be interrupted once `fn` begins execution. Context cancellation is
  honored at entry and pre-broadcast boundaries only.
- Panic-safety for `fn` in `Apply`: a panic inside `fn` leaks the doc's
  write lock due to a pre-existing bug in `crdt.Doc.Transact`. Tracked
  as a separate follow-up issue; this PR documents the constraint.
- Content-level policy: `OnInject` gates *whether* a write is allowed,
  not *what* can be written. Callers needing content-level filtering
  should pre-validate before calling `BroadcastUpdate`/`Apply`.

## Public API

### Methods on `*Server`

```go
// BroadcastUpdate fans out a pre-encoded V1 update to all peers currently
// connected to the named room. Does NOT apply the update to the server's
// doc. Callers who want the server's state to reflect the broadcast must
// call crdt.ApplyUpdateV1 first (or use Apply). Failing to do so creates
// divergence between live peers and new joiners: live peers see the
// update, but a peer joining after the broadcast receives only the
// server's stale state.
//
// Returns:
//   - ErrServerShutdown if the server is shut down
//   - ErrInvalidRoomName if room is empty, too long, or path-unsafe
//   - ErrUpdateTooLarge if len(update) exceeds effective MaxUpdateBytes
//   - ErrInvalidUpdate if update cannot be parsed as a V1 update
//   - wrapped OnInject error if the hook refuses
//   - ErrRoomNotFound if no room by that name exists (may occur if the
//     last peer disconnected concurrently; callers broadcasting to
//     ephemeral rooms should treat this as non-fatal)
//
// Peer write failures during fan-out do not produce an error: they match
// the existing peer-broadcast path, which fires writes in goroutines and
// ignores individual failures (a slow or unresponsive peer is dropped by
// its own write deadline).
func (s *Server) BroadcastUpdate(
    ctx context.Context,
    room string,
    update []byte,
) error

// Apply auto-creates the room if needed, runs fn with a bound transact
// helper, captures the update produced by fn's transaction(s), and fans
// the result out to all connected peers.
//
// fn MUST call transact() to mutate the doc. Calls to doc.GetText,
// doc.GetMap, doc.GetXmlFragment, etc. must happen OUTSIDE transact():
// these methods acquire the doc's write lock, which transact() already
// holds, and calling them inside would deadlock.
//
// fn should be fast. It runs inside the doc's write lock and blocks all
// peer reads and writes to the room for the duration.
//
// The update(s) emitted by fn's transaction(s) are merged and broadcast
// atomically (from the caller's POV). Persistence is triggered via the
// existing OnUpdate hook — callers do not need to separately persist.
//
// Returns:
//   - ErrServerShutdown if the server is shut down
//   - ErrInvalidRoomName if room is empty, too long, or path-unsafe
//   - ErrTooManyRooms if auto-creating would exceed MaxRooms
//   - wrapped persistence error if LoadDoc fails during auto-create
//   - wrapped OnInject error if the hook refuses
//   - ErrNoChanges if fn produced no delta (either never called transact
//     or called transact with a no-op body)
//   - ErrUpdateTooLarge if the merged delta exceeds MaxUpdateBytes
//     (the doc has already been mutated; this is post-hoc reporting)
//
// If fn panics, the subscription is cleaned up via defer and the panic
// propagates. NOTE: due to a pre-existing bug in crdt.Doc.Transact, a
// panic inside fn leaks the doc's write lock and wedges the room.
// Callers MUST ensure fn does not panic.
func (s *Server) Apply(
    ctx context.Context,
    room string,
    fn func(doc *crdt.Doc, transact func(func(*crdt.Transaction))),
) error

// CloseRoom removes the named room from the server. Drains the room's
// persistence write queue and deletes the room from the server's map so
// that subsequent GetDoc / BroadcastUpdate / Apply calls do not see it.
//
// If peers are connected, behavior depends on force:
//   - force=false: returns ErrRoomHasPeers without modifying state
//   - force=true:  closes each peer connection, waits for disconnect
//                  handlers to run, then deletes the room
//
// Returns ErrRoomNotFound if no room by that name exists.
// Returns ErrServerShutdown if the server is shut down.
//
// CloseRoom is primarily intended for releasing rooms created by Apply
// that never accumulated peer connections — without it, such rooms
// linger until process exit.
func (s *Server) CloseRoom(name string, force bool) error
```

### Fields on `*Server`

```go
// OnInject, if non-nil, is called once before every server-side write
// (BroadcastUpdate or Apply). Return a non-nil error to refuse the
// operation; the error is wrapped and returned to the caller.
//
// For BroadcastUpdate, InjectInfo.UpdateSize is the length of the update
// bytes. For Apply, UpdateSize is 0 (the delta has not yet been produced).
//
// ctx is the caller's context; hooks should use ctx.Value to read caller
// identity, tenant, request ID, or other per-call metadata.
OnInject InjectHook

// MaxUpdateBytes is the maximum size of a single V1 update that
// BroadcastUpdate will fan out, or that Apply will produce and fan out.
// Zero means use the same 64 MiB default applied to WebSocket peer frames.
MaxUpdateBytes int

// MaxRooms caps the total number of rooms the server will hold at once,
// across both peer-upgrade-created and Apply-created rooms. Zero means
// unlimited. Enforcement applies uniformly: peer upgrades to a new room
// past the cap receive HTTP 503; Apply to a new room past the cap
// returns ErrTooManyRooms. Upgrades or Apply calls to rooms that
// already exist are unaffected by the cap (no room is being created).
MaxRooms int
```

### Types

```go
// InjectOp identifies which server-side write path is being invoked.
type InjectOp int

const (
    OpBroadcastUpdate InjectOp = iota
    OpApply
)

func (o InjectOp) String() string // "BroadcastUpdate" | "Apply" | "unknown"

// InjectInfo is passed to OnInject. Additional fields may be added in
// future versions; callers should not rely on the struct being fixed-size.
type InjectInfo struct {
    Room       string
    Op         InjectOp
    UpdateSize int // bytes for BroadcastUpdate; 0 for Apply
}

// InjectHook is called before every server-side write.
type InjectHook func(ctx context.Context, info InjectInfo) error
```

### Error sentinels

```go
var (
    ErrServerShutdown   = errors.New("ygo/websocket: server is shut down")
    ErrInvalidRoomName  = errors.New("ygo/websocket: invalid room name")
    ErrRoomNotFound     = errors.New("ygo/websocket: room not found")
    ErrRoomHasPeers     = errors.New("ygo/websocket: room has connected peers")
    ErrInvalidUpdate    = errors.New("ygo/websocket: invalid V1 update")
    ErrUpdateTooLarge   = errors.New("ygo/websocket: update exceeds MaxUpdateBytes")
    ErrTooManyRooms     = errors.New("ygo/websocket: MaxRooms exceeded")
    ErrNoChanges        = errors.New("ygo/websocket: no changes produced")
)
```

## Execution flow

### `BroadcastUpdate(ctx, room, update)`

1. `ctx.Err() != nil` → return that error.
2. `shutdownCh` closed → `ErrServerShutdown`.
3. `isValidRoomName(room)` false → `ErrInvalidRoomName`.
4. `len(update) > effectiveMaxUpdateBytes()` → `ErrUpdateTooLarge`.
5. `crdt.ApplyUpdateV1(crdt.New(), update, nil)` fails → wrap as
   `ErrInvalidUpdate`. Applies to a throwaway doc as the cheapest
   available full-structure validation; `crdt` does not currently
   expose a decode-only parse helper.
6. If `s.OnInject != nil`, call with `InjectInfo{Room: room, Op:
   OpBroadcastUpdate, UpdateSize: len(update)}`. Non-nil → wrap and return.
7. Acquire `s.rmu.RLock()`; look up room. Absent → release, return
   `ErrRoomNotFound`. No auto-create.
8. Snapshot peer slice under `room.mu` (same pattern as
   `peer.broadcast`). Release both locks.
9. Re-check `ctx.Err()`; cancelled → return that error before dispatching.
10. Build wire message: outer `msgSync` + raw sync `MsgSyncUpdate` +
    `update` (same framing as `broadcastSync` at
    [server.go:686-691](provider/websocket/server.go:686)).
11. For each peer, `go other.write(data)` — fire-and-forget.
12. Return `nil`.

### `Apply(ctx, room, fn)`

1. `ctx.Err() != nil` → return.
2. `shutdownCh` closed → `ErrServerShutdown`.
3. `isValidRoomName(room)` false → `ErrInvalidRoomName`.
4. If `s.OnInject != nil`, call with `InjectInfo{Room: room, Op: OpApply,
   UpdateSize: 0}`. Non-nil → wrap and return.
5. `getOrCreateRoomCapped(room)`:
   - Acquire `s.rmu.Lock()`.
   - If room exists, use it.
   - Else: if `s.MaxRooms > 0 && len(s.rooms) >= s.MaxRooms`, release and
     return `ErrTooManyRooms`.
   - Else: create room via existing bootstrap logic (including persistence
     `LoadDoc` and goroutine setup at
     [server.go:306-357](provider/websocket/server.go:306)).
     If `LoadDoc` fails, release, return wrapped error.
   - Release `s.rmu`.
6. Create `origin := new(struct{})` (unique pointer, private to this
   call). Allocate `captured := make([][]byte, 0, 1)`.
7. Subscribe: `unsub := doc.OnUpdate(func(update []byte, o any) {
   if o == origin { captured = append(captured, update) } })`.
   `defer unsub()` immediately (ensures cleanup on panic).
8. Build `transact := func(inner func(*crdt.Transaction)) {
   doc.Transact(inner, origin) }`.
9. Call `fn(doc, transact)`. Panics propagate; `unsub` runs via defer.
10. If `len(captured) == 0` → `ErrNoChanges`.
    (No `ctx.Err()` re-check here: `fn`'s mutation is already committed
    and queued to persistence. Aborting the broadcast at this point
    would create the divergence hazard documented under `BroadcastUpdate`.
    Context cancellation after `fn` starts is therefore ignored.)
11. Merge: if `len(captured) == 1`, use `captured[0]` as-is; otherwise
    `crdt.MergeUpdatesV1(captured...)`.
12. If `len(merged) > effectiveMaxUpdateBytes()` → `ErrUpdateTooLarge`.
    The doc has already been mutated; this is post-hoc reporting.
13. Fan out via the same framing and goroutine-per-write pattern as
    `BroadcastUpdate` steps 10–11.
14. Return `nil`.

### `CloseRoom(name, force)`

1. `shutdownCh` closed → `ErrServerShutdown`.
2. `isValidRoomName(name)` false → `ErrInvalidRoomName`.
3. Acquire `s.rmu.Lock()`.
4. Look up room. Absent → release, return `ErrRoomNotFound`.
5. Acquire `room.mu`. Snapshot `peers := len(rm.peers)` and a copy of
   connections.
6. If `peers > 0 && !force` → release both locks, return `ErrRoomHasPeers`.
7. If `peers > 0 && force`:
   - Release `room.mu` to avoid holding it across network-ish work.
   - Close each peer connection (the peer's read loop exits and
     `handleDisconnect` runs, which acquires `s.rmu` and `room.mu`; we
     must release `s.rmu` too before waiting).
   - Release `s.rmu`. Wait for each peer's `done` channel.
   - Re-acquire both locks.
8. `delete(s.rooms, name)`.
9. If `rm.persistStop != nil`, `close(rm.persistStop)`.
10. Release both locks.
11. If `rm.persistDone != nil`, `<-rm.persistDone` (outside locks).
12. Return `nil`.

Subtlety: step 7 has to drop both locks during network close and
disconnect-handler completion because disconnect handlers take those same
locks. A TOCTOU window exists where a new peer could upgrade into the
room between steps 7 and 8; we tolerate this because `force=true` is
"best-effort teardown." If a new peer races in, it will be left in a
room that gets deleted, and its next read will fail. Document this.

### Helper: `effectiveMaxUpdateBytes`

```go
func (s *Server) effectiveMaxUpdateBytes() int {
    if s.MaxUpdateBytes > 0 {
        return s.MaxUpdateBytes
    }
    return maxWSMessageBytes // 64 MiB, matches peer frame limit
}
```

## Concurrency and ordering

### Locks

- `s.rmu` (RWMutex) — guards `s.rooms` map. Held in write mode for room
  create/delete; read mode for lookup. The new `MaxRooms` check and the
  `CloseRoom` deletion both require write mode.
- `room.mu` (Mutex) — guards `room.peers` map and persistence channels.
  Snapshot-and-release pattern for broadcasts.
- `doc.mu` (internal to crdt package) — held by `Transact` for Phase 1.
  Not directly touched by our code.

### Broadcast goroutine pattern

Both `BroadcastUpdate` and `Apply` broadcast via `go peer.write(data)` for
each peer. This matches the existing peer-broadcast path
([server.go:729-731](provider/websocket/server.go:729)). The `data` slice
is read-only after the goroutines are launched; sharing it is safe.

Each `peer.write` takes the peer's `wmu`, sets a per-write deadline of
`writeTimeout` (10 s), and writes. A slow peer cannot stall the broadcast
to other peers.

### Apply's origin-scoped capture

`Doc.Transact` serializes on `d.mu` for Phase 1. Phase 2 (observer
firing) runs outside the lock but is invoked synchronously by the same
`Transact` call that produced the bytes. So when our `OnUpdate` listener
fires, the `origin` parameter is the same pointer value passed to that
`Transact` call.

A concurrent peer write during `fn`'s execution looks like:
1. Peer's `ApplySyncMessage` eventually calls `Doc.Transact(_, nil)`
   (default origin).
2. That `Transact` blocks on `d.mu` until our `Transact` releases.
3. After both finish, observer phases fire independently. Our listener
   sees two callbacks: one with `origin == ours` (collect), one with
   `origin == nil` (ignore).

Strict pointer equality (`o == origin` where `origin` is our private
`*struct{}`) is race-free because no other caller in the process can
forge that pointer value.

**Footgun to document:** if `fn` calls `doc.Transact(...)` directly
(without the bound `transact` helper), the mutation still commits and
persists, but the origin on the resulting `OnUpdate` callback will not
match our marker. The delta is not captured, `Apply` returns
`ErrNoChanges`, and the server broadcasts nothing — yet the doc state
has changed. This splits the doc from live peers in the same way an
unapplied `BroadcastUpdate` does. The godoc for `Apply` is explicit:
`fn` MUST use the supplied `transact` helper. The test suite exercises
this failure mode so behavior is at least well-defined (`ErrNoChanges`
with a mutated doc), even if accidentally-triggered.

### Subscription lifetime

`unsub` from `doc.OnUpdate` is called via `defer` in `Apply`. This
guarantees cleanup on every exit path:
- Normal return after `fn` completes.
- `ErrNoChanges` return.
- `ErrUpdateTooLarge` return after merge.
- Panic during `fn`.
- `ctx` cancellation re-check.

No listener leaks even under adversarial `fn` behavior.

### Shutdown

`Server.Shutdown` ([server.go:233](provider/websocket/server.go:233))
closes `shutdownCh`. Our new methods check this at step 2 and return
`ErrServerShutdown`. A TOCTOU window exists where `Shutdown` could close
the channel after our step 2 check but before step 5 (room creation).
Accepted: the new room will be created, and `Shutdown`'s `persistDones`
collection will miss it, but the process is shutting down anyway. If
this proves problematic in practice, we can add a second shutdown check
inside the `s.rmu.Lock()` critical section of `getOrCreateRoomCapped`.

## Security model

### Trust boundary

The new methods are Go-level API. Anyone who can call them is, by
construction, trusted to mutate any document the server owns. `OnInject`
is defense-in-depth: it lets embedders enforce that a given caller only
writes to rooms they own, but it does not substitute for the caller's
own AuthZ.

Document under a "Trust Model" section in the README: *"Server.Apply and
Server.BroadcastUpdate grant total write authority on the document. Treat
the `*Server` handle with the same care as a database connection — do
not expose it directly to untrusted code."*

### Input validation

- **Room names:** `isValidRoomName` (existing) rejects empty, > 255
  bytes, `.`, `..`, and control characters. Both new methods call this
  at entry. This is the primary defense against path traversal in
  user-implemented persistence adapters that key on room name.
- **Update bytes:** `BroadcastUpdate` calls `crdt.ParseUpdateV1` before
  fan-out. Malformed bytes are rejected at the server boundary, not at
  each peer. `Apply` produces its own bytes via `Transact`, which cannot
  produce invalid V1 updates.
- **Update size:** `MaxUpdateBytes` (default 64 MiB, matching the peer
  frame limit) caps both entry points. Protects peers from oversized
  fan-outs that would exceed their own `ws.SetReadLimit`.

### Resource caps

- **Per-update:** `MaxUpdateBytes` (default 64 MiB).
- **Per-server rooms:** `MaxRooms` (default 0 = unlimited). Protects
  against attackers who can reach `Apply` looping over
  attacker-influenced room names to exhaust memory, goroutines, and
  persistence-backend connections.
- **Per-room peers:** `MaxPeersPerRoom` (existing).
- **Per-server peers:** `MaxConnections` (existing).

### Persistence-bypass hazard

A caller invoking `BroadcastUpdate` *without* first calling
`crdt.ApplyUpdateV1` sends an update to live peers that is never
persisted and never reflected in the server's doc. Live peers apply it;
new peers joining later receive the server's stale state via sync
step 2. The CRDT eventually converges when a peer re-broadcasts, but
a window of divergence exists.

A malicious or buggy caller can use this to hide writes from persistence
indefinitely. `OnInject` is the only guard.

This is documented prominently in the `BroadcastUpdate` godoc and in the
README. The recommended pattern (also shown in the issue) is:
```go
doc := server.GetDoc(room)
if err := crdt.ApplyUpdateV1(doc, update, nil); err != nil { return err }
if err := server.BroadcastUpdate(ctx, room, update); err != nil { return err }
```
Or equivalently, use `Apply` which applies-and-broadcasts atomically.

### Client-ID spoofing

A server-crafted update can carry any `clientID`, including one
belonging to an active peer. This can corrupt the logical history for
that client. This is **not a new attack surface** — it is equally
reachable via the pre-existing `GetDoc` + `ApplyUpdateV1` path. Included
under the trust-model documentation; no code-level mitigation.

### `fn` stalling the room

A slow or blocking `fn` in `Apply` holds `doc.mu` for its full duration,
blocking all peer reads and writes to the room. An attacker who can
reach `Apply` with a controlled `fn` can stall all legitimate
collaborators. Inherent to CRDT transactional semantics; mitigation is
documentation — `fn` must be fast and non-blocking.

### `OnInject` signature rationale

`InjectInfo` is a struct with `Room`, `Op`, and `UpdateSize`. `ctx` is
the first parameter. Specifically chosen for:
- **`ctx`:** carries tenant ID, auth principal, request ID via
  `ctx.Value`. Idiomatic Go; no API surface for us to curate.
- **`Op`:** lets policy differ per path (e.g., allow `Apply` but not
  `BroadcastUpdate`, which is the more dangerous primitive because it
  accepts arbitrary bytes).
- **`UpdateSize`:** enables tighter-than-`MaxUpdateBytes` per-caller
  caps. Zero for `Apply` because the delta hasn't been produced yet;
  `MaxUpdateBytes` still enforces post-hoc.

Future additions (e.g., `Origin`, `CallerID`, idempotency key) are
backward-compatible via new struct fields.

## Testing

### Functional

- `BroadcastUpdate` fans out to all connected peers.
- `BroadcastUpdate` does *not* mutate the server's doc.
- `Apply` mutates doc, triggers persistence via `OnUpdate`, broadcasts
  delta to all peers.
- `Apply` on a never-before-seen room auto-creates, bootstraps
  persistence, and a subsequent peer-joins receives the new state via
  sync step 2.
- `Apply` with multiple `transact()` calls in `fn` merges deltas and
  broadcasts once.
- `Apply` where `fn` calls `doc.Transact` directly (bypassing the bound
  helper) → `ErrNoChanges`; doc is mutated but nothing broadcast.
  Behavior is well-defined even though the caller violated the contract.
- `CloseRoom` on empty room deletes it; subsequent `GetDoc` returns nil.
- `CloseRoom` with `force=true` disconnects peers and deletes.
- `CloseRoom` with `force=false` on non-empty room returns
  `ErrRoomHasPeers`; room and peers untouched.

### Concurrency

- `Apply` + concurrent peer writes converge (acceptance criterion from
  issue). Peers and `Apply` independently write to the room; the CRDT
  state is the same at all observers afterward.
- `Apply` captures only its own transaction's output. Run two `Apply`
  calls and a peer write concurrently; each `Apply` sees exactly its own
  delta, no cross-contamination.
- `Apply` with `fn` that calls `transact` multiple times in sequence →
  all emitted updates are captured; merged into a single broadcast.

### Error paths

- `BroadcastUpdate` on missing room → `ErrRoomNotFound`; room not
  created.
- `BroadcastUpdate` with malformed bytes → `ErrInvalidUpdate`.
- `BroadcastUpdate` with bytes exceeding `MaxUpdateBytes` →
  `ErrUpdateTooLarge`; no fan-out; no peer sees partial data.
- `isValidRoomName` rejects: empty, > 255 bytes, `.`, `..`,
  control-character names. Both methods return `ErrInvalidRoomName`.
- `Apply` where `fn` never calls `transact` → `ErrNoChanges`; no
  broadcast; subscription cleaned up (verify via internal observer count
  if exposed, or via a panic-then-reuse test).
- `Apply` where `fn` calls `transact` with a no-op body → `ErrNoChanges`.
- `Apply` with `ctx` already cancelled at entry → `context.Canceled`;
  no mutation, no broadcast, no `OnInject` call.
- `Apply` where `fn` panics → panic propagates to caller; `defer unsub`
  ran (verify by calling `Apply` again successfully with a fresh doc;
  the prior listener is not present).
- `Apply` where `MaxRooms` exceeded → `ErrTooManyRooms`; room not
  created; no listener subscribed.
- `Apply` where persistence `LoadDoc` fails during auto-create → error
  wrapped and returned; no partial room in `s.rooms`.
- `OnInject` refusal in `BroadcastUpdate` → wrapped error; no fan-out
  performed.
  - Check order for `BroadcastUpdate`: `ctx` → `shutdown` → `name` →
    `size` → `parse` → `inject` → `lookup` → `dispatch`. `OnInject`
    runs *after* parse, so malformed bytes are rejected with
    `ErrInvalidUpdate` even when `OnInject` is set to an unconditional
    refuser. This is intentional — cheap rejections happen first, and
    the `OnInject` hook should not be used as a content firewall. Tests
    must exercise each boundary precisely.
- `OnInject` refusal in `Apply` → wrapped error; room still
  auto-created if it needed to be? **No** — `OnInject` runs before
  `getOrCreateRoomCapped` (step 4 before step 5). Room is not created
  on refusal.
- Post-`Shutdown` calls to any of the three methods → `ErrServerShutdown`.

### OnInject semantics

- `InjectInfo.Op` correctly set to `OpBroadcastUpdate` or `OpApply`.
- `InjectInfo.UpdateSize` is `len(update)` for `BroadcastUpdate`, `0`
  for `Apply`.
- `ctx` passed through unchanged; `OnInject` can `ctx.Value` to read
  caller-attached metadata.
- `OnInject` is called exactly once per call, not multiple times.

### Security-adjacent

- Path-traversal-looking names (`../foo`, `..`) rejected even when the
  persistence adapter would happily accept them.
- `MaxUpdateBytes = 1024`; `BroadcastUpdate` with 2 KiB payload →
  `ErrUpdateTooLarge`.
- `MaxRooms = 2`; third `Apply` → `ErrTooManyRooms`.

## Documentation

- **Godoc** on every new exported symbol.
- **README** — new section "Server-side document injection":
  - The `ApplyUpdateV1 + BroadcastUpdate` pattern.
  - An `Apply` example (corrected from the issue to get XML fragment
    outside `transact`).
  - `OnInject` example for multi-tenant gating.
  - Trust-model paragraph.
- **CHANGELOG / release notes** — user-facing summary of the new API.

## Out of scope

Filed as separate follow-up issues as part of this branch's prep:

1. **`crdt: Doc.Transact leaks d.mu lock on panic in fn`** — pre-existing
   correctness bug. Needs its own semver and behavior review (on panic,
   do we fire observers? skip? partial?). Referenced from this spec and
   from `Apply`'s godoc as a known constraint.

2. **`crdt: TransactContext cannot interrupt fn mid-execution`** —
   feature gap. `TransactContext` today only checks `ctx.Err()` at entry
   and exit; it does not cancel `fn` once running. Our `Apply` checks
   `ctx` at the same granularity, making the limitation visible at the
   new API surface.

Not in scope for any follow-up:

- Richer `OnInject` signature beyond what this spec ships. If a concrete
  multi-tenant need emerges that struct-field additions cannot cover,
  revisit then.
- Fan-out delivery receipts. `BroadcastUpdate` is fire-and-forget,
  matching existing peer-broadcast semantics.

## Migration and backward compatibility

- All new API. No existing signatures change.
- No behavior change to peer-connection paths, persistence, or the
  WebSocket protocol.
- Default values for new `Server` fields preserve today's behavior:
  `OnInject == nil` (no gating), `MaxUpdateBytes == 0` (use 64 MiB
  default, same as peer frame cap), `MaxRooms == 0` (unlimited).
- Minor version bump (v1.1.0) sufficient per semver.
