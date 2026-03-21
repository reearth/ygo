# Internals

Deep-dive reference for contributors working on the CRDT core, encoding layer, or wire protocol.

## VarUint encoding

Each byte contributes 7 bits of data. The MSB is a continuation flag: `1` means another byte follows, `0` means this is the last byte. Bits are packed least-significant-first.

```
Value 300 (binary: 100101100)

Byte 0: 1_0101100  (bits 0-6, continuation=1)  → 0xAC
Byte 1: 0_0000010  (bits 7-13, continuation=0) → 0x02
```

Go implementation note: a simple loop shifting right by 7 each iteration, ORing `0x80` on all but the last byte, matches the lib0 JS behaviour exactly.

**Overflow guard:** reject inputs requiring more than 8 bytes (> 53 significant bits) with `ErrOverflow`. JavaScript's `Number` type cannot represent integers beyond 2^53 safely, so the protocol never sends larger values.

## VarInt encoding

Signed integers are ZigZag-encoded before being written as VarUint:

```
encode: (n << 1) ^ (n >> 63)
decode: (n >> 1) ^ -(n & 1)
```

This maps small negative numbers to small positive numbers, keeping the wire representation compact.

## UpdateV1 binary layout

```
Update = numStructs:VarUint
         [struct...]
         deleteSet

struct =
  client:VarUint  clock:VarUint
  info:uint8                      // content type tag + flags
  [content fields...]             // varies by content type
  hasParentSub:bool (from info)
  parentSub?:VarString
  origin?:  client:VarUint clock:VarUint
  originRight?: client:VarUint clock:VarUint

deleteSet =
  numClients:VarUint
  [client:VarUint numRanges:VarUint [clock:VarUint len:VarUint]...]
```

The `info` byte encodes:
- bits 0–4: content type (0=deleted, 1=JSON, 2=binary, 3=string, 4=embed, 5=format, 6=type, 7=any, 8=doc)
- bit 5: has left origin
- bit 6: has right origin
- bit 7: has parent sub (map key)

## UpdateV2 binary layout

V2 groups structs by client and uses differential clock encoding:

```
Update = dsClock:VarUint          // used internally; 0 for standard updates
         numClients:VarUint
         [clientBlock...]
         deleteSet (same as V1)

clientBlock =
  client:VarUint
  numStructs:VarUint
  firstClock:VarUint              // absolute clock of first struct
  [struct...]                     // clocks are implicit (sequential)
```

Because structs within a `clientBlock` are consecutive, there is no need to repeat the client or clock fields. This is the primary source of compression.

## StateVector binary layout

```
numClients:VarUint
[client:VarUint clock:VarUint]...
```

`clock` is the highest integrated clock for that client (i.e., all items up to and including `clock` have been applied).

## Sync message binary layout

All sync messages share a two-byte header before the payload:

```
messageType:VarUint (0=SyncStep1, 1=SyncStep2, 2=Update)
payload:VarBytes
```

- **SyncStep1 payload:** encoded StateVector
- **SyncStep2 payload:** encoded UpdateV1 (missing items + full DeleteSet)
- **Update payload:** encoded UpdateV1 or V2

## Awareness message binary layout

```
numClients:VarUint
[client:VarUint clock:VarUint state:VarBytes(json)]...
```

`state` is a JSON-encoded object (`null` signals that the client has disconnected and its entry should be removed).

## YATA integration — formal description

Given a set of items with a total order on IDs `(client, clock)`, the integration rule for a new item `i` with `origin = L` and `originRight = R` is:

1. Start scanning from the item immediately right of `L` (or the list head if `L` is nil).
2. For each candidate item `c` encountered before `R`:
   - If `c.origin` is to the **left** of `L` in the list, stop — insert `i` before `c`.
   - If `c.origin == L` and `c.client < i.client`, skip `c` (it has priority).
   - Otherwise stop.
3. Insert `i` at the current position.

**Why `originRight`?** If `L` is deleted before `i` arrives, a single-origin approach would have no anchor. `originRight` provides a fallback: scan forward until `R` is found, then apply the rule above relative to the reconstructed position. This is the key correctness property that distinguishes YATA from simpler list CRDTs.

## Run-length squashing (YText optimisation)

When a transaction closes, consecutive `ContentString` items from the same client with no intervening deleted or formatted items are merged into a single item with a longer `length`. This keeps the linked list short for the common case of sequential typing, matching the behaviour of the JS reference implementation and its performance characteristics.

## LRU position cache

Linear search through the linked list to find index `n` is O(n). Yjs maintains a cache of the 80 most-recently-accessed `(type, index) → *Item` mappings. ygo implements the same cache to match reference performance for random-access editing patterns.

## Garbage collection walk

When `doc.GC = true`, at the end of each transaction:

1. Walk the linked list of every changed type.
2. For each deleted item whose `Content` is not `ContentDeleted`:
   - Replace content with `ContentDeleted{Len: item.Content.Len()}`.
   - Attempt to merge with adjacent deleted items (reduces list length).
3. Recursively GC deleted `ContentType` items (their child lists).

GC is skipped for items that are referenced by an active `Snapshot` state vector.

## Known divergences from JS reference

None intentional. Any divergence is a bug. The `TestCompat_*` suite is the authoritative check.
