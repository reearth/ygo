## What's new

Fourth in the [`gaps` label](https://github.com/reearth/ygo/issues?label=gaps) series. Two HIGH-severity correctness bugs in the delete pipeline, both reachable through normal wire-protocol traffic.

- **`Item.delete` now cascades into `ContentType` children** (#72 vector B1). Deleting a container item that held a nested `YMap` / `YArray` / `YText` previously only tombstoned the outer item; the inner children stayed live and the encoded delete-set omitted them. Peers that held the same nested type saw inconsistent state. The fix walks the nested type head-to-tail and recursively deletes each child, matching Yjs JS and yrs. Cascade depth is unbounded.

- **`DeleteSet.applyToPartial` now splits items at range boundaries before tombstoning** (#72 vector B2). A partial-range delete-set entry against a locally-squashed run previously wiped the whole item, including content outside the declared range. Two-peer trigger: one side squashes text the sender saw as multiple items, then receives a partial-range delete from the sender. `applyToPartial` now pre-splits at both range boundaries via the existing `getItemCleanStart` helper so every affected item lies entirely inside the range before being deleted. Matches Yjs JS `iterateDeletedStructs` and yrs `Update::integrate`.

Both fixes are behavior changes (no new exported API), so this is a patch release. Benchmark results: no regression on the integrate / apply / two-peer convergence hot paths (benchstat n=5; allocations and bytes bit-identical with main).

## Install

```
go get github.com/reearth/ygo@v1.11.1
```

See [CHANGELOG.md](https://github.com/reearth/ygo/blob/main/CHANGELOG.md) for full details.
