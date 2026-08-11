# Persistence Adapter Pattern

The WebSocket server exposes a `PersistenceAdapter` interface that lets you
store and restore room state across server restarts without modifying any
server code.  This document explains the contract, shows concrete
implementations for common backends, and covers multi-node deployment.

> **Two layers.** `PersistenceAdapter` (`LoadDoc` / `StoreUpdate`) is the
> **low-level primitive**: a head blob plus an append hook, with no notion of
> history. The `persistence` package builds a **`VersionedPersistence`** layer
> on top of it — append-only versioned history, point-in-time materialisation,
> named snapshots, and crash-safe pruning — and ships a `LegacyAdapter` so a
> versioned store still plugs into `NewServerWithPersistence`. See
> [Versioned persistence](#versioned-persistence-the-persistence-package) below.
> For live cross-node fan-out (documents **and** awareness) see
> [CLUSTERING.md](CLUSTERING.md).

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

`MemoryPersistence` stores room state in process memory. It **appends** each
incremental update — an O(update) write — rather than re-merging the whole
document on every call, which used to cost O(document) per write and
O(document²) over a session (#186).

It bounds that append log itself: once a room accumulates `CompactEvery`
writes (default 500, matching `provider/client`'s own compaction default) it
folds the room's backlog into a single blob. `LoadDoc` also folds first —
materialising the room's records is what makes a load coherent regardless of
where the room sits in its append cycle; the fold only makes the *next* load
cheap again, since a load always merges whatever records are present whether
or not it folds first.

One behaviour change worth knowing if you handle `LoadDoc`'s error: it could
previously never return an error, and now can — an unmergeable append log
surfaces at load time rather than at write time. `StoreUpdate`/`AppendUpdate`
validate each update as it is written, so there is no practical way to reach
this in normal operation, but the failure mode has moved.

Measured per-write cost, old merge-on-write vs. append-then-compact, at three
room sizes:

| Updates already in room | Before | After | Improvement |
|---|---|---|---|
| 100 | 13,975 ns/op | 1,457 ns/op | 9.6× |
| 1,000 | 132,871 ns/op | 1,710 ns/op | 77.7× |
| 10,000 | 1,676,329 ns/op | 4,653 ns/op | 360× |

The growth constant is divided by `CompactEvery`, **not eliminated**: per-write
cost is still `append + O(document)/CompactEvery`, so it keeps growing with
document size, just ~500× more slowly. Flat, constant-time writes are not
achievable for this storage model — see the trade below.

The trade this makes explicit: `LoadDoc` is no longer O(1). It folds whatever
records the room still holds — bounded by `CompactEvery` — and persists that
fold, so subsequent loads are cheap again until the log builds back up.
Writes are continuous and loads happen once per room residency, so the
direction is right, but folding is inherently O(document) — there is no way
to keep returning one V1 blob from `LoadDoc` without periodically paying for
it. This is still primarily for tests and single-process deployments; a
multi-process deployment wants `persistence/sqlite` or another
`VersionedPersistence`.

```go
srv := websocket.NewServerWithPersistence(websocket.NewMemoryPersistence())
```

To change the compaction threshold, set `CompactEvery` on the adapter before
serving:

```go
adapter := websocket.NewMemoryPersistence()
adapter.CompactEvery = 2000 // fold less often; more memory, fewer folds
srv := websocket.NewServerWithPersistence(adapter)
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

> **Prefer the `cluster` package for new deployments.** The adapter-based
> pattern below piggy-backs cross-node fan-out on `StoreUpdate` and carries
> **document updates only** — awareness/presence is *not* relayed, so each node
> only sees its own peers' cursors (see the table at the end of this section).
> The first-class [`cluster.Relay`](CLUSTERING.md) carries **both document
> updates and awareness**, with a built-in echo guard. The pattern below remains
> valid for document-only setups and as a reference for wiring fan-out onto a
> persistence backend.

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
| Awareness state | This adapter pattern relays document updates only. For cluster-wide presence use [`cluster.Relay`](CLUSTERING.md) (`KindAwareness`) |
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

---

## Versioned persistence (the `persistence` package)

The `github.com/reearth/ygo/persistence` package layers an **append-only,
versioned** store on top of the `PersistenceAdapter` primitive. Where
`PersistenceAdapter` only knows the current head, `VersionedPersistence` keeps
the full sequence of updates as numbered versions, can rebuild the document at
any past version, holds named snapshots, and prunes/compacts the log
crash-safely.

Everything is stored in lib0 **V1** internally — V1 is the only format ygo can
merge (`crdt.MergeUpdatesV1`). Convert at the edges with `crdt.UpdateV1ToV2` /
`UpdateV2ToV1` if you need V2.

### The interface

```go
// github.com/reearth/ygo/persistence

type Version uint64
type VersionMeta struct {
    Version   Version
    UpdatedAt time.Time
}
type LoadResult struct {
    Update  []byte  // merged V1 head state (nil for an empty room)
    Version Version // highest version folded into Update, or 0
}

type VersionedPersistence interface {
    Load(ctx context.Context, room string) (LoadResult, error)
    AppendUpdate(ctx context.Context, room string, update []byte) (Version, error)
    ListVersions(ctx context.Context, room string) ([]VersionMeta, error)        // newest-first; single (non-cumulative) updates
    GetUpdate(ctx context.Context, room string, v Version) (update []byte, meta VersionMeta, ok bool, err error)
    MaterializeAt(ctx context.Context, room string, v Version) ([]byte, error)   // rebuild V1 head at v (MergeUpdatesV1)
    CaptureSnapshot(ctx context.Context, room, name string, state []byte) (Version, error)
    RestoreSnapshot(ctx context.Context, room, name string) (update []byte, v Version, ok bool, err error)
    PruneAfter(ctx context.Context, room string, target Version, rolledBack []byte) error
    Compact(ctx context.Context, room string, keep int) (deleted int, err error)
    Delete(ctx context.Context, room string) error
}
```

Semantics:

- **Versions** are dense, per-room, monotonically increasing sequence numbers
  assigned by `AppendUpdate`, starting at 1. `0` is the "empty room" sentinel.
- **`ListVersions`** returns metadata **newest-first**; each entry is a single,
  *non-cumulative* update. An unknown room yields an empty slice (not an error).
- **`MaterializeAt(v)`** folds every update with version ≤ `v` into a full V1
  head via `MergeUpdatesV1`. `v == 0` materialises to empty.
- **Snapshots** are named V1 blobs you supply — typically
  `EncodeStateAsUpdateV1` of the materialised doc (a *portable head blob*, **not**
  a `crdt.Snapshot` state-vector marker). `CaptureSnapshot` returns the head
  version it is pinned to; `RestoreSnapshot` returns `(blob, version, ok)`.
- **`PruneAfter(target, rolledBack)`** is **snapshot-before-delete**: it first
  persists a checkpoint (the `target` ceiling + the `rolledBack` head you pass,
  usually from `MaterializeAt(target)`), and only then deletes the updates newer
  than `target`. The checkpoint is a *hard ceiling* on the visible version
  range, so a crash between the checkpoint write and the deletes can never
  resurrect a "future" version on reopen — this is the spurious-future-version
  guard.
- **`Compact(keep)`** folds the oldest updates into the oldest retained record
  (preserving materialised state) and returns the count removed. `keep <= 0`
  keeps everything.

### Reference implementations

| Type | Backing | Notes |
|------|---------|-------|
| `persistence.NewMemoryPersistence()` | in-process maps | reference impl; simplest conformance target |
| `persistence.NewFilePersistence(dir)` | one directory per store | atomic temp+rename writes; `checkpoint` file is the crash-safety pivot; `Reopen()` models a restart |

### Conformance suite

The package exports a reusable, table-driven behavioural suite so external
adapters (e.g. a GCS-backed store in another repo) verify themselves with one
call:

```go
import "github.com/reearth/ygo/persistence"

func TestMyStore(t *testing.T) {
    persistence.RunConformance(t, func() persistence.VersionedPersistence {
        return mystore.New(/* fresh, empty */)
    })
}
```

`RunConformance` covers: append → `ListVersions` newest-first; `GetUpdate`;
`MaterializeAt` rebuilding correct state; `PruneAfter` removing future versions;
**crash-safe prune** (no spurious future versions after a mid-prune crash);
`Compact` trimming the oldest; and `CaptureSnapshot`/`RestoreSnapshot`
round-trip.

Two **optional** interfaces unlock extra coverage; implement them if your store
can model them, otherwise the relevant subtest is skipped (with a notice):

```go
// Lets the suite simulate a crash between PruneAfter's checkpoint write and
// its deletes. Without it, the crash-safety subtest is skipped.
type CrashInjector interface {
    SetCrashAfterCheckpoint(fn func() bool)
}

// Models a process restart by returning a fresh handle over the same backing
// store (file stores reopen the dir; in-memory stores return themselves).
type Reopener interface {
    Reopen() (VersionedPersistence, error)
}
```

Both `MemoryPersistence` and `FilePersistence` implement both, so the
crash-safety regression runs against each.

### Plugging into the WebSocket server (`LegacyAdapter`)

`VersionedPersistence` does not match the provider's `PersistenceAdapter`
signature directly (`Load`/`AppendUpdate` vs `LoadDoc`/`StoreUpdate`). The
`LegacyAdapter` shim bridges them:

```go
store := persistence.NewFilePersistence("/var/lib/ygo")
srv := websocket.NewServerWithPersistence(persistence.NewLegacyAdapter(store))
```

The mapping: `LoadDoc` → `Load().Update` (materialised head), `StoreUpdate` →
`AppendUpdate` (the assigned `Version` is dropped). Because the provider calls
`StoreUpdate` once per committed transaction, **every transaction becomes one
version** — you get the full history for free. `LegacyAdapter` also implements
the optional `StoreUpdateContext` (see below), and `NewLegacyAdapterContext`
threads a shutdown context through every store call. Reach the versioned API
(history, snapshots, prune) via `adapter.Store()` or by keeping a reference to
the underlying store.

---

## Context-aware adapters (v1.7.0+)

The `PersistenceAdapter` interface is unchanged. v1.7.0 added an optional extension that adapters can implement to receive a `context.Context` cancelled when `Server.Shutdown` begins:

```go
type PersistenceAdapterContext interface {
    StoreUpdateContext(ctx context.Context, room string, update []byte) error
}
```

The persistence worker checks for this interface at runtime via a type assertion. Adapters that implement both `PersistenceAdapter` and `PersistenceAdapterContext` get the context-aware path; others fall back to `StoreUpdate` with no behavior change.

### When to implement it

Implement `PersistenceAdapterContext` when your `StoreUpdate` body can take longer than a few hundred milliseconds — typical for adapters that hit a network or slow disk. Otherwise the legacy interface is fine.

### Example: Postgres adapter that respects ctx

```go
type PostgresAdapter struct {
    db *sql.DB
}

func (p *PostgresAdapter) LoadDoc(room string) ([]byte, error) {
    var data []byte
    err := p.db.QueryRow(
        "SELECT state FROM rooms WHERE name = $1", room,
    ).Scan(&data)
    if errors.Is(err, sql.ErrNoRows) {
        return nil, nil
    }
    return data, err
}

func (p *PostgresAdapter) StoreUpdate(room string, update []byte) error {
    return p.storeUpdate(context.Background(), room, update)
}

func (p *PostgresAdapter) StoreUpdateContext(ctx context.Context, room string, update []byte) error {
    return p.storeUpdate(ctx, room, update)
}

func (p *PostgresAdapter) storeUpdate(ctx context.Context, room string, update []byte) error {
    _, err := p.db.ExecContext(ctx,
        "INSERT INTO updates(room, payload) VALUES ($1, $2)",
        room, update,
    )
    return err
}
```

During `Server.Shutdown`, the in-flight `ExecContext` call sees the context cancellation and returns early instead of waiting for the database driver's default timeout.

This pattern mirrors `io.WriterTo`, `http.CloseNotifier`, and `database/sql/driver.QueryerContext` in the standard library — extension interfaces that callers can opt into without breaking older implementations.

---

## Shutdown durability, and what it does not promise (v1.49.0+)

Cancellation exists to unwedge a slow adapter, not to decide which writes
count. Since v1.49.0 (#229) that separation is enforced:

- **Every final flush uses `context.Background()`**, on both the coalescing and
  the per-update path, so the last write is never aborted by the very signal
  that triggered it. Before v1.49.0 the per-update path passed the cancellable
  context to its shutdown drain, and a ctx-aware adapter discarded the whole
  tail.
- **A store aborted by cancellation is retained, not dropped.** The update is
  re-stored on the exit path under a background context — the same
  retain-and-re-flush rule that already applied to a coalesced batch.
- **A transaction committed while `Shutdown` is running still reaches the
  adapter.** Peer read loops keep committing for the whole
  close-connections-and-join window, so the room's persistence worker publishes
  its retirement before its final drain; a commit that arrives too late for
  that drain is written by the committing goroutine itself.
- **`Shutdown` joins those writes** (bounded by the caller's context) before
  returning, so `srv.Shutdown(ctx)` followed by exiting the process does not
  kill a write in flight.

### The guarantee, stated exactly

> **Precondition:** you have stopped accepting new WebSocket connections before
> calling `Shutdown` (shut down your `http.Server`, or otherwise stop routing to
> the handler).
>
> **Then:** for every room that was present in the server and had finished
> loading when `Shutdown` began, any commit whose `Transact` returned before
> `Shutdown` returned is durable.

Both halves are load-bearing.

**Why the precondition.** `Shutdown` snapshots the room set once, and skips any
room still mid-load at that instant. `ServeHTTP` has no shutdown gate, so a
connection accepted while `Shutdown` is running can create a room, or finish
loading one, *after* that snapshot. `Shutdown` never waits on such a room's
persistence worker, so a commit into it can return from `Transact` with the
update merely buffered — and the usual `Shutdown(ctx); return` shape then kills
the drain. This is not new in v1.49.0; what is new is that the rest of this
section would otherwise read as promising against it. Stop accepting
connections first and the room set cannot grow underneath `Shutdown`.

**Why the second half is still not losslessness.** **`Shutdown` is not lossless
and cannot be made so.** A transaction that *begins* committing after `Shutdown`
has observed its last in-flight write is not covered: the producers are peer
read loops and any code holding a `*crdt.Doc`, none of which the server can
join, and inventing a join would risk a `Shutdown` that never returns — a worse
failure than the one being fixed.

### What adapters must now tolerate

- **A `StoreUpdate` arriving late in, or just after, `Shutdown`.**
- **A blocking `StoreUpdate` costing the caller its deadline.** Because the
  final flush runs under `context.Background()`, an adapter that never returns
  wedges that room's worker at exit and `Shutdown` returns
  `context.DeadlineExceeded` where it previously returned `nil` — by discarding
  your data. The honest error replaces a silent lie, but it is a behaviour
  change if you have a `Shutdown` deadline.
- **A second, unrelated cause of that same `DeadlineExceeded`:** a *sustained*
  producer. `Shutdown` waits for the in-flight commit count to reach zero, and
  that count covers every committing goroutine, not only the ones performing a
  write themselves. Code that holds a retained `*crdt.Doc` and commits in a
  tight loop can keep it above zero for as long as it runs, so `Shutdown` burns
  its whole deadline with no wedged adapter anywhere. Stop your writers, not
  just your connections, if you want `Shutdown` to return early.
- **Writes for a room the server already considers gone.** Nothing calls
  `doc.Destroy` on teardown, so a caller that retains the `*crdt.Doc` (from
  `GetDoc`, or from an `OnLoadDocument` hook) keeps its persistence observer
  alive and its later commits are written directly — indefinitely. Before
  v1.49.0 those commits were silently dropped.
- **`Compact` overlapping a `StoreUpdate` for the same room NAME.** The
  provider serialises `Compact` with `StoreUpdate` per room *instance*, not per
  name, and the point above widens that window. If your `Compact` mutates state
  keyed by name, serialise inside the adapter. Every store this repo ships
  behind `LegacyAdapter` already does, each serialising its writers under a
  single mutex: `persistence.MemoryPersistence` and `persistence.FilePersistence`
  do so for every operation, reads included; `persistence/sqlite`'s reads are
  deliberately lock-free instead and consistent by other means, but its
  `Compact` and `AppendUpdate` are both writers serialised under its own
  mutex — the property this section actually relies on.

Straggler writes also bypass coalescing, auto-versioning and compaction, and
delay that update's relay fan-out (the persistence observer runs before the
relay observers). All of this is scoped to room retirement — except the
retained-`*crdt.Doc` case, which is not.
