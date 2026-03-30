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

The benchmarks fall back to a synthetic trace when the real dataset is absent.
To use the authentic B4 data:

1. Download `editing-trace.json` from
   <https://github.com/dmonad/crdt-benchmarks/blob/master/benchmarks/editing-trace.json>
2. Place it at `benchmarks/testdata/editing-trace.json`
3. Re-run the benchmarks — the loader will detect the file automatically.

## Targets

| Benchmark       | Target    | Description                                 |
|-----------------|-----------|---------------------------------------------|
| B4 Apply        | < 2 s     | Apply full 260 k-op trace to empty document |
| B4 Encode V1    | < 200 ms  | Encode final state as V1 binary update      |
| B4 Encode V2    | < 300 ms  | Encode final state as V2 binary update      |
| B4 Decode       | < 200 ms  | Decode + apply full V1 update               |

V2 encoding is allowed more time than V1 because its column-oriented RLE
compression produces a meaningfully smaller payload (typically 30–50% of V1
size), trading encoding CPU for network bandwidth.

## Latest Results

Measured on Apple M4 Max (darwin/arm64, Go 1.26.1) using the synthetic B4-equivalent
trace (260 000 ops). All targets met.

```
goos: darwin
goarch: arm64
pkg: github.com/reearth/ygo/benchmarks
cpu: Apple M4 Max

BenchmarkB4_Apply-16       1   1293414375 ns/op   289662896 B/op   3374045 allocs/op
BenchmarkB4_Encode-16      1     10054959 ns/op    28260640 B/op         75 allocs/op
BenchmarkB4_EncodeV2-16    1     12104375 ns/op    35870712 B/op     259087 allocs/op
BenchmarkB4_Decode-16      1     38755959 ns/op    44623624 B/op     777056 allocs/op
BenchmarkB4_Size-16        1   1277818000 ns/op    2832510 v1_bytes   259060 v2_bytes   9.146 v2_pct_of_v1
```

| Benchmark    | Target    | Measured  | Status |
|--------------|-----------|-----------|--------|
| B4 Apply     | < 2 s     | ~1.29 s   | ✅     |
| B4 Encode V1 | < 200 ms  | ~10 ms    | ✅     |
| B4 Encode V2 | < 300 ms  | ~12 ms    | ✅     |
| B4 Decode    | < 200 ms  | ~39 ms    | ✅     |
| V2 size      | < V1 size | 9% of V1  | ✅     |

> To reproduce: `go test -bench=BenchmarkB4 -benchmem -benchtime=1x -count=3 ./benchmarks/`

<!-- BASELINE_PLACEHOLDER -->
