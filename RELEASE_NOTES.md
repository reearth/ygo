## What's new

Two correctness fixes from the cross-reference audit, plus a long-overdue README refresh. After this lands the audit will have one remaining PR (#74 D1+D2 and #78) targeted for v1.15.0.

- **`ToJSON` / `ToSlice` / `Entries` now recursively unwrap nested shared types** (#75). Pre-fix, a YArray containing a nested YMap silently dropped the nested map from `ToSlice` output, and `ToJSON` round-trips lost data. Now nested types serialize as their JSON-equivalent values (YArray → `[]any`, YMap → `map[string]any`, YText → `string`, YXml* → XML string). Arbitrarily-deep nesting recurses cleanly. Matches Yjs JS's `toJSON` convention.

- **`YTextEvent.Delta` now reports embed inserts/deletes/retains** (#74 D3). Observers were missing embed events entirely because `computeDelta` only switched on `ContentString` and `ContentFormat`. Now also handles `ContentEmbed` (and `ContentType` for rare YText-inside-YText cases), emitting `Insert{embedValue}`, `Delete{1}`, and contributing 1 to retains — matching Yjs's UTF-16 length convention for embeds.

- **README refresh** — the version reference was five months stale (v1.7.0 → v1.14.0). Extended the post-v1.0 hardening section to cover the v1.8.x security work, the v1.10.0 lib0 wire-format parity, the v1.9.0–v1.14.0 cross-reference audit, and the smaller features in between (`sync.WithErrorHandler`, `Awareness.Heartbeat`, `YText.InsertEmbed`). Added a callout for the [`gaps` label](https://github.com/reearth/ygo/issues?label=gaps) tracking the audit.

## Install

```
go get github.com/reearth/ygo@v1.14.0
```

See [CHANGELOG.md](https://github.com/reearth/ygo/blob/main/CHANGELOG.md) for full details.
