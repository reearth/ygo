## What's new

The final correctness PR closing the cross-reference audit. v1.15.0 fully resolves [#74](https://github.com/reearth/ygo/issues/74) and [#78](https://github.com/reearth/ygo/issues/78), wrapping up the [`gaps` label](https://github.com/reearth/ygo/issues?label=gaps) work tracked across v1.9.0–v1.15.0.

### Observer event shape parity with Yjs (#74)

- **`YMapEvent.Keys`** — new `map[string]KeyChange` field carries the per-key change action (`KeyAdded` / `KeyUpdated` / `KeyDeleted`) plus the `OldValue` for updates and deletes. The legacy `KeysChanged` set stays populated for backwards compatibility; new code should prefer `Keys`. Mirrors Yjs JS's `YMapEvent.keys`.
- **`YArrayEvent.Delta`** — new `[]Delta` field carries Quill-style insert / retain / delete ops with their values, matching the existing `YTextEvent.Delta` shape. Trailing retains are elided per Quill convention. Pre-fix, array observers received only `Target` and `Txn` and had to recompute the diff themselves.

### Transaction-end housekeeping (#78)

- **Auto-GC at transaction commit (H1)** — with `WithGC(true)` (the default), items tombstoned during a transaction have their content replaced with a length-only `ContentDeleted` placeholder at commit time. Long collaborative sessions no longer accumulate full content for items that have been deleted and will never be observable again. Auto-GC runs *after* the observer-delta computation, so subscribers still see the original content. It is suppressed while an `UndoManager` is attached so undo / redo can still restore deleted items.
- **Transient-split re-merge (H2)** — when `splitItem` produces a right half and no item ends up integrated between the two halves before commit, the halves are reunited. Prevents linked-list fragmentation in long edit sessions. Mirrors Yjs's `_mergeStructs` / `tryToMergeWithLeft`.

## Audit complete

This release ships the last of the gaps surfaced by the cross-reference audit against Yjs JS and yrs. The full list (#71 through #79) is now closed. See [CHANGELOG.md](https://github.com/reearth/ygo/blob/main/CHANGELOG.md) for the per-release breakdown.

## Install

```
go get github.com/reearth/ygo@v1.15.0
```

See [CHANGELOG.md](https://github.com/reearth/ygo/blob/main/CHANGELOG.md) for full details.
