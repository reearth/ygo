# ygo Examples

This directory contains self-contained examples that demonstrate the core features of `ygo`. Each example is a runnable Go program with detailed inline comments explaining every step.

## Examples

| Example | What it shows |
|---------|---------------|
| [peer-sync](peer-sync/) | In-process two-peer sync using the y-protocols handshake — no network required |
| [http-sync](http-sync/) | Pull/push sync over HTTP using `provider/http` — good for offline-capable apps |
| [collab-editor](collab-editor/) | Real-time collaborative text editor with a browser client over WebSocket |
| [snapshot-history](snapshot-history/) | Document version history using snapshots — restore any past state |

---

## peer-sync — In-process sync

**What it demonstrates:** The y-protocols three-message handshake (`SyncStep1 → SyncStep2 → Update`) using Go channels as the "transport". No HTTP, no WebSocket — just bytes passing between goroutines.

**Run:**
```bash
go run ./examples/peer-sync
```

**Key concepts:**
- `sync.EncodeSyncStep1(doc)` — encode your state vector and ask a peer for what you're missing
- `sync.ApplySyncMessage(doc, msg, origin)` — decode and apply any sync message; returns a reply when step-1 is received
- `sync.EncodeUpdate(delta)` — wrap an incremental update for broadcast after the initial handshake
- CRDT convergence guarantee: no matter what order updates arrive, all peers converge to the same state

**When to use HTTP vs WebSocket vs in-process:**
| Transport | Latency | Use case |
|-----------|---------|----------|
| In-process channels | Microseconds | Testing, server-side fan-out, same-process replicas |
| HTTP (pull/push) | ~1 RTT per sync | Offline-first mobile apps, batch sync, simple REST APIs |
| WebSocket | Near real-time | Collaborative editors, live cursors, multiplayer games |

---

## http-sync — HTTP pull/push

**What it demonstrates:** Two Go peers syncing a shared document through an HTTP server. Peer A pushes updates via `POST /doc/{room}`. Peer B fetches only the content it's missing via `GET /doc/{room}?sv=<base64-state-vector>`.

**Run:**
```bash
go run ./examples/http-sync
```

**Key concepts:**
- State vectors are compact summaries: `{clientID → highestClock}`. Sending your state vector to a server lets it compute only the diff you need.
- `crdt.EncodeStateVectorV1(doc)` → base64 → `?sv=` query parameter
- `crdt.ApplyUpdateV1(doc, diff, origin)` — apply the returned binary diff
- Bandwidth savings: a client that already has 90% of a document only downloads the remaining 10%

**Interoperability with JS/Rust:**
The HTTP provider speaks the same Yjs V1 binary update format. A JS browser using `y-protocols` or a Rust peer using `yrs` can POST updates to the same server.

---

## collab-editor — WebSocket collaborative editor

**What it demonstrates:** A fully functional multi-room collaborative text editor with a Go backend and a browser frontend. Multiple browser tabs share the same document in real time.

**Run:**
```bash
go run ./examples/collab-editor/server
```

Then open `http://localhost:8080` in two browser tabs. Both tabs edit the same document live.

**Key concepts:**
- `provider/websocket.NewServer()` — a single `http.Handler` that manages all rooms and peers
- Uses [y-websocket](https://github.com/yjs/y-websocket) JS client for the browser side (loaded from CDN)
- Three-message handshake on connect: server sends `SyncStep1` + `SyncStep2` (full state) + current `Awareness`
- Awareness protocol: cursors, user presence, and custom ephemeral state (e.g. `{cursor: 42, color: "blue"}`)
- Rooms are isolated: `#room-a` and `#room-b` in the URL hash map to separate documents

**Connecting a JS client manually:**
```js
import * as Y from 'yjs'
import { WebsocketProvider } from 'y-websocket'

const doc = new Y.Doc()
const provider = new WebsocketProvider('ws://localhost:8080/yjs', 'my-room', doc)
const text = doc.getText('content')
```

---

## snapshot-history — Document versioning

**What it demonstrates:** Taking point-in-time snapshots of a document, encoding them for storage, and restoring the document to any past revision.

**Run:**
```bash
go run ./examples/snapshot-history
```

**Key concepts:**
- `crdt.CaptureSnapshot(doc)` — captures a `{StateVector, DeleteSet}` pair (very compact — no item content)
- `crdt.EncodeSnapshot` / `crdt.DecodeSnapshot` — binary round-trip compatible with `Y.encodeSnapshot` / `Y.decodeSnapshot` in JS
- `crdt.RestoreDocument(doc, snap)` — reconstructs the document as it was at snapshot time
- `crdt.EncodeStateFromSnapshot(doc, snap)` — produces a standard V1 update that any peer can apply to see the historical version
- **GC interaction:** `WithGC(false)` must be set on documents where you need full restoration. After `RunGC` runs, deleted item content is freed and cannot be un-deleted during restore.

**Storing snapshots in a database:**
```go
snap := crdt.CaptureSnapshot(doc)
data := crdt.EncodeSnapshot(snap)
db.Save("snapshots", revisionID, data) // store bytes anywhere

// Later, restore:
data = db.Load("snapshots", revisionID)
snap, _ = crdt.DecodeSnapshot(data)
restored, _ = crdt.RestoreDocument(doc, snap)
```

---

## Interoperability with JavaScript and Rust

All examples use the standard Yjs V1 binary update format. Updates produced by `ygo` can be applied by:

- **JavaScript:** [Yjs](https://github.com/yjs/yjs) v13.x — `Y.applyUpdate(doc, update)` / `Y.encodeStateAsUpdate(doc)`
- **Rust:** [yrs](https://github.com/y-crdt/y-crdt) — `Doc::apply_update` / `Doc::encode_state_as_update`
- **Any compliant Yjs implementation** that speaks y-protocols and lib0 encoding

The WebSocket provider additionally speaks [y-websocket](https://github.com/yjs/y-websocket) wire format, so a standard JS or Rust WebSocket client connects without modifications.
