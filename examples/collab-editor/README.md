# ygo Collaborative Editor Example

A real-time collaborative plain-text editor powered by the **ygo** Go CRDT library and the JavaScript [Yjs](https://yjs.dev) ecosystem. Two (or more) browser tabs editing the same document simultaneously, with changes merged instantly using the YATA conflict-free replicated data type.

No npm, no webpack, no build step — the browser client loads Yjs directly from the [esm.sh](https://esm.sh) CDN.

---

## What this example shows

| Concept | How it is demonstrated |
|---|---|
| CRDT convergence | Two users can type simultaneously and both see the same result |
| WebSocket sync | Go server speaks the y-protocols binary sync protocol |
| Awareness | Online user list updates in real time when tabs open/close |
| Room multiplexing | Different URL hashes (`#room-name`) map to independent documents |
| Zero-dependency server | The WebSocket upgrade and framing are implemented in pure Go |

---

## Prerequisites

- **Go 1.22 or later** (the server uses Go 1.22 ServeMux named wildcards)
- An internet connection at browser load time (esm.sh CDN for Yjs)

---

## How to run

From the **repository root**:

```sh
go run ./examples/collab-editor/server
```

Then open two browser tabs at:

```
http://localhost:8080
```

Both tabs will join the default room. Start typing in either tab and watch the text appear in the other.

---

## How to test collaboration

1. Open `http://localhost:8080` in Tab 1.
2. Open `http://localhost:8080` in Tab 2.
3. Type in Tab 1 — the text appears in Tab 2 within milliseconds.
4. Type in both tabs simultaneously — both edits are merged without conflicts.

### Switching rooms

Append a URL hash to join a different, isolated document:

```
http://localhost:8080/#team-notes
http://localhost:8080/#sprint-planning
```

The hash is used only in the browser. The Go server extracts the room name from
the WebSocket URL path (`/yjs/team-notes`), not from the HTTP request hash.

---

## How it works

```
Browser Tab 1                       Go ygo Server                    Browser Tab 2
─────────────                       ──────────────                   ─────────────

Y.Doc + YText                       room "default"                   Y.Doc + YText
     │                              ┌──────────┐                          │
     │  WebSocket /yjs/default      │ *crdt.Doc │                         │
     ├─────────────────────────────►│          │◄────────────────────────┤
     │  1. SyncStep1 (state vector) │          │  1. SyncStep1             │
     │◄─────────────────────────────│          │─────────────────────────►│
     │  2. SyncStep2 (missing diff) │          │  2. SyncStep2             │
     │                              │          │                           │
     │  User types "Hello"          │          │                           │
     │  3. Update (CRDT delta)      │          │                           │
     ├─────────────────────────────►│ apply    │                           │
     │                              │ broadcast│  3. Update (forwarded)    │
     │                              │          │──────────────────────────►│
     │                              │          │   apply → textarea shows  │
     │                              └──────────┘   "Hello"                 │
```

### y-protocols sync handshake

When a client connects, the server immediately sends a **SyncStep1** message
containing its current state vector (a compact map of `clientID → highestClock`).

The client replies with a **SyncStep2** containing every update the server is
missing (based on the state vector). The server applies those updates and also
sends a SyncStep2 back to the client with what the client was missing.

After the handshake, both peers are fully converged. Live edits are sent as
**Update** messages, which the server applies to its in-memory `*crdt.Doc` and
broadcasts to all other connected peers.

### Binary message format

All messages are binary WebSocket frames. The top-level structure is:

```
[msgType: VarUint] [payload…]
```

| msgType | Payload |
|---|---|
| `0` (sync) | `[syncType: VarUint] [data: VarBytes]` |
| `1` (awareness) | raw awareness encoding |

Sync sub-types:

| syncType | Direction | Data |
|---|---|---|
| `0` SyncStep1 | client→server and server→client | state vector (VarBytes) |
| `1` SyncStep2 | server→client and client→server | update diff (VarBytes) |
| `2` Update | peer→server→peers | V1 update bytes (VarBytes) |

VarUint and VarBytes use the lib0 variable-length encoding (7 bits per byte,
MSB as continuation flag).

### Awareness protocol

Awareness messages carry **ephemeral** state — it is not stored in the CRDT
document and is not replayed to new joiners. Each peer maintains a local state
object:

```js
{ user: { name: "User-1234", color: "#7c83fd" } }
```

When any field changes, the peer broadcasts an awareness update. The server
relays it to all other connected peers. When a tab closes, the WebSocket
disconnects and the peer's state is removed from the awareness map.

The Go server does not parse awareness payloads — it simply forwards them as-is.
This matches the reference y-websocket server behaviour and means any future
awareness fields added by the JS client work automatically.

---

## Project structure

```
examples/collab-editor/
├── server/
│   └── main.go          Go HTTP + WebSocket server
├── client/
│   └── index.html       Single-file browser app (no build step)
└── README.md
```

The WebSocket provider is implemented in:

```
provider/websocket/
├── doc.go               Package-level documentation
└── server.go            websocket.Server — HTTP handler, y-protocols, RFC 6455 framing
```

---

## How this would differ in production

| Concern | This example | Production recommendation |
|---|---|---|
| **TLS** | Plain ws:// on localhost | Use wss:// with a TLS terminator (nginx, Caddy, or Go's `tls.Listen`) |
| **Persistence** | In-memory only; restarting the server loses all documents | Persist `doc.EncodeStateAsUpdate()` to a database on each update; restore on room creation |
| **Authentication** | None | Validate a JWT or session cookie in the HTTP upgrade handler before accepting the WebSocket |
| **Scale-out** | Single process; rooms are in-process maps | Use Redis pub/sub or a message broker to fan out updates across multiple server instances |
| **Back-pressure** | Writes block if a slow client can't keep up | Add per-connection write queues with timeouts and drop-or-close slow peers |
| **Awareness TTL** | Client disconnect cleans up immediately | Add a server-side TTL so stale entries expire even if the disconnect event is missed |
