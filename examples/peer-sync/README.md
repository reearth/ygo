# peer-sync example

This example demonstrates the ygo sync protocol in complete isolation — no network,
no HTTP, no WebSockets. Two (and then three) in-process Go peers exchange binary
messages through a channel, showing exactly how the y-protocols sync handshake works
at the byte level.

```
go run ./examples/peer-sync
```

---

## Why transport-agnostic sync matters

The sync protocol only knows about byte slices (`[]byte`). It does not care whether
those bytes travel over a WebSocket frame, an HTTP POST body, a TCP stream, a Unix
pipe, or a Go channel. This design has two important consequences:

1. **Testability** — you can exercise the entire sync logic in a unit test without
   spinning up any network infrastructure. This example is effectively that unit test
   made verbose and educational.

2. **Flexibility** — the same `sync.ApplySyncMessage` and `sync.EncodeSyncStep1`
   functions power every provider in this repository. If you understand this example,
   you understand the WebSocket provider and the HTTP provider — they are just
   wrappers that move the bytes across a different channel.

---

## The three message types

| Type byte | Constant        | Direction     | Purpose |
|-----------|-----------------|---------------|---------|
| `0x00`    | `MsgSyncStep1`  | initiator → peer | "I have up to clock X — send me what I'm missing" |
| `0x01`    | `MsgSyncStep2`  | peer → initiator | "Here is everything you are missing (V1 update)" |
| `0x02`    | `MsgUpdate`     | any → any     | "Here is a new incremental change (V1 update)" |

Step1 and Step2 together form the **initial handshake**. After the handshake, all
subsequent edits travel as `MsgUpdate` messages.

---

## The full handshake

```
Peer A                               Peer B
  │                                    │
  │──── SyncStep1(sv_A) ─────────────▶ │  A shares its state vector
  │ ◀─── SyncStep2(diff_A) ──────────── │  B replies with what A is missing
  │                                    │
  │ ◀─── SyncStep1(sv_B) ────────────── │  B shares its state vector
  │──── SyncStep2(diff_B) ────────────▶ │  A replies with what B is missing
  │                                    │
  │       (both peers converged)       │
  │                                    │
  │──── Update(delta) ────────────────▶ │  A sends an incremental edit
  │ ◀─── (applied, no reply needed) ─── │
```

The handshake is **bidirectional**: each peer must send a `SyncStep1` to the other so
that both directions of the diff are exchanged. In this example Alice initiates, then
Bob initiates, and after both step-2 messages arrive each peer holds the union of all
operations.

---

## How `ApplySyncMessage` works

```go
reply, err := sync.ApplySyncMessage(doc, msg, origin)
```

- If `msg[0] == MsgSyncStep1`: decodes the remote state vector, encodes a `MsgSyncStep2`
  containing the diff, and **returns the reply bytes**. The caller is responsible for
  sending the reply back to the remote peer.
- If `msg[0] == MsgSyncStep2`: applies the contained V1 update to `doc`; returns `nil`
  (no reply expected).
- If `msg[0] == MsgUpdate`: applies the contained V1 update to `doc`; returns `nil`.

The `origin` parameter is threaded through to the document's `OnUpdate` observers so
you can identify which peer triggered each change (useful for loop-prevention in relay
architectures).

---

## The CRDT convergence guarantee

The CRDT used by ygo is based on **YATA** (Yet Another Transformation Approach).
Every insert operation carries:

- Its own unique `(ClientID, Clock)` identifier
- The ID of the item immediately to its left at the moment of insertion (`Origin`)
- The ID of the item immediately to its right at the moment of insertion (`OriginRight`)

These pointers allow the algorithm to replay any set of concurrent inserts in a
deterministic order, regardless of the sequence in which they are received. Two
peers who receive the same set of operations will always produce byte-for-byte
identical documents. This property — **strong eventual consistency** — means the
sync protocol never needs a central coordinator to resolve conflicts.

For concurrent inserts at the same position, the peer with the **lower ClientID**
is placed first. In this example Alice has ClientID 1 and Bob has ClientID 2, so
Alice's text always precedes Bob's in the merged result.

---

## Adapting to a real transport

The only thing you need to change is where the bytes go. Here is a sketch for
WebSocket:

```go
// Server side: relay messages between all connected clients.
func handleConn(doc *crdt.Doc, conn *websocket.Conn) {
    // Send initial step-1 to the new client.
    conn.WriteMessage(websocket.BinaryMessage, sync.EncodeSyncStep1(doc))

    for {
        _, msg, err := conn.ReadMessage()
        if err != nil {
            return
        }
        reply, err := sync.ApplySyncMessage(doc, msg, conn.RemoteAddr())
        if err != nil {
            log.Println("sync error:", err)
            continue
        }
        if reply != nil {
            conn.WriteMessage(websocket.BinaryMessage, reply)
        }
    }
}
```

The `sync` package handles all encoding. You handle reading and writing bytes.

---

## The role of the state vector in incremental sync

A **state vector** is a compact map from `ClientID → clock`. It says: "I have
integrated every operation from client X up to and including clock Y."

When you send a `SyncStep1`, you attach your current state vector. The recipient
uses it to compute the exact set of operations you have not yet seen — nothing
more, nothing less. This makes the initial handshake bandwidth-efficient even for
large documents with long histories.

For incremental updates (post-handshake), you capture the state vector **before**
an edit, make the edit, then call:

```go
delta := crdt.EncodeStateAsUpdateV1(doc, svBeforeEdit)
msg   := sync.EncodeUpdate(delta)
```

The resulting `msg` contains only the newly inserted operations — typically a
single tiny item per keystroke.

---

## Interoperability

The binary wire format — state vectors, V1 updates, and the three message types —
is identical to that used by:

- **y-websocket** (JavaScript, the reference implementation)
- **y-protocols** (Rust)
- **Hocuspocus** (Node.js server)
- Any other Yjs-compatible implementation

A ygo peer can synchronise directly with a JavaScript browser tab using the same
three message types described here.
