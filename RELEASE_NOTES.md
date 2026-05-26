## What's new

First post-audit release. Focused exclusively on `crdt/` internal performance — no public API changes.

### `YText.Delete` -47% on head-delete workloads (#86)

The `cleanupDanglingFormatsInRegion` walk introduced in v1.12.0 became O(N²) for sequential head-deletes: it started from `txt.start` on every call, re-skipping every accumulated tombstone before reaching live content. The fix combines two complementary optimisations, both inspired by a cross-reference comparison against Yjs JS and yrs:

1. **`hasFormatting` gating** (mirrors Yjs's `_hasFormatting` flag). `abstractType` now flips a `hasFormatting` bit the first time a `ContentFormat` item is integrated. `YText.Delete` skips the cleanup walk entirely on YText types that have never had `Format()` called — the dominant cost on plain-text head-delete workloads.
2. **`firstLiveCache` extended to `deleteRange`**. `abstractType` memoises the first live item from `t.start`; both `deleteRange` and the cleanup walk now resume from that cached pointer instead of re-walking leading tombstones on every call.

**Benchstat n=5 on `BenchmarkYText_Delete`: -46.77% (2.87ms → 1.53ms)** per 1000-char delete loop. Geomean across the hot-path suite: **-8.09% sec/op**, **0.00% B/op**, **0.00% allocs/op**.

### Transaction allocation hygiene (#54 A)

`Transaction.changed` is now pre-sized to capacity 4 — most transactions touch 1-3 types and the prior zero-hint allocation forced immediate rehashing on the first append.

Two related candidates from #54 (`newItems` pre-sizing and YATA conflict-scan map reuse via `clear()`) were measured and reverted because they net-regressed other benchmarks. They remain candidates for a future PR once the cost model is right.

### Why not a full Yjs structural port

A cross-reference comparison against Yjs JS showed that Yjs's `cleanupContextlessFormattingGap` model has different cleanup semantics — it only removes duplicate-key markers in a contiguous gap, not orphan opener/closer pairs whose effect zone has no live content. ygo's existing `cleanupDanglingFormatsInRegion` is intentionally more aggressive and is what closes #71 vector A4. We kept ygo's richer cleanup and adopted only Yjs's `_hasFormatting` gating + the `firstLiveCache` optimisation, which together give the YText_Delete win without weakening cleanup semantics.

## Install

```
go get github.com/reearth/ygo@v1.16.0
```

See [CHANGELOG.md](https://github.com/reearth/ygo/blob/main/CHANGELOG.md) for full details.
