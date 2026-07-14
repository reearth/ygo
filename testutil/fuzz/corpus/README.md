# Convergence fuzz corpus

Each `*.json` file is a frozen fuzz `Scenario` (see `../op.go`) that once
exposed a real convergence bug. `TestFuzzCorpus` (in `crdt/fuzz_test.go`)
replays every file through `RunGo` + `Converged` on each run, so a regression
in the same shape fails fast and node-free.

**Do not delete entries.** Add new ones by saving the minimized output of
`Shrink` (see `../shrink.go`) after a fuzzer or oracle failure.

`TestFuzzCorpus` checks *internal* (ygo-vs-ygo) convergence. Cross-implementation
(ygo-vs-Yjs) regressions are guarded separately by `TestFuzzCrossImpl` and by the
hand-written regressions in `crdt/` (e.g. `TestYArrayPush_TombstonedTail_MatchesYjs`,
`TestYText_InsertAdjacentTombstone_MatchesYjs`).

## Entries

- **`parent-by-id-mergev2.json`** — a low-client peer creates an XML `<div>`; a
  high-client peer sets an attribute on it (a keyed child resolved by
  parent-item-ID); delivery back uses `MergeV2`. Exercises the parent-by-ID
  resolution + V2 merge path behind the rightOrigin / parent-by-ID convergence
  work (#65, #68).
