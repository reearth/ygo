# Benchmarks

Performance baselines for the ygo CRDT library, measured against the
[B4 editing trace](https://github.com/dmonad/crdt-benchmarks) — 260 k
real-world collaborative text-editing operations.

## Running

```bash
# Run all benchmarks (3 repetitions, memory allocation stats)
make bench

# Run only B4 benchmarks
go test -bench=BenchmarkB4 -benchmem -count=3 ./benchmarks/

# Compare two branches (requires benchstat)
go install golang.org/x/perf/cmd/benchstat@latest
git stash          # or switch branches
make bench         # writes benchmarks/latest.txt
git stash pop
make bench         # overwrites benchmarks/latest.txt
benchstat benchmarks/baseline.txt benchmarks/latest.txt
```

## Using the real B4 trace

The benchmarks load `benchmarks/testdata/editing-trace.json` when present and
fall back to a synthetic trace otherwise.  The real trace is checked in to this
repository at that path (extracted from the crdt-benchmarks JS module).

```bash
# Benchmarks pick it up automatically — no extra steps needed.
go test -bench=BenchmarkB4 -benchmem -count=3 ./benchmarks/
```

## Targets

| Benchmark       | Target    | Description                                 |
|-----------------|-----------|---------------------------------------------|
| B4 Apply        | < 2 s     | Apply full 182 k-op real trace to empty doc |
| B4 Encode V1    | < 200 ms  | Encode final state as V1 binary update      |
| B4 Encode V2    | < 300 ms  | Encode final state as V2 binary update      |
| B4 Decode       | < 2 s     | Decode + apply full V1 snapshot (3.4 MB)    |

**Decode target note:** Decoding a full-state V1 snapshot of the B4 document
requires allocating and integrating 182 k individual `Item` objects (one per
character insertion in the original trace), resulting in ~932 k heap allocations
and significant GC pressure.  The practical ceiling on current hardware is
~1–2 s.  Future work (arena allocation, inlined `*ID` fields) could bring this
below 200 ms; tracked in ROADMAP Phase 9.

V2 encoding is allowed more time than V1 because its column-oriented RLE
compression produces a meaningfully smaller payload (typically 5–10% of V1
size), trading encoding CPU for network bandwidth.

## Latest Results

Measured on Apple M4 Max (darwin/arm64) using the real B4 editing trace
(182 315 ops, 259 778 total with deletes).

```
goos: darwin
goarch: arm64
pkg: github.com/reearth/ygo/benchmarks
cpu: Apple M4 Max

BenchmarkB4_Apply-16       1   1421463750 ns/op  1202052232 B/op  3096985 allocs/op
BenchmarkB4_Encode-16    122      9703058 ns/op    24000456 B/op       69 allocs/op
BenchmarkB4_EncodeV2-16  134      8989514 ns/op    23215707 B/op   182435 allocs/op
BenchmarkB4_Decode-16      1   1178102000 ns/op  1043572616 B/op   931973 allocs/op
BenchmarkB4_Size-16        1   1521173375 ns/op     3428241 v1_bytes   234910 v2_bytes   6.852 v2_pct_of_v1
```

| Benchmark    | Target    | Measured   | Status |
|--------------|-----------|------------|--------|
| B4 Apply     | < 2 s     | ~1.40 s    | ✅     |
| B4 Encode V1 | < 200 ms  | ~9.7 ms    | ✅     |
| B4 Encode V2 | < 300 ms  | ~9.0 ms    | ✅     |
| B4 Decode    | < 2 s     | ~1.18 s    | ✅     |
| V2 size      | < V1 size | 6.9% of V1 | ✅     |

> To reproduce: `go test -bench=BenchmarkB4 -benchmem -benchtime=1x -count=3 ./benchmarks/`

<!-- BASELINE_PLACEHOLDER -->
