# Benchmarks

This document explains how to run ygo's benchmark suite, what each tier
measures, and reports real numbers from a local run. It complements the
per-file benchmark docs (`crdt/bench_test.go`, `crdt/bench_heavy_test.go`,
`provider/websocket/scale_bench_test.go`, `provider/websocket/scale_probe_test.go`,
`persistence/scale_bench_test.go`, `cluster/relay_bench_test.go`,
`benchmarks/b4_test.go`), which document individual scenarios in detail.

## Methodology

**Reference machine** (all numbers in this document, unless noted otherwise):

```
$ uname -a
Darwin Nimits-MacBook-Pro.local 25.5.0 Darwin Kernel Version 25.5.0: Tue Jun  9 22:28:34 PDT 2026; root:xnu-12377.121.10~1/RELEASE_ARM64_T6041 arm64
$ sysctl -n machdep.cpu.brand_string hw.ncpu hw.memsize
Apple M4 Max
16
51539607552   # 48 GiB
$ go version
go version go1.26.5 darwin/arm64
```

- **Apple M4 Max, 16 cores, 48 GiB RAM, macOS (Darwin 25.5.0), go1.26.5 darwin/arm64.**
- Every table below is labeled with this machine. Numbers from a different
  machine (CI's `ubuntu-latest` runners, a teammate's laptop, target
  production hardware) will differ — do not treat these as portable SLAs,
  treat them as a baseline for `benchstat` regression comparison on *this*
  machine, and re-run locally before trusting an absolute number for
  capacity planning.
- CI (`.github/workflows/benchmark.yml`) uses `-count=5` on the PR gate, where
  `benchstat` needs enough samples to report a meaningful confidence interval
  and actually gates a decision. The nightly heavy tier uses `-count=3`: `crdt`
  alone costs ~6.5min per sample, so six samples did not fit in the run, and a
  drift artifact produced every night is worth more than six samples in a run
  that never finishes.
- This document's numbers use `-count=3` (light tier) and `-count=1`
  (`-benchtime=10x`, heavy tier) — enough to sanity-check the shape of the
  numbers, not a substitute for `benchstat old.txt new.txt` when evaluating a
  real performance change.
- **Determinism:** every randomized scenario in this suite seeds its PRNG
  from a fixed constant (`rand.New(rand.NewSource(<const>))`) — never
  unseeded `math/rand` or a wall-clock seed. Re-running any benchmark here
  exercises the exact same operation sequence every time; only timing
  varies.

## Run tiers

### Light (PR gate)

Runs on every PR touching `crdt/`, `encoding/`, `benchmarks/`, or the
workflow file itself (see `.github/workflows/benchmark.yml`). Sizes are
bounded (≤ ~2000 ops) so this stays fast enough for the PR gate.

```
go test -bench=. -benchmem ./...
```

### Heavy (nightly + manual)

Gated behind the `benchheavy` build tag. 100k-scale engine benchmarks,
concurrent-conflict scenarios (B2/B3), persistence flush-cost scaling,
relay fan-out/backpressure, and websocket broadcast fan-out. Too slow for
the PR gate; runs nightly on a cron schedule and on demand
(`workflow_dispatch`).

```
go test -tags benchheavy -bench=. -benchtime=10x -benchmem -count=3 -timeout 90m ./...
```

`-benchtime=10x` is required, not optional. These benchmarks are built for a
fixed small iteration count; letting Go auto-scale `b.N` pushes `crdt` and
`persistence` past the default 10m per-package test timeout, and drives
`BenchmarkBroadcastFanout` into back-pressure — its peers cannot drain what an
unthrottled producer enqueues, so the slow-peer policy closes them, the emptied
room is evicted, and the broadcast fails with "room not found". This recipe
omitted the flag until 2026-08-05, and every nightly run from 2026-07-31 onward
failed as a result. `-timeout` is explicit for the same reason: the default is a
silent 10m.

### Scaling probe (nightly + manual)

Not a benchmark — a measurement harness (`TestScaleProbe`) that creates N
rooms directly (bypassing real peer connections) and reports heap/sys
memory, goroutine count, and RSS (Linux only, via `/proc/self/status`) at
that room count, to spot `O(rooms)` growth in server-side resource usage.

```
go test -tags benchheavy -run TestScaleProbe -v ./provider/websocket/
```

By default `TestScaleProbe` runs at both 1,000 and 10,000 rooms. Override
with `SCALE_PROBE_N` to run a single size instead — set it lower (e.g.
`200`) for a quick laptop smoke run, or higher to approximate production
room counts:

```
SCALE_PROBE_N=200 go test -tags benchheavy -run TestScaleProbe -v ./provider/websocket/
```

## Results

All numbers below are from the reference machine described in
**Methodology**, `go1.26.5 darwin/arm64`. `ns/op`, `B/op`, `allocs/op` are
per Go's `testing.B` convention (per-iteration, amortized over `b.N`).

### Light tier

`go test -bench=. -benchmem -count=3 ./crdt/ ./encoding/ ./sync/ ./awareness/`
completed in full (`crdt` 963s — dominated by the pre-existing
`BenchmarkSearchMarker_*_100k_*` scenarios re-run 3x, not by anything added
in this task; `encoding` 70s, `sync` 16s, `awareness` 92s — all `PASS`).
Representative results below (one of the 3 samples per benchmark; given the
fixed-seed scenarios the 3 samples cluster tightly except where noted):

**`crdt` — B1-style engine benches (mirrors `dmonad/crdt-benchmarks` shapes) + wire encode/decode:**

| Benchmark | ns/op | B/op | allocs/op |
|---|---|---|---|
| `YText_Insert` | 1,325 | 1,256 | 16 |
| `YText_InsertBulk` | 2,287 | 1,352 | 16 |
| `YText_Delete` | 1,544,071† | 1,360,004 | 14,000 |
| `YText_Prepend` | 214,750 | 467,681 | 8,068 |
| `YText_RandomInsert` | 763,029 | 508,121 | 10,053 |
| `YText_InsertThenDelete` | 28,339,417 | 3,230,249 | 38,059 |
| `YText_WordInsert` | 20,594,592 | 2,483,694 | 32,034 |
| `YText_MixedEdits` | 1,837,221 | 361,064 | 7,001 |
| `YText_Format` | 2,742 | 2,535 | 32 |
| `YText_ApplyDelta` | 4,396 | 2,592 | 37 |
| `EncodeStateAsUpdateV1` | 41,873 | 23,899 | 16 |
| `ApplyUpdateV1` | 121,962 | 228,478 | 3,087 |
| `EncodeStateAsUpdateV2` | 52,267 | 56,328 | 40 |
| `ApplyUpdateV2` | 123,423 | 231,239 | 3,114 |
| `MergeUpdatesV1` | 138,561 | 273,811 | 3,258 |
| `YMap_Set` | 22,119 | 33,760 | 438 |
| `YArray_Push` | 102,039 | 123,920 | 1,508 |
| `TwoPeerConvergence` | 20,063 | 29,737 | 406 |
| `ConcurrentSamePositionInsert/peers=400` | 7,579,318 | 12,314,931 | 14,659 |
| `ObservedTxn_Apply` | 1,750 | 1,738 | 22 |
| `ObservedTxn_ApplyBaseline` | 858.3 | 1,242 | 16 |

† `YText_Delete` benchmarks a ~2,000-element delete, hence the disproportionate `ns/op` vs. `YText_Insert` above — this is deliberate benchmark shape (single-op vs. bulk-delete cost), not a light/heavy tier inconsistency.

Pre-existing `BenchmarkSearchMarker_*` (search-marker positional-access;
not part of this task, included here only because they live in the same
file and package): 1k/100k warm-marker and cold-cache variants across
Insert/Get/DeleteRange/ApplyDelta all completed, ranging from ~90 ns/op
(warm `ArrayGetRandom_1k`) to ~5 ms/op (cold `DeleteRangeTail_100k`). See
`crdt/bench_test.go` for the full scenario list; not reproduced in full
here to keep this table scoped to Task 1–6's additions.

**`encoding` — varint/varstring/varbytes wire encode/decode:**

| Benchmark | ns/op | MB/s | allocs/op |
|---|---|---|---|
| `WriteVarUint_Small` | 1.768 | 565.6 | 0 |
| `WriteVarUint_Large` | 8.665 | 577.0 | 0 |
| `ReadVarUint_Small` | 1.013 | 987.5 | 0 |
| `ReadVarUint_Large` | 3.219 | 1,553.5 | 0 |
| `WriteVarString_Short` | 3.846 | 2,600.0 | 0 |
| `WriteVarString_Long` | 15.31 | 65,308.2 | 0 |

**`sync` — sync-protocol step1/update/handshake:**

| Benchmark | ns/op | B/op | allocs/op |
|---|---|---|---|
| `EncodeSyncStep1` | 164.8 | 296 | 6 |
| `ApplySyncMessage_Update` | 1,729 | 3,658 | 37 |
| `FullHandshake` | 1,540 | 3,253 | 46 |

**`awareness` — presence state + update encode/apply:**

| Benchmark | ns/op | B/op | allocs/op |
|---|---|---|---|
| `SetLocalState` | 104.4 | 15 | 1 |
| `EncodeUpdate_Single` | 234.4 | 240 | 7 |
| `EncodeUpdate_Many` | 13,494 | 16,747 | 348 |
| `ApplyUpdate_Single` | 775.6 | 2,008 | 26 |
| `ApplyUpdate_Many` | 23,463 | 46,312 | 586 |
| `RemoveExpired` | 5,462 | 5,448 | 15 |

### Heavy tier

`go test -tags benchheavy -bench=. -benchtime=10x -benchmem ./crdt/ ./persistence/ ./cluster/ ./provider/websocket/`
— this completed in full on the reference machine (no OOM, no size
reduction needed):

**`crdt` — 100k-scale random access + conflict (B2/B3):**

| Benchmark | ns/op | B/op | allocs/op |
|---|---|---|---|
| `YArray_RandomGet_100k` | 184,038 | 0 | 0 |
| `YArray_RandomInsert_100k` | 208,079 | 1,265 | 16 |
| `YText_RandomInsert_100k` | 10,829 | 1,392 | 21 |
| `YText_RandomDelete_100k` | 11,125 | 1,688 | 22 |
| `Conflict_B2_TwoUsers` (2×500 ops) | 12,194,792 | 19,634,680 | 21,869 |
| `Conflict_B3_ManyUsers` (10×150 ops) | 432,413,896 | 595,506,409 | 273,836 |

**`persistence` — `MemoryPersistence` flush cost vs. doc size, coalescing throughput:**

| Benchmark | ns/op | B/op | allocs/op | other |
|---|---|---|---|---|
| `MemoryPersistence_FlushVsDocSize/N=1000` | 345,283 | 662,261 | 7,056 | |
| `MemoryPersistence_FlushVsDocSize/N=10000` | 3,473,975 | 7,331,690 | 70,094 | |
| `MemoryPersistence_FlushVsDocSize/N=100000` | 34,912,188 | 78,205,744 | 700,123 | |
| `PersistThroughput_Coalescing/M=1000` | 15,098,808 | 5,540,300 | 73,501 | 71,042 updates/s, 0.999 coalesce-hit-rate |

`FlushVsDocSize` measures `LoadDoc`'s cost as the number of stored
single-op V1 updates grows — i.e. the `MergeUpdatesV1`-over-all-blobs cost
a `LegacyAdapter`-backed `VersionedPersistence` pays on every load when no
compaction has run. On this run the three sampled sizes (1k/10k/100k) each
scaled close to 10x latency per 10x N, which reads as roughly linear at
these specific points — but per Task 5's more granular sweep (see
`persistence/scale_bench_test.go` and the Task 5 report) the underlying
cost is a **superlinear merge-per-flush cost, not strict O(doc²)**:
`MergeUpdatesV1` does struct-level dedup rather than pairwise-quadratic
merging, so it degrades worse than linear but well short of quadratic.
Treat "superlinear, sub-quadratic" as the accurate characterization rather
than either "linear" (an artifact of these three sample points) or
"O(doc²)" (not supported by the measurements).

**`cluster` — `MemRelay` fan-out and backpressure:**

| Benchmark | ns/op | B/op | allocs/op | other |
|---|---|---|---|---|
| `MemRelay_Fanout/Fanout` (8 subscribers) | 416.7 | 64 | 1 | |
| `MemRelay_Fanout/Backpressure` | 469,654,317 | 77,553 | 1,209 | 177.0 publish-timeouts, 200.0 published |

The `Backpressure` sub-benchmark's `publish-timeouts` metric is a
**caller-side publish-timeout count, not a relay-internal drop count**:
`MemRelay.Publish` intentionally blocks rather than drops when a
subscriber's channel is full (see the doc comment on `Publish` in
`cluster/mem_relay.go` and the benchmark's own doc comment in
`cluster/relay_bench_test.go`). This benchmark pairs a 1-slot buffer with a
slow subscriber and a short per-call `context.Context` deadline, so most
`Publish` calls hit that deadline and return `ctx.Err()` — that is what
`publish-timeouts` counts. Real drop-on-full semantics don't exist in
`MemRelay` today; they're deferred to the Redis-relay follow-up (#187,
see the drafted follow-up issue below).

**`provider/websocket` — room creation, reconnect reuse, broadcast fan-out:**

| Benchmark | ns/op | B/op | allocs/op |
|---|---|---|---|
| `GetOrCreateRoom_ConcurrentDistinctRoomLoads` | 21,002,204 | 206,628 | 924 |
| `ReconnectReuse_WarmIdleRoom` | 158.4 | 0 | 0 |
| `BroadcastFanout/N=10` | 2,467 | 2,790 | 38 |
| `BroadcastFanout/N=100` | 6,750 | 4,022 | 39 |
| `BroadcastFanout/N=500` | 234,058 | 13,410 | 63 |

### Scaling probe

`SCALE_PROBE_N=1000 go test -tags benchheavy -run TestScaleProbe -v ./provider/websocket/`
— completed on the reference machine:

```
rooms    heapAllocMiB  sysMiB   goroutines rss
1000     1             12       2          n/a
```

(`rss` is `n/a` because RSS sampling in `TestScaleProbe` reads
`/proc/self/status`, which doesn't exist on macOS — Darwin is where this
document's numbers were captured, so `rss` is unavailable here by
construction, not a measurement failure. Run on Linux to get an RSS
figure.)

**Larger `SCALE_PROBE_N` (5,000 / 10,000+ rooms) and the true "10k rooms"
capacity-planning scenario:** not run for this document — **run on target
hardware.** The reference machine's 48 GiB completed 1,000 simulated rooms
trivially (1 MiB heap, 12 MiB sys), so scaling to 10k+ is plausibly fine on
this machine too, but that is an inference, not a measurement; don't take
it as a substitute for actually running:

```
SCALE_PROBE_N=10000 go test -tags benchheavy -run TestScaleProbe -v ./provider/websocket/
```

on hardware representative of production before using it for capacity
planning.

## Cross-implementation comparison

The engine-scenario benchmarks in this suite (`crdt/bench_test.go`'s
B1-style Prepend/RandomInsert/InsertThenDelete/WordInsert/MixedEdits, and
`crdt/bench_heavy_test.go`'s B2/B3 conflict scenarios, plus the pre-existing
B4 editing-trace suite in `benchmarks/`) are deliberately named and shaped
to mirror the operation definitions from
[`dmonad/crdt-benchmarks`](https://github.com/dmonad/crdt-benchmarks), so
that a future native yjs (JS)/yrs (Rust) run on the same hardware is a
drop-in comparison rather than a re-design.

That native re-run has **not** been done as part of this task — it is
explicitly out of scope here and tracked as a follow-up (see "Draft
follow-up issues" below). Existing prior art already comparing ygo against
yrs/Yjs JS on non-identical hardware exists at
`docs/comparison/ygo-vs-yrs.md` (which itself flags the hardware mismatch
caveat: its Yjs JS numbers are from an Intel Core i5-8400, its ygo numbers
from an Apple M4 Max, so it reports ratios rather than claiming absolute
apples-to-apples parity). This document's heavy-tier and light-tier numbers
above are pure-Go engine/server/persistence/relay measurements on one
machine; they are not yet lined up against yjs/yrs on the same hardware.
That apples-to-apples run is the deferred follow-up.

Separately, an earlier investigation (see
[`move_fuzzer_and_yrs_nonconformance`](https://github.com/reearth/ygo) —
internal project notes) found that yrs 0.21 actually diverges from Yjs's
own reference behavior on several push/insert-with-tombstone scenarios,
while ygo matches Yjs in the same scenarios — a *conformance* result, not
a *performance* one, but relevant context for interpreting any future
cross-impl performance numbers: "faster than yrs" and "as conformant as
yrs" are separate claims, and on the conformance axis ygo already has
better-documented parity with the Yjs reference than yrs does.
