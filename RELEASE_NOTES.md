## What's new

Wire-framing performance pass. Focused on the two largest sources of allocation churn on the WebSocket send / receive paths.

### sync.Pool for Encoder across all send paths (#52)

Every `sendSync`, `broadcastSync`, `sendAwareness`, `broadcastAwareness`, and update-encoder call site previously allocated a fresh `*Encoder` plus its 64-byte initial buffer. With six call sites in `provider/websocket/peer.go` and the doc-update encoder in `crdt/update.go`, a 100-peer room doing 10 updates/sec/peer paid ~7000 small allocations/sec purely on wire-framing overhead.

New helpers in `encoding/`:

- **`encoding.GetEncoder` / `encoding.PutEncoder`** — pool primitives.
- **`encoding.EncodeBytes(fn)`** — the recommended wrapper. Gets an encoder from the pool, runs `fn`, copies the result into an independent allocation, and returns the encoder to the pool. Safe to hand the returned bytes to write channels.

### Zero-copy decoder paths (#53)

- **`Decoder.RemainingBytes`** now returns a sub-slice instead of a copy. The previous behaviour is preserved under the new name `RemainingBytesCopy` for callers that need an independent allocation. Documented contract: treat the result as read-only.
- **`Awareness.ApplyUpdate`** decodes JSON payloads as `[]byte` end-to-end (via `ReadVarBytes`, which already returned a sub-slice) and passes them directly to `json.Unmarshal`. Pre-fix, every entry incurred two copies: one via `ReadVarString`'s `[]byte→string` conversion, and one via `json.Unmarshal([]byte(s), ...)`. On `BenchmarkApplyUpdate_Many` (100 entries): **-15.97% allocs/op** (626 → 526), **-9.93% sec/op** on the single-entry variant.

## Benchstat n=5 vs main

```
                   sec/op       vs base
awareness:
  SetLocalState        64.90n   -5.34%
  EncodeUpdate_Single  224.5n   -10.52%
  EncodeUpdate_Many    13.00µ   -7.85%
  ApplyUpdate_Single   686.0n   -9.93%   (also -4.35% allocs/op)
  ApplyUpdate_Many     21.41µ   ~        (-15.97% allocs/op, 100 fewer allocs)
  geomean                       -7.97% sec/op, -3.58% allocs/op
encoding:
  geomean                       -1.81% sec/op, neutral allocs
```

## Install

```
go get github.com/reearth/ygo@v1.17.0
```

See [CHANGELOG.md](https://github.com/reearth/ygo/blob/main/CHANGELOG.md) for full details.
