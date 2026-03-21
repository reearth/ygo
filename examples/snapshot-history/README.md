# snapshot-history

Demonstrates document versioning using `ygo` snapshots.

```
go run ./examples/snapshot-history
```

## What is a snapshot?

A **snapshot** is a compact record of a Yjs document's state at a specific point in time. It contains two things:

1. **State vector** — for each peer (client ID), the highest clock value that had been integrated when the snapshot was taken. This tells you exactly which items existed.
2. **Delete set** — the set of clock ranges that had been marked deleted at snapshot time.

A snapshot does **not** duplicate item content. It only records *which* items existed and *which* were deleted. The actual content bytes stay in the document's item history. This makes snapshots very small regardless of document size — their size is O(number of clients), not O(document size).

## Wire format

The snapshot encoding is compatible with the JavaScript Yjs library's `Y.encodeSnapshot` / `Y.decodeSnapshot`:

```
VarUint(numClients)
  for each client:
    VarUint(clientID)
    VarUint(clock)            ← state vector entry

VarUint(numClients)           ← delete set header
  for each client:
    VarUint(clientID)
    VarUint(numRanges)
      for each range:
        VarUint(clock)
        VarUint(length)
```

Both sections use the same variable-length integer encoding (`lib0`) used throughout the Yjs protocol.

## When to use snapshots

| Use case | How snapshots help |
|---|---|
| Version history (like Google Docs) | Take a snapshot after each save; restore on demand |
| Audit trails | Attach snapshots to audit log entries |
| Server-level undo/redo | Store a snapshot stack; pop to restore |
| Share a historical revision | Use `EncodeStateFromSnapshot` to generate a shareable update |
| Checkpoint before a risky merge | Snapshot before applying an unknown update; restore if needed |

## The GC trade-off

| Setting | Memory | Full restoration |
|---|---|---|
| `WithGC(false)` | Higher (all item content kept) | Yes — any past snapshot can be fully restored |
| `WithGC(true)` (default) | Lower (deleted content freed) | Partial — only live items can be recovered |

For documents where version history matters, create the `Doc` with `WithGC(false)`. For read-heavy replicas or caches where you only need the current state, the default `GC(true)` is fine.

## Storing snapshots

Snapshot bytes are opaque binary blobs. Store them anywhere you store binary data:

```go
// Capture and encode
data := crdt.EncodeSnapshot(crdt.CaptureSnapshot(doc))

// Store in a database
_, err := db.Exec("INSERT INTO snapshots (doc_id, revision, data) VALUES (?, ?, ?)",
    docID, revision, data)

// Later: decode and restore
snap, err := crdt.DecodeSnapshot(data)
restored, err := crdt.RestoreDocument(doc, snap)
```

## RestoreDocument vs EncodeStateFromSnapshot

Both functions work with a snapshot, but serve different purposes:

### `RestoreDocument(doc, snap) (*Doc, error)`

Creates a new, independent `Doc` that reflects the document state at snapshot time. Deletions from the snapshot's delete set are applied, so the result is an exact replica of the document as it appeared at that moment.

Use this when you want to **read** or **display** a past version locally.

```go
snap, _ := crdt.DecodeSnapshot(storedBytes)
historical, _ := crdt.RestoreDocument(doc, snap)
fmt.Println(historical.GetText("article").ToString())
```

### `EncodeStateFromSnapshot(doc, snap) []byte`

Returns a standard V1 update containing only the items that existed at snapshot time, with an empty delete set. The recipient can apply this update to a fresh document and use it as a **starting point for new edits** at that historical revision.

Use this when you want to **share** a past version with another peer.

```go
snap, _ := crdt.DecodeSnapshot(storedBytes)
update := crdt.EncodeStateFromSnapshot(doc, snap)

peer := crdt.New()
crdt.ApplyUpdateV1(peer, update, nil)
// peer now has the document as of that snapshot, ready for further editing
```

## Performance characteristics

- **Snapshot size**: proportional to the number of distinct peers (clients), not the document size. A document with 3 peers and 1 million characters still produces a tiny snapshot (a few dozen bytes).
- **Capture time**: O(number of clients × items per client) for building the delete set from the item store.
- **Restoration time**: O(number of items up to snapshot clock) for re-encoding and re-applying.
- **Memory impact**: Zero — snapshots hold only integer values, no content strings.
