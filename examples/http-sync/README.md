# HTTP Sync Example

This example shows how two Go peers can synchronise a Yjs document over plain
HTTP using the ygo HTTP provider. No WebSockets, no long-polling — just a
REST pull-push pattern that works with any HTTP client including `curl`.

## Running the example

```
go run ./examples/http-sync
```

Expected output (byte counts will vary slightly as the CRDT encoding evolves):

```
=== Phase 1: Start HTTP server ===
Server listening at http://127.0.0.1:<random-port>

Peer A (ClientID=1) and Peer B (ClientID=2) created.

=== Phase 2: Peer A inserts content and pushes to server ===
Peer A full-state update size: 30 bytes
Peer A pushed update to server (room: shared-notes)

=== Phase 3: Peer B pulls from server and applies the diff ===
Peer B state vector size: 1 bytes (empty doc)
Received diff from server: 30 bytes
Peer B GetText("notes") = "Hello from Peer A!"

=== Phase 4: Incremental sync — only new content travels ===
Peer A full-state update size after 2nd write: 54 bytes
Incremental diff size (only new content): 35 bytes
Bandwidth saving: 35 bytes vs 54 bytes full update
Peer B GetText("notes") = "Hello from Peer A! And more from Peer A."

=== Phase 5: Peer B writes and syncs back ===
Peer A set greetings["peerA"] and pushed to server.
Peer B set greetings["peerB"] and pushed to server.

=== Summary ===
All changes converged. Both peers see identical content.

Peer A greetings map: map[peerA:Hello from Peer A! peerB:And Peer B says hello too!]
Peer B greetings map: map[peerA:Hello from Peer A! peerB:And Peer B says hello too!]

Peer A notes text: "Hello from Peer A! And more from Peer A."
Peer B notes text: "Hello from Peer A! And more from Peer A."

Notes text: CONVERGED
Greetings map: CONVERGED
```

## What HTTP sync is and when to use it

HTTP sync implements the Yjs state-vector diff protocol on top of ordinary
HTTP request/response pairs:

- **POST /doc/{room}** — a peer pushes a binary update to the server. The
  server applies it to its in-memory document for that room.
- **GET /doc/{room}?sv=\<base64\>** — a peer pulls everything it is missing.
  The `sv` parameter is a base64-encoded state vector; the server returns only
  the diff the client has not yet seen.

Use HTTP sync when:

- You need the simplest possible deployment (no WebSocket upgrade, no
  persistent connections, works through any proxy or CDN).
- Clients sync infrequently (mobile apps, batch pipelines, offline-first
  tools that reconcile when they come online).
- You want to debug sync traffic with `curl` without any special tooling.
- Your existing infrastructure already handles HTTP but not WebSocket.

## The pull-push pattern

```
Peer                        Server
 |                             |
 |--- POST /doc/room (update)->|  push local changes
 |                             |  server merges into its doc
 |                             |
 |-- GET /doc/room?sv=<sv> --->|  pull remote changes
 |                             |  server encodes diff(doc, sv)
 |<--- binary update ----------|
 |  apply update locally       |
```

A **state vector** (SV) is a compact map of `{clientID: highestClockSeen}`.
It summarises everything a peer has already integrated. When the server
receives an SV it calls `EncodeStateAsUpdateV1(serverDoc, sv)`, which returns
only the items whose clock exceeds the client's known maximum for each
client — nothing more.

## Bandwidth advantage of incremental sync

Phase 4 in the demo illustrates this directly. After Peer B has already
synced Phase 2's content, it captures its SV and then Peer A adds more text.
When Peer B pulls with the captured SV, the server returns only the new
fragment, not the entire document:

| Transfer            | Size  |
|---------------------|-------|
| Full document (GET without sv) | ~54 bytes |
| Incremental diff (GET with sv) | ~35 bytes |

At small scales this difference is modest, but for documents with thousands
of edits the savings are proportional to the number of items already
integrated: a client that is only one edit behind pays the cost of one
edit, regardless of how large the document has grown.

## Extending to a real server

The `yhttp.Server` implements `http.Handler`, so plugging it into a real
`net/http` server requires only a few extra lines:

```go
package main

import (
    "log"
    "net/http"

    yhttp "github.com/reearth/ygo/provider/http"
)

func main() {
    mux := http.NewServeMux()
    mux.Handle("/doc/", yhttp.NewServer())
    log.Println("Listening on :9090")
    log.Fatal(http.ListenAndServe(":9090", mux))
}
```

For production use you would also want:

- Persistence: serialize each room's document to a database on every POST
  (use `crdt.EncodeStateAsUpdateV1` to get the bytes).
- Authentication: add middleware before the yhttp handler.
- Room lifecycle: evict idle rooms from memory after a timeout.

## Using curl for debugging

```bash
# Encode Peer B's empty state vector (0x00 = VarUint 0, meaning no clients)
echo -n '\x01\x00' | base64 > sv_empty.b64

# Pull the full document
curl "http://localhost:9090/doc/my-room?sv=$(cat sv_empty.b64)"

# Push an update from a file
curl -X POST http://localhost:9090/doc/my-room \
     -H 'Content-Type: application/octet-stream' \
     --data-binary @update.bin
```

The binary format is the standard Yjs V1 update encoding, compatible with
the JavaScript `Y.applyUpdate` / `Y.encodeStateAsUpdate` functions.

## HTTP vs WebSocket

| Property | HTTP pull-push | WebSocket |
|---|---|---|
| Connection | Stateless, one request per sync | Persistent, full-duplex |
| Server push | No — clients must poll | Yes — server can push immediately |
| Proxy/CDN | Works everywhere | Requires upgrade support |
| Complexity | Minimal | Higher (connection management, reconnect) |
| Latency | Polling interval | Near real-time |
| Offline-first | Natural fit | Requires reconnect logic |

Choose WebSocket when you need collaborative editing with sub-second
convergence and can afford the connection overhead. Choose HTTP when you
prioritise simplicity, compatibility, or infrequent sync.

## Use cases

- **Mobile sync**: apps upload their local changes and download the diff when
  they regain connectivity.
- **Offline-first web apps**: a service worker queues POST requests; on
  reconnect it flushes the queue and pulls the diff.
- **Batch processing**: a pipeline stage reads the current document state,
  runs a transformation, and pushes the result back.
- **Debugging and testing**: `curl` is all you need to inspect or inject
  document state without running a full client.
- **Low-frequency collaboration**: documents that are shared but rarely
  edited concurrently (e.g. shared configuration files, design assets).
