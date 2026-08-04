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

### Common pitfalls

- **Always obtain shared-type handles outside `Transact` callbacks.** `doc.GetText`, `doc.GetArray`, `doc.GetMap`, `doc.GetXmlFragment` and the other root accessors acquire the doc's write lock — which `Transact` already holds. Calling them inside the callback deadlocks. The same applies to reads like `text.ToString()` and `arr.ToSlice()`. Pattern:

  ```go
  // CORRECT — capture the handle before Transact.
  txt := doc.GetText("content")
  doc.Transact(func(txn *crdt.Transaction) {
      txt.Insert(txn, 0, "hello", nil)
  })

  // DEADLOCK — GetText inside Transact tries to re-acquire the write lock.
  doc.Transact(func(txn *crdt.Transaction) {
      doc.GetText("content").Insert(txn, 0, "hello", nil) // hangs forever
  })
  ```

  README has the user-facing version of this warning; it's repeated here because tests are the most common place to hit it. If a new test hangs indefinitely, this is almost always the cause.

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

## PR conventions

We follow a few conventions that aren't enforced by tooling but help keep the project tidy. Reviewers may push back if these aren't met.

### Commit messages

Use [Conventional Commits](https://www.conventionalcommits.org/) prefixes: `feat:`, `fix:`, `refactor:`, `docs:`, `chore:`, `test:`, `perf:`. Add a scope when it adds information:

```
feat(crdt): add TransactE error variant (#14)
fix(provider/websocket): plug runWriter goroutine leak (#33)
docs(persistence): document PersistenceAdapterContext extension
```

### Closing issues

Use a separate `Closes #N` keyword for each issue your PR resolves. Comma-separated lists like `Closes #A, #B, #C` only close the first issue — GitHub's auto-close parser ignores the rest. The right form:

```
Closes #14, closes #15, closes #16
```

See [GitHub's docs on closing keywords](https://docs.github.com/en/issues/tracking-your-work-with-issues/linking-a-pull-request-to-an-issue#linking-a-pull-request-to-an-issue-using-a-keyword) for the full list of recognised verbs.

### Branch naming

- `chore/v<version>-<topic>` for release-targeted work (e.g., `chore/v1.7.0-additive-context-and-errors`)
- `chore/<topic>` otherwise
- `fix/<topic>` for isolated bug fixes

### Hot-path changes need benchstat

Changes inside `Item.integrate`, `applyV1Txn`, or other functions called once per item per update must include a `benchstat` comparison over at least n=10 samples:

```bash
git checkout main
go test -bench=ApplyUpdateV1 -benchmem -count=10 -run=^$ ./crdt/ > before.txt

git checkout your-branch
go test -bench=ApplyUpdateV1 -benchmem -count=10 -run=^$ ./crdt/ > after.txt

benchstat before.txt after.txt
```

Three samples are not enough to distinguish real change from noise — we found this out the hard way (see closed issue #22). The PR description should include the `benchstat` output for any row that changed.

### CHANGELOG and release notes

Two files need an entry, both in your own branch. `main` is PR-protected, so anything left for "later" costs a second, docs-only PR before the release can be tagged.

1. **`CHANGELOG.md`** — add your change under the next version's heading (e.g. `## [1.43.0] — 2026-08-14`). We do **not** use an `[Unreleased]` section: entries go directly into the version they ship in. If you don't know which version that is, ask in the PR and a maintainer will tell you — it follows from your change's API surface, not its intent (a new exported symbol is a MINOR even in a bug-fix PR).

2. **`RELEASE_NOTES.md`** — add a section for the same version, headed `## v<version>` to match the tag. `.github/workflows/release.yml` extracts the file's **top section only** and publishes it as the GitHub release body. The heading must equal the tag being pushed: if it doesn't, the release job fails rather than attaching the previous version's notes to your tag.

Both files are newest-first: your new section goes at the top, above the previous release.

**Exemption.** A PR that changes no library behaviour — documentation, tests, CI, tooling, benchmarks — needs neither entry, and does not get a release of its own. Those changes ride along with whatever release comes next. If you are unsure whether your PR qualifies, assume it does not and add the entries; an unnecessary entry is trivial to drop in review, a missing one blocks a tag.

## Pull Request Checklist

Before opening a PR ensure:

- [ ] `make test` passes (with `-race`)
- [ ] `make lint` passes with no new warnings
- [ ] `make vet` passes (no vulnerabilities)
- [ ] New tests are added for changed behaviour
- [ ] `CHANGELOG.md` is updated under the next version's heading (e.g. `## [1.43.0]`) — not `[Unreleased]`
- [ ] `RELEASE_NOTES.md` has a section for that same version
- [ ] Wire-format changes regenerate fixtures (`make fixtures`) and update `TestCompat_` tests
- [ ] Doc comments updated for any changed public API
- [ ] PR description links the related issue (`Closes #NNN`)

## Reproducing a convergence fuzz failure (#70)

`testutil/fuzz` runs a randomized, seed-reproducible convergence fuzzer as part
of `go test ./...` (see `crdt/fuzz_test.go`). On failure it prints a seed. To
replay that one scenario:

    FUZZ_SEED=<seed> go test ./crdt/ -run TestFuzzSeed -v

To explore more of the space locally (nightly-style), raise the iteration count:

    FUZZ_ITER=50000 go test ./crdt/ -run TestFuzzConvergence -timeout 30m

`TestFuzzCrossImpl` additionally replays each scenario against real Yjs and
compares the results; it needs Node + `yjs` (`cd testutil && npm ci`) and skips
automatically when they are absent. A minimized failing scenario (from
`fuzz.Shrink`) should be saved as a JSON file under `testutil/fuzz/corpus/` so
it is replayed on every run.

## Cross-language conformance (#99)

Cross-impl tests compare ygo against real Yjs. In CI they are hard-gated:
`YGO_REQUIRE_NODE=1` makes them **fail** (not skip) if Node/yjs is unavailable,
so coverage can never silently vanish. Locally they skip cleanly without Node.

Checked-in fixtures under `crdt/testdata/*_yjs_fixtures.json` are generated
from the pinned `yjs@13.6.30`. After an intentional wire change, regenerate
and commit:

    cd testutil && npm ci && cd .. && make fixtures

`make fixtures` regenerates only the conformance JSON fixtures under
`crdt/testdata/`; it does not touch `testutil/fixtures/*.bin`, which are
produced separately by `gen_fixtures.js`/`gen_fixtures_v2.js` and are not
drift-gated.

The `Conformance fixture drift` CI job fails if committed fixtures don't match a
fresh generation. (Attribution / `yjs14` fixtures are excluded until yjs v14 is
final.)
