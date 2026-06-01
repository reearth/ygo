# Cluster + Versioned-Persistence — API Reconnaissance

Task 1.1 of the migration plan. Confirms (or corrects) the exact ygo APIs that the
upcoming `Relay`/`Sink` cluster interfaces and the `VersionedPersistence` interface
will build on.

- Repo: `github.com/reearth/ygo`
- Branch: `feat/cluster-and-versioned-persistence`
- Baseline: `go build ./...` clean; `go test ./...` = 742 pass / **1 fail** / 2 skip.
  The single failure is `crdt.TestCompat_GoToJS` (`crdt/go_js_compat_test.go:19`),
  which shells out to Node and fails with `Cannot find module 'yjs'` — the `yjs`
  npm package is not installed in this environment. It is an external-interop test,
  not a Go-code regression. **Baseline is effectively GREEN.** (The test is written
  to `t.Skip` when `node` is absent, but here `node` *is* on PATH while the `yjs`
  module is not, so it errors instead of skipping.)

All file:line references below are against the checked-out tree.

---

## 1. `provider/websocket/server.go`

### `Server` struct — `server.go:217`
net/http-compatible WebSocket handler; one independent Yjs `*crdt.Doc` per room.
Internals of interest:
- `rooms map[string]*room` (guarded by `rmu sync.RWMutex`)
- `persistence PersistenceAdapter`
- `shutdownCh chan struct{}`

Public/config fields (all exported, set directly on the struct — there is **no
functional-options constructor**):
- `AuthFunc func(r *http.Request) bool` — `server.go:230`. Return false ⇒ 401.
- `AllowedOrigins []string` — `server.go:247`. Empty ⇒ same-origin check; `"*"` ⇒ any.
- `OnInject InjectHook` — `server.go:264` (see §2 for `InjectHook`).
- `OnStateless StatelessHook` — `server.go:273`.
- `OnLoadDocument func(ctx context.Context, room string, doc *crdt.Doc) error` — `server.go:289`.
  Fires once per room after persistence bootstrap, before any peer; runs **under `s.rmu.Lock`**; non-nil error fails room creation.
- `OnUnloadDocument func(ctx context.Context, room string)` — `server.go:296`.
- `OnFirstPeer func(ctx context.Context, room string)` — `server.go:309` (0→1 transition; ctx = WS request ctx).
- `OnLastPeer func(ctx context.Context, room string)` — `server.go:317` (1→0 transition; ctx = `context.Background()`).
- Plus caps: `MaxConnections`, `MaxPeersPerRoom`, `MaxUpdateBytes` (int), `MaxRooms`,
  `MaxMessageBytes int64`, `Logger *slog.Logger`, `PeerWriteQueueSize`, `MaxPendingItems`,
  `HandshakeTimeout time.Duration`, `MaxAwarenessBytesPerRoom int64`.

> **No options pattern.** Configuration is by exported struct fields after construction.

### Constructors
- `func NewServer() *Server` — `server.go:446`. Empty room store, no persistence.
- `func NewServerWithPersistence(p PersistenceAdapter) *Server` — `server.go:505`.
  Wraps `NewServer()` and sets `s.persistence = p`.

### Lifecycle
- `func (s *Server) Shutdown(ctx context.Context) error` — `server.go:458`.
- `func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request)` — `server.go:596`.
- `func (s *Server) GetDoc(name string) *crdt.Doc` — `server.go:513`.
  Returns the room's `*crdt.Doc`, or **`nil` if the room is not currently resident**
  (no peer connected and no `Apply` created it). Read-locks `rmu`.

### `PersistenceAdapter` interface — `server.go:130`
```go
type PersistenceAdapter interface {
    LoadDoc(room string) ([]byte, error)          // full binary V1 update, or (nil,nil)
    StoreUpdate(room string, update []byte) error  // incremental V1 update per committed txn
}
```

### `PersistenceAdapterContext` (optional extension) — `server.go:152`
```go
type PersistenceAdapterContext interface {
    StoreUpdateContext(ctx context.Context, room string, update []byte) error
}
```
Detected at runtime by type-assertion in the persistence worker; ctx is cancelled at
`Shutdown`. Implementing only `PersistenceAdapter` is fully supported.

### How persistence is wired (important for VersionedPersistence)
In `getOrCreateRoom` (`server.go:522`):
- On room creation: `data, _ := s.persistence.LoadDoc(name)`; if non-empty,
  `crdt.ApplyUpdateV1(r.doc, data, nil)` bootstraps the doc (`server.go:548-557`).
- `OnLoadDocument` fires after bootstrap, before the persistence worker starts (`server.go:565`).
- A buffered channel `persistCh` + worker is started, then:
  ```go
  r.doc.OnUpdate(func(update []byte, _ any) {       // server.go:582
      select {
      case r.persistCh <- update:
      case <-r.persistStop:
      }
  })
  ```
  ⇒ **Every committed transaction's incremental V1 update is fed to the adapter via the
  doc's `OnUpdate` observer.** The adapter receives raw incremental V1 updates; it is
  free to append (versioned log) or merge (snapshot) them.
- The in-memory default `MemoryPersistence` (`server.go:164`) merges via
  `crdt.MergeUpdatesV1(existing, update)` into one V1 blob per room.

> For a VersionedPersistence relay/echo design: the `origin any` passed to `OnUpdate` is
> available here (currently discarded as `_`). The echo-guard / version-tagging logic can
> key off it — see §4.

---

## 2. `provider/websocket/inject.go`

- `type InjectHook func(ctx context.Context, info InjectInfo) error` — `inject.go:63`.
- `func (s *Server) BroadcastUpdate(ctx context.Context, room string, update []byte) error` — `inject.go:150`.
  Fans a **pre-encoded V1 update** to all peers in `room`. Does NOT apply to the server doc
  (caller must `crdt.ApplyUpdateV1` first to avoid divergence). Validates update by applying
  to a throwaway doc. Honors `OnInject` (`OpBroadcastUpdate`).
- `func (s *Server) Apply(ctx context.Context, room string, fn func(doc *crdt.Doc, transact func(func(*crdt.Transaction)))) error` — `inject.go:253`.
  Auto-creates the room, runs `fn` with a bound `transact` helper, captures updates produced
  under a private sentinel origin (`origin := new(struct{})`, `inject.go:283`), merges via
  `MergeUpdatesV1`, and fans out. Honors `OnInject` (`OpApply`). `fn` MUST use the supplied
  `transact`; calling `doc.Transact` directly ⇒ `ErrNoChanges`.
- `func (s *Server) CloseRoom(name string, force bool) error` — `inject.go:428`.
  Drains persistence, removes the room. `force=false` + peers ⇒ `ErrRoomHasPeers`.

Error sentinels (`inject.go:97-127`): `ErrServerShutdown`, `ErrInvalidRoomName`,
`ErrRoomNotFound`, `ErrRoomHasPeers`, `ErrInvalidUpdate`, `ErrUpdateTooLarge`,
`ErrTooManyRooms`, `ErrNoChanges`, `ErrInjectRefused`.

Wire helper: `encodeBroadcastWire(update []byte) []byte` — `inject.go:408`. Frames as
`[msgSync=0][MsgUpdate][VarBytes(update)]`.

---

## 3. `provider/websocket/peer.go` — where fan-out happens

Inbound dispatch is `(*peer).handleMessage(data []byte)` — `peer.go:38`.

### Sync (doc-update) fan-out
- `case msgSync` (`peer.go:46`): `ygsync.ApplySyncMessage(p.room.doc, payload, p)` applies the
  peer's sync message to the room doc; if it was a step-1 the reply goes back to that peer
  only, otherwise `p.broadcastSync(payload)` fans to **all other peers**.
- `broadcastSync(syncMsg []byte)` — `peer.go:353` → `broadcast(..., excludeSelf=true)`.
- Note: the doc's own `OnUpdate` observer is what drives persistence (§1); peer fan-out of
  sync messages is byte-relay of the raw inbound payload, not re-encoded from the doc.

### Awareness fan-out
- `case msgAwareness` (`peer.go:61`): `p.room.awareness.ApplyUpdate(awBytes, p)` then
  `p.broadcastAwareness(awBytes)`.
- `broadcastAwareness(awMsg []byte)` — `peer.go:361` → `broadcast(..., excludeSelf=true)`.
- `broadcastAwarenessFromRoom(awMsg []byte)` — `peer.go:370` → `broadcast(..., excludeSelf=false)`
  (used by disconnect handler after the peer is already removed).
- Server→peer initial awareness send: `sendAwareness` (`peer.go:345`).

### The actual fan-out primitive
- `func (p *peer) broadcast(data []byte, excludeSelf bool)` — `peer.go:387`. Enqueues into each
  peer's `writeCh`; on full queue it **closes the slow peer** (bounded-broadcast, no drop).
- Per-peer writer goroutine: `(*peer).runWriter()` — `peer.go:458`.

> For a relay: the natural injection point mirrors `BroadcastUpdate` (re-uses `encodeBroadcastWire`)
> for sync, and `sendAwareness`/`broadcastAwareness` framing for awareness. Awareness state for a
> room is reachable via the room's `*awareness.Awareness` (see §6 / Sink note below).

---

## 4. Origin mechanism — `crdt/doc.go` + `crdt/update.go` (echo guard)

- Origin type is `any`; callers type-assert on read (documented `crdt/doc.go:33-39`).
- `func ApplyUpdateV1(doc *Doc, update []byte, origin any) error` — `update/update.go:48`.
  Internally: `doc.Transact(func(txn){ applyV1Txn(txn, update) }, origin)` — i.e. the
  `origin` is forwarded straight into `Transact` as the transaction's `Origin`.
- `func (d *Doc) Transact(fn func(*Transaction), origin ...any)` — `doc.go:510` (variadic; only the
  first is used). Also `TransactE`, `TransactContext`, `TransactContextE` (all `origin ...any`).
- **Observer registration (the echo-guard hook):**
  ```go
  func (d *Doc) OnUpdate(fn func(update []byte, origin any)) func()   // doc.go:540 — returns unsubscribe
  ```
  The callback receives the incremental V1 update bytes **and the `origin` passed to Transact**.
  This is exactly the channel an echo guard rides: tag locally-injected updates with a sentinel
  origin, and in the `OnUpdate` callback skip republishing when `origin == sentinel`. `Apply`
  already uses this pattern (`inject.go:283-299`): `if o != origin { return }`.
- Richer alternative: `func (d *Doc) OnAfterTransaction(fn func(*Transaction)) func()` — `doc.go:562`.
  The callback receives the full `*Transaction`, whose exported `Origin any` field
  (`doc.go:411`, struct field set at `Origin: orig`) carries the origin, plus `Local bool`,
  `beforeState`/`afterState` state vectors, and the delete set.
- **Update-event struct:** there is **no dedicated `UpdateEvent` struct.** `OnUpdate` delivers a
  plain `(update []byte, origin any)` pair; the richer event object is `*Transaction` itself
  (delivered via `OnAfterTransaction`). Relevant exported `Transaction` field: `Origin any`.

> Echo-guard recommendation: define a package-private sentinel value
> (`var fromRelay = new(struct{})` or a typed token) and pass it as `origin` to
> `ApplyUpdateV1`/`Apply`; the relay's `OnUpdate` listener compares pointer identity to drop
> echoes — identical to `Apply`'s existing private-origin capture.

---

## 5. `awareness/awareness.go`

- `func New(clientID uint64) *Awareness` — `awareness.go:129`. (Server uses `awareness.New(0)`.)
- `func (a *Awareness) ApplyUpdate(update []byte, origin any) error` — `awareness.go:369`.
  Decodes + merges; enforces the y-protocols **clock gate** (`awareness.go:420-435`):
  older clock → drop; equal clock + null + active → accept (offline signal); equal clock
  otherwise → drop; newer → accept. Plus self-state protection (`awareness.go:441`).
  Returns `ErrTooManyClients` / `ErrStateTooLarge`.
- `func (a *Awareness) ApplyUpdateContext(ctx, update []byte, origin any) error` — `awareness.go:541`.
- `func (a *Awareness) EncodeUpdate(clientIDs []uint64) []byte` — `awareness.go:326`.
  `nil` ⇒ encode all known clients (including removed/null). **No error return.**
- `func (a *Awareness) OnChange(fn func(ChangeEvent)) func()` — `awareness.go:287`. Returns unsubscribe.
  `ChangeEvent` (`awareness.go:92`): `Added/Updated/Removed []uint64`, `Origin any`.
- Clock gate: enforced inside `ApplyUpdate` (`awareness.go:420-435`) — there is no separate
  public "clock-gate" function; it is intrinsic to `ApplyUpdate`.
- Expiry / heartbeat:
  - `func (a *Awareness) StartAutoExpiry(timeout time.Duration) func()` — `awareness.go:553` (ticks at `timeout/2`; returns stop fn).
  - `func (a *Awareness) RemoveExpired(timeout time.Duration)` — `awareness.go:605` (local client never self-expires).
  - `func (a *Awareness) Heartbeat()` — `awareness.go:226` (re-emit local state at bumped clock; observers NOT fired).
  - `func (a *Awareness) SetLocalState(map[string]any)` / `SetLocalStateContext` — `awareness.go:164` / `:252`.
  - `func (a *Awareness) GetStates() map[uint64]ClientState` — `awareness.go:273`; `GetLocalState` — `:261`.
  - `func (a *Awareness) SetMaxBytes(n int64)` — `awareness.go:151`; `Destroy()` — `awareness.go:588`; `ClientID() uint64` — `:158`.
  - `const DefaultTimeout = 30 * time.Second` — `awareness.go:14`.

> Sink note: the server holds the room's `*awareness.Awareness` privately on the
> unexported `room` struct (`server.go:202`, field `awareness`). There is **no public
> `Server` accessor that returns the `*awareness.Awareness`** (only `GetDoc` for the doc).
> See mismatch M4.

---

## 6. Snapshot / merge primitives — `crdt/snapshot.go` + `crdt/update.go`

- `type Snapshot struct { StateVector StateVector; DeleteSet DeleteSet }` — `snapshot.go:13`.
- `func CaptureSnapshot(doc *Doc) *Snapshot` — `snapshot.go:19`.
- `func EncodeSnapshot(snap *Snapshot) []byte` — `snapshot.go:32` (Yjs-compatible wire format).
- `func DecodeSnapshot(data []byte) (*Snapshot, error)` — `snapshot.go:54`.
- `func RestoreDocument(doc *Doc, snap *Snapshot) (*Doc, error)` — `snapshot.go:113`.
  **Returns a NEW `*Doc`** (created `WithGC(false)`) reflecting doc's state at snapshot time;
  requires the source doc to still hold full item history (gc off or `RunGC` not yet run).
- `func EncodeStateFromSnapshot(doc *Doc, snap *Snapshot) ([]byte, error)` — `snapshot.go:124`
  (V1 update for the historical state; apply to a fresh doc to reconstruct).
- `func EncodeStateAsUpdateV1(doc *Doc, sv StateVector) []byte` — `update.go:41` (nil sv ⇒ full state).
- `func EncodeStateAsUpdateV2(doc *Doc, sv StateVector) []byte` — `update.go:59`.
- `func MergeUpdatesV1(updates ...[]byte) ([]byte, error)` — `update.go:96`
  (applies all to a temp doc, re-encodes full V1 state).
- `func EncodeStateVectorV1(doc *Doc) []byte` — `update.go:117` (**no error return**).
- Bonus primitives available: `DiffUpdateV1(update []byte, sv StateVector) ([]byte, error)` — `update.go:107`
  (useful for `MaterializeAt`/incremental catch-up); `UpdateV1ToV2`/`UpdateV2ToV1`.

> There is **no `EncodeStateAsUpdateV2(...)` `error` return** and **no `MergeUpdatesV2`** in this
> tree — only the V1 merge. A versioned store that needs V2 must convert via `UpdateV1ToV2`.

---

## 7. Mismatches vs the design sketches (and corrected signatures)

### Cluster sketch
- **M1 — `origin any` is the right echo-guard channel, and it is real.** Sketch's "echo guard
  rides the `origin any` sentinel" is fully supported: `ApplyUpdateV1(doc, update, origin)`
  forwards `origin` into `Transact`, and `Doc.OnUpdate(func(update []byte, origin any))` exposes
  it. Awareness has its own parallel `origin any` on `ApplyUpdate` and `ChangeEvent.Origin`.
  **No correction needed** — but note: for awareness the origin does **not** flow over the wire
  in `EncodeUpdate`; it is observer-local only. The relay must therefore key its awareness echo
  guard on the `Origin` it passes to the room's `ApplyUpdate`, not on anything decodable from the
  awareness bytes.
- **M2 — `Sink.AwarenessRef(room) (*awareness.Awareness, bool)` is NOT directly satisfiable
  with the current public API.** `GetDoc(room) *crdt.Doc` exists (`server.go:513`) but there is
  **no public accessor for the room's `*awareness.Awareness`** (it lives on the unexported
  `room.awareness` field). The Sink either (a) needs a new `Server` method, e.g.
  `func (s *Server) GetAwareness(room string) (*awareness.Awareness, bool)`, added in a later
  task, or (b) must maintain its own per-room `*awareness.Awareness` outside the server. Recommend
  adding the accessor. Corrected sketch signature is fine; the **gap is server-side plumbing**.
- **M3 — `Sink.Inject(ctx, Inbound) error` maps cleanly onto existing methods, but there are
  two distinct injection paths, not one.** For `KindSync`, inbound bytes are a V1 update ⇒
  apply to the doc with a relay sentinel origin and fan out: either
  `crdt.ApplyUpdateV1(doc, data, relayOrigin)` + `Server.BroadcastUpdate(ctx, room, data)`, or a
  new helper. For `KindAwareness` ⇒ `aw.ApplyUpdate(data, relayOrigin)` + awareness fan-out.
  There is **no single existing entrypoint** that does both; `Sink.Inject` will dispatch on
  `Kind`. The `BroadcastUpdate` signature is `(ctx, room string, update []byte)` — matches.
- **M4 — `Sink.Rooms() []string` has no public backing.** `Server` keeps `rooms map[string]*room`
  unexported with no lister. Recommend adding `func (s *Server) Rooms() []string` in a later task,
  or have the Sink track activations itself via `OnFirstPeer`/`OnLastPeer`/`OnLoadDocument`/
  `OnUnloadDocument` (which already give room-name granularity — see §1). The sketch's
  `RoomActivated/RoomDeactivated(room string)` on `Relay` map naturally onto
  `OnFirstPeer`/`OnLastPeer` (or `OnLoadDocument`/`OnUnloadDocument`).
- **M5 — `Outbound/Inbound{... Origin any}` Kind enum naming.** Sketch uses
  `KindSync|KindAwareness`. Fine as a new cluster-package enum. Just note that the wire-level
  message tags in the server are `msgSync=0` / `msgAwareness=1` (`server.go:60-64`); keep the
  cluster `Kind` independent of those numeric tags (they are unexported anyway).
- **M6 — `Relay.Start(ctx, Sink) error` / `Publish(ctx, Outbound) error` / `Close() error`.**
  No conflict with ygo; these are new. The publish trigger inside the server is the doc
  `OnUpdate` observer (`server.go:582`) and the awareness path in `peer.go`. To publish *every*
  local change, the cluster layer should register its own `doc.OnUpdate` (and `aw.OnChange`)
  per room — the server does not currently expose a "post-broadcast" callback for relaying, so
  the relay subscribes to the doc/awareness directly (which requires the §M2 awareness accessor).

### Persistence sketch
- **M7 — `Load` vs `LoadDoc`.** The server's adapter contract method is `LoadDoc(room) ([]byte, error)`
  (`server.go:133`), not `Load`. If `VersionedPersistence` is meant to *also* satisfy
  `websocket.PersistenceAdapter`, name the method `LoadDoc` (returning the materialized-head V1
  update) and `StoreUpdate(room, update []byte) error` (append path). Recommend the versioned
  interface embed or alias to those names, e.g.:
  ```go
  LoadDoc(room string) ([]byte, error)            // materialized head — satisfies PersistenceAdapter
  AppendUpdate(room string, update []byte) (Version, error)
  ```
  If `VersionedPersistence` is a *separate* abstraction wrapped by an adapter shim, `Load` is
  fine — but the shim must expose `LoadDoc`/`StoreUpdate` to plug into `NewServerWithPersistence`.
- **M8 — `CaptureSnapshot`/`RestoreSnapshot` naming vs crdt.** crdt provides
  `CaptureSnapshot(doc) *Snapshot`, `EncodeSnapshot`/`DecodeSnapshot`, and **`RestoreDocument`
  (returns a new `*Doc`)** — there is **no `RestoreSnapshot`**. Recommend the persistence method
  be named to avoid colliding/confusing with the crdt free functions, e.g.
  `CaptureSnapshot(room string) (Version, error)` (persist a snapshot) and
  `RestoreSnapshot(room string, v Version) error` (materialize back), internally built on
  `crdt.CaptureSnapshot` + `crdt.EncodeSnapshot` and `crdt.RestoreDocument`/
  `crdt.EncodeStateFromSnapshot`. Note the crdt snapshot is **state-vector + delete-set**, not a
  standalone byte blob of content — to reconstruct content you also need the historical updates
  (or a gc-off source doc). For a persistence store, `EncodeStateAsUpdateV1` of the materialized
  doc is the portable "snapshot blob"; `crdt.Snapshot` is better viewed as a *version marker*.
- **M9 — `MaterializeAt` / `GetUpdate` / `ListVersions` / `PruneAfter` / `Compact` / `Delete`
  have no ygo equivalent** — they are pure storage-layer concerns and are NOT provided by crdt.
  The building blocks they sit on are: `MergeUpdatesV1(...)` (fold a version range into head),
  `DiffUpdateV1(update, sv)` (compute the delta a client is missing, for incremental
  materialize/catch-up), `EncodeStateVectorV1(doc)` (the per-version state-vector marker), and
  `EncodeStateAsUpdateV1(doc, sv)` (materialize). **Recommended corrected signatures**, all
  returning errors and keyed by a store-defined `Version` type:
  ```go
  type VersionedPersistence interface {
      LoadDoc(room string) ([]byte, error)                        // materialized head (V1)
      AppendUpdate(room string, update []byte) (Version, error)
      ListVersions(room string) ([]Version, error)
      GetUpdate(room string, v Version) ([]byte, error)           // the V1 delta stored at v
      MaterializeAt(room string, v Version) ([]byte, error)       // MergeUpdatesV1 over [..v]
      CaptureSnapshot(room string) (Version, error)
      RestoreSnapshot(room string, v Version) error
      PruneAfter(room string, target Version, rolledBack bool) error
      Compact(room string, keep int) error
      Delete(room string) error
  }
  ```
  (`StoreUpdate` from `PersistenceAdapter` is the same operation as `AppendUpdate` minus the
  returned `Version`; provide a thin adapter method if this type must plug directly into
  `NewServerWithPersistence`.)
- **M10 — V2 merge gap.** `MergeUpdatesV1` exists; there is **no `MergeUpdatesV2`**. If the store
  keeps V2 payloads, convert with `UpdateV1ToV2`/`UpdateV2ToV1` (`update.go:76`/`:86`). Recommend
  the store standardize on **V1** internally for merge/diff and only emit V2 at the edge if needed.

---

## 8. Confirmed signature quick-reference

```go
// server.go
func NewServer() *Server
func NewServerWithPersistence(p PersistenceAdapter) *Server
func (s *Server) GetDoc(name string) *crdt.Doc
func (s *Server) Shutdown(ctx context.Context) error
type PersistenceAdapter interface {
    LoadDoc(room string) ([]byte, error)
    StoreUpdate(room string, update []byte) error
}
type PersistenceAdapterContext interface {
    StoreUpdateContext(ctx context.Context, room string, update []byte) error
}
// hooks are exported struct fields:
//   AuthFunc func(*http.Request) bool
//   AllowedOrigins []string
//   OnInject InjectHook
//   OnLoadDocument func(ctx context.Context, room string, doc *crdt.Doc) error
//   OnUnloadDocument func(ctx context.Context, room string)
//   OnFirstPeer func(ctx context.Context, room string)
//   OnLastPeer  func(ctx context.Context, room string)

// inject.go
type InjectHook func(ctx context.Context, info InjectInfo) error
func (s *Server) BroadcastUpdate(ctx context.Context, room string, update []byte) error
func (s *Server) Apply(ctx context.Context, room string, fn func(doc *crdt.Doc, transact func(func(*crdt.Transaction)))) error
func (s *Server) CloseRoom(name string, force bool) error

// crdt/doc.go + crdt/update.go
func (d *Doc) OnUpdate(fn func(update []byte, origin any)) func()
func (d *Doc) OnAfterTransaction(fn func(*Transaction)) func()   // Transaction has exported Origin any
func (d *Doc) Transact(fn func(*Transaction), origin ...any)
func ApplyUpdateV1(doc *Doc, update []byte, origin any) error
func EncodeStateAsUpdateV1(doc *Doc, sv StateVector) []byte
func EncodeStateAsUpdateV2(doc *Doc, sv StateVector) []byte
func MergeUpdatesV1(updates ...[]byte) ([]byte, error)
func DiffUpdateV1(update []byte, sv StateVector) ([]byte, error)
func EncodeStateVectorV1(doc *Doc) []byte

// awareness/awareness.go
func New(clientID uint64) *Awareness
func (a *Awareness) ApplyUpdate(update []byte, origin any) error
func (a *Awareness) EncodeUpdate(clientIDs []uint64) []byte
func (a *Awareness) OnChange(fn func(ChangeEvent)) func()        // ChangeEvent.Origin any
func (a *Awareness) StartAutoExpiry(timeout time.Duration) func()
func (a *Awareness) RemoveExpired(timeout time.Duration)
func (a *Awareness) Heartbeat()

// crdt/snapshot.go
func CaptureSnapshot(doc *Doc) *Snapshot
func EncodeSnapshot(snap *Snapshot) []byte
func DecodeSnapshot(data []byte) (*Snapshot, error)
func RestoreDocument(doc *Doc, snap *Snapshot) (*Doc, error)     // NOTE: no RestoreSnapshot; returns NEW *Doc
func EncodeStateFromSnapshot(doc *Doc, snap *Snapshot) ([]byte, error)
```
