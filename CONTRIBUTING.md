# Contributing to ygo

Thank you for your interest in contributing to ygo!

## Prerequisites

| Tool | Version |
|------|---------|
| Go | 1.23+ |
| make | any recent version |
| Node.js | 18+ (only for regenerating test fixtures) |

Install Go tooling dependencies with:

```sh
make tools
```

## Make Targets

| Target | Description |
|--------|-------------|
| `make test` | Run all tests with the race detector |
| `make lint` | Run golangci-lint across all packages |
| `make fuzz` | Run all fuzz targets for 60 seconds each |
| `make bench` | Run all benchmarks and write results to `benchmarks/latest.txt` |
| `make fixtures` | Regenerate binary golden fixtures via Node/Yjs |
| `make coverage` | Produce `coverage.txt` + `coverage.html` |
| `make tools` | Install golangci-lint and govulncheck |
| `make fmt` | Format all Go source with gofmt |
| `make vet` | Run `go vet` and `govulncheck` |
| `make clean` | Remove generated artefacts |

## Coding Standards

### Formatting and Linting

- All code must be formatted with `gofmt` before committing (`make fmt`).
- All code must pass `golangci-lint` (`make lint`). The lint configuration lives in `.golangci.yml`.
- Do **not** suppress linter warnings without a code comment explaining why.

### Dependencies

- **Core packages** (`encoding/`, `crdt/`, `crdt/types/`, `sync/`, `awareness/`) must have **zero external runtime dependencies**. Only the standard library is permitted.
- Provider packages (`provider/websocket/`, `provider/http/`) may depend on well-maintained standard-library-adjacent packages.
- Test-only dependencies are unrestricted but must be in `go.mod` under `require … // indirect` if transitive.

### Error Handling

- **Errors must always be returned**, never silently swallowed. Use `fmt.Errorf("context: %w", err)` for wrapping.
- Sentinel errors live at package level, exported, and end in `Err` prefix (e.g., `ErrUnexpectedEOF`).
- `panic` is only acceptable in package `init`, provably unreachable branches, or programmer-error guards (use `//nolint:panic` with a comment).

### Style

- Prefer table-driven tests.
- Keep functions short and focused. Large switch statements on content types are acceptable in `encoding/` and `crdt/`.
- Unexported types and functions should still have doc comments where the behaviour is non-obvious.

## Testing Expectations

ygo uses four test layers:

### Layers

| Layer | Naming convention | Tag / flag | Location |
|-------|-------------------|------------|----------|
| Unit | `TestUnit_<Type>_<Scenario>` | (none) | `*_test.go` beside source |
| Integration | `TestInteg_<Scenario>` | `//go:build integration` | `*_integ_test.go` |
| Compatibility | `TestCompat_<Fixture>` | (none) | `*_compat_test.go`, loads `testutil/fixtures/*.bin` |
| Fuzz | `FuzzX` | `-fuzz` flag | `*_fuzz_test.go` |
| Benchmark | `BenchmarkX` | `-bench` flag | `*_bench_test.go` |

### Expectations

- Every new exported function must have at least one `TestUnit_` test.
- Behaviour changes that affect wire format must update or add a `TestCompat_` test and regenerate fixtures with `make fixtures`.
- New fuzz targets must be registered in the `fuzz` Makefile target and the `fuzz.yml` workflow.
- Benchmarks should cover hot paths in `encoding/` and `crdt/`.

## Conventional Commits

Commit messages must follow [Conventional Commits](https://www.conventionalcommits.org/en/v1.0.0/):

```
<type>[optional scope]: <description>

[optional body]

[optional footer(s)]
```

**Types:** `feat`, `fix`, `docs`, `style`, `refactor`, `test`, `chore`, `perf`, `ci`, `build`

Examples:

```
feat(crdt): implement YXmlElement attribute merging
fix(encoding): handle VarUint overflow past 53 bits
perf(crdt): add LRU position cache for large documents
test(encoding): add fuzz corpus entries for edge cases
```

Breaking changes must include `BREAKING CHANGE:` in the footer or `!` after the type:

```
feat(sync)!: change SyncStep2 message layout for V2 updates
```

## Pull Request Checklist

Before opening a PR ensure:

- [ ] `make test` passes (with `-race`)
- [ ] `make lint` passes with no new warnings
- [ ] `make vet` passes (no vulnerabilities)
- [ ] New tests are added for changed behaviour
- [ ] `CHANGELOG.md` is updated under `[Unreleased]`
- [ ] Wire-format changes regenerate fixtures (`make fixtures`) and update `TestCompat_` tests
- [ ] Doc comments updated for any changed public API
- [ ] PR description links the related issue (`Closes #NNN`)
