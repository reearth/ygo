## What's new

A YATA correctness fix and two awareness DoS hardenings, surfaced by an external bug report (#65) and a follow-up architectural audit.

- **YATA `OriginRight` boundary fix** (#65, #68). `Item.integrate`'s conflict-scan loop terminated on `o != item.Right`, but `item.Right` was never resolved from `item.OriginRight` before the loop ran. When an incoming item declared a right boundary via `OriginRight`, the scan had no upper bound and placed the item past concurrent items that share the same `Origin` — causing divergence with Yjs JS and yrs on a class of updates that turned out to include local inserts in the middle of same-client runs as well as remote integration. Now resolves via a new `StructStore.getItemCleanStart` helper at the top of `integrate`, mirroring Yjs JS. 5 new regression tests in `crdt/yata_origin_right_test.go`; no perf regression on the integrate hot path (benchstat n=5).
- **Awareness per-state key cap** (#48, vector A). A small JSON state object with thousands of keys (e.g. `{"k1":1,...,"k65535":1}`) passed the existing 1 MiB byte cap but materialised into a multi-MB `map[string]any`. States with more than 1,000 top-level keys are now dropped silently (treated as null), matching the existing pattern for oversized-state handling.
- **Awareness per-room byte cap** (#48, vector B). Total wire-applied awareness state per `Awareness` instance was unbounded — a single peer could claim up to ~10 GiB by spreading large states across 10,000 clientIDs. New `Awareness.SetMaxBytes(n int64)` API; `provider/websocket.Server.MaxAwarenessBytesPerRoom` plumbs the cap to each room at creation. Default is unlimited (backward compatible); suggested production value is 100 MiB.

## Install

```
go get github.com/reearth/ygo@v1.8.1
```

See [CHANGELOG.md](https://github.com/reearth/ygo/blob/main/CHANGELOG.md) for full details.
