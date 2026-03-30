# Persistence Adapter Pattern

The WebSocket server exposes a `PersistenceAdapter` interface that lets you
store and restore room state across server restarts without modifying any
server code.  This document explains the contract, shows concrete
implementations for common backends, and covers multi-node deployment.

---

## The Interface

```go
// provider/websocket/server.go
type PersistenceAdapter interface {
    // LoadDoc returns the full binary V1 update for the room, or (nil, nil)
    // if no state exists yet.
    LoadDoc(room string) ([]byte, error)

    // StoreUpdate is called with each incremental V1 update produced by a
    // committed transaction.  Implementations must be safe for concurrent
    // calls from different goroutines.
    StoreUpdate(room string, update []byte) error
}
```

`LoadDoc` is called once when the first peer connects to a room.  The returned
bytes are passed to `crdt.ApplyUpdateV1` to seed the in-memory document.

`StoreUpdate` is called on every committed transaction via `doc.OnUpdate`.
For durability the implementation should write to stable storage before
returning; the server does not retry failed writes.

### Registering an adapter

```go
adapter := mybackend.NewAdapter(db)
srv := websocket.NewServerWithPersistence(adapter)
http.Handle("/yjs/{room}", srv)
```

---

## Built-in: MemoryPersistence

`MemoryPersistence` merges all incremental updates into a single V1 snapshot
per room and stores it in process memory.  It is useful for tests and
single-process deployments where durability across restarts is not required.

```go
srv := websocket.NewServerWithPersistence(websocket.NewMemoryPersistence())
```

---

## Redis (append-log strategy)

Store each incremental update as a separate Redis list entry.  On load, read
all entries and merge them into one snapshot.  This avoids a read-modify-write
on every write, making `StoreUpdate` a simple `RPUSH`.

```go
package redisadapter

import (
    "context"
    "github.com/redis/go-redis/v9"
    "github.com/reearth/ygo/crdt"
)

type Adapter struct{ rdb *redis.Client }

func New(rdb *redis.Client) *Adapter { return &Adapter{rdb: rdb} }

func key(room string) string { return "ygo:room:" + room }

func (a *Adapter) LoadDoc(room string) ([]byte, error) {
    ctx := context.Background()
    entries, err := a.rdb.LRange(ctx, key(room), 0, -1).Result()
    if err != nil || len(entries) == 0 {
        return nil, err
    }
    updates := make([][]byte, len(entries))
    for i, e := range entries {
        updates[i] = []byte(e)
    }
    return crdt.MergeUpdatesV1(updates...)
}

func (a *Adapter) StoreUpdate(room string, update []byte) error {
    return a.rdb.RPush(context.Background(), key(room), update).Err()
}
```

**Compaction:** The list grows without bound.  Run a periodic job that calls
`LoadDoc` (which merges), then replaces the list with a single merged entry:

```go
func Compact(rdb *redis.Client, room string) error {
    a := New(rdb)
    merged, err := a.LoadDoc(room)
    if err != nil || len(merged) == 0 {
        return err
    }
    pipe := rdb.Pipeline()
    pipe.Del(context.Background(), key(room))
    pipe.RPush(context.Background(), key(room), merged)
    _, err = pipe.Exec(context.Background())
    return err
}
```

---

## PostgreSQL (single-row upsert strategy)

Store the merged V1 snapshot as a single `BYTEA` column per room.  Each
`StoreUpdate` call does a read-merge-write inside a transaction.  Suitable
when the document update rate is low (<100 updates/s per room).

```sql
CREATE TABLE ygo_docs (
    room    TEXT PRIMARY KEY,
    doc     BYTEA NOT NULL,
    updated TIMESTAMPTZ NOT NULL DEFAULT now()
);
```

```go
package pgadapter

import (
    "context"
    "database/sql"
    "github.com/reearth/ygo/crdt"
)

type Adapter struct{ db *sql.DB }

func New(db *sql.DB) *Adapter { return &Adapter{db: db} }

func (a *Adapter) LoadDoc(room string) ([]byte, error) {
    var doc []byte
    err := a.db.QueryRowContext(context.Background(),
        `SELECT doc FROM ygo_docs WHERE room = $1`, room,
    ).Scan(&doc)
    if err == sql.ErrNoRows {
        return nil, nil
    }
    return doc, err
}

func (a *Adapter) StoreUpdate(room string, update []byte) error {
    ctx := context.Background()
    tx, err := a.db.BeginTx(ctx, nil)
    if err != nil {
        return err
    }
    defer tx.Rollback()

    var existing []byte
    err = tx.QueryRowContext(ctx,
        `SELECT doc FROM ygo_docs WHERE room = $1 FOR UPDATE`, room,
    ).Scan(&existing)
    if err != nil && err != sql.ErrNoRows {
        return err
    }

    var merged []byte
    if len(existing) == 0 {
        merged = update
    } else {
        merged, err = crdt.MergeUpdatesV1(existing, update)
        if err != nil {
            return err
        }
    }

    _, err = tx.ExecContext(ctx, `
        INSERT INTO ygo_docs (room, doc, updated)
        VALUES ($1, $2, now())
        ON CONFLICT (room) DO UPDATE SET doc = $2, updated = now()
    `, room, merged)
    if err != nil {
        return err
    }
    return tx.Commit()
}
```

For high update rates consider batching: buffer updates for ~100 ms and merge
them in a single DB round-trip.

---

## Multi-node deployment

A single `websocket.Server` holds documents in process memory.  When you run
multiple server instances behind a load balancer, peers connected to different
nodes will not see each other's edits unless you add a cross-node relay.

### Architecture

```
Browser A ──→ Node 1 ──→ PersistenceAdapter (Postgres / Redis)
                  ↕ pub/sub (Redis PUBLISH / NATS / etc.)
Browser B ──→ Node 2 ──→ PersistenceAdapter (same DB)
```

Each node:
1. Writes every incremental update to the shared DB via `StoreUpdate`.
2. Publishes the raw update bytes to a pub/sub channel.
3. Subscribes to the same channel and applies incoming updates to its
   in-memory document, then broadcasts to its local peers.

### Redis pub/sub adapter

```go
package clusteredadapter

import (
    "context"
    "github.com/redis/go-redis/v9"
    "github.com/reearth/ygo/crdt"
    wsadapter "github.com/reearth/ygo/provider/websocket"
)

type ClusteredAdapter struct {
    rdb    *redis.Client
    server *wsadapter.Server // pointer to the local server for doc access
}

func New(rdb *redis.Client, srv *wsadapter.Server) *ClusteredAdapter {
    a := &ClusteredAdapter{rdb: rdb, server: srv}
    go a.subscribe()
    return a
}

func channel(room string) string { return "ygo:updates:" + room }
func snapshotKey(room string) string { return "ygo:snap:" + room }

// LoadDoc fetches the persisted snapshot.
func (a *ClusteredAdapter) LoadDoc(room string) ([]byte, error) {
    val, err := a.rdb.Get(context.Background(), snapshotKey(room)).Bytes()
    if err == redis.Nil {
        return nil, nil
    }
    return val, err
}

// StoreUpdate merges the incremental update into the snapshot and publishes
// it so that peer nodes apply it to their in-memory documents.
func (a *ClusteredAdapter) StoreUpdate(room string, update []byte) error {
    ctx := context.Background()

    // Merge into the persistent snapshot (Redis atomic compare-and-set omitted
    // for brevity — use a Lua script or WATCH/MULTI/EXEC in production).
    existing, err := a.rdb.Get(ctx, snapshotKey(room)).Bytes()
    if err != nil && err != redis.Nil {
        return err
    }
    var merged []byte
    if len(existing) == 0 {
        merged = update
    } else {
        merged, err = crdt.MergeUpdatesV1(existing, update)
        if err != nil {
            return err
        }
    }
    if err := a.rdb.Set(ctx, snapshotKey(room), merged, 0).Err(); err != nil {
        return err
    }

    // Publish the raw incremental update to sibling nodes.
    msg := append([]byte(room+"\n"), update...)
    return a.rdb.Publish(ctx, "ygo:updates", msg).Err()
}

// subscribe listens for updates published by sibling nodes and applies them
// to the local in-memory document.
func (a *ClusteredAdapter) subscribe() {
    pubsub := a.rdb.Subscribe(context.Background(), "ygo:updates")
    defer pubsub.Close()
    ch := pubsub.Channel()
    for msg := range ch {
        payload := []byte(msg.Payload)
        nl := -1
        for i, b := range payload {
            if b == '\n' { nl = i; break }
        }
        if nl < 0 { continue }
        room := string(payload[:nl])
        update := payload[nl+1:]

        doc := a.server.GetDoc(room)
        if doc == nil { continue }
        _ = crdt.ApplyUpdateV1(doc, update, "remote-node")
        // NOTE: broadcasting to local peers is handled by the doc's OnUpdate
        // observer registered in getOrCreateRoom.
    }
}
```

### Key points for multi-node

| Concern | Recommendation |
|---------|----------------|
| Update ordering | CRDTs converge regardless of order — no coordination needed |
| Snapshot races | Use optimistic locking (Redis `WATCH` or Postgres `FOR UPDATE`) |
| Room fan-out | Partition rooms by consistent hash if pub/sub throughput is a concern |
| Awareness state | Awareness is ephemeral — each node stores only its own peers' cursors |
| Reconnect | On connect, server sends full V1 snapshot → client converges immediately |

---

## Testing your adapter

The simplest test applies a series of updates, reloads from storage, and
verifies the document state round-trips correctly:

```go
func TestAdapter(t *testing.T, adapter PersistenceAdapter) {
    const room = "test-room"

    // Write some updates.
    doc := crdt.New()
    txt := doc.GetText("content")
    doc.Transact(func(txn *crdt.Transaction) {
        txt.Insert(txn, 0, "hello", nil)
    })
    update := crdt.EncodeStateAsUpdateV1(doc, nil)
    if err := adapter.StoreUpdate(room, update); err != nil {
        t.Fatal(err)
    }

    // Reload from storage.
    stored, err := adapter.LoadDoc(room)
    if err != nil {
        t.Fatal(err)
    }

    fresh := crdt.New()
    if err := crdt.ApplyUpdateV1(fresh, stored, nil); err != nil {
        t.Fatal(err)
    }

    got := fresh.GetText("content").ToString()
    if got != "hello" {
        t.Fatalf("got %q, want %q", got, "hello")
    }
}
```
