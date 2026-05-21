## What's new

Fifth in the [`gaps` label](https://github.com/reearth/ygo/issues?label=gaps) series. The YText overhaul — closes #76 entirely and partially addresses #71 (the structural rewrite of `Insert` to support attribute inheritance and negation is scoped to a follow-up PR).

- **`YText.InsertEmbed(txn, index, embed, attrs)`** (#76). Public API for inserting embedded objects — images, formulas, videos, any inline non-text payload — into the rich-text stream. Each embed counts as one UTF-16 code unit in document length, matching Yjs JS. Optional `attrs` argument scopes attributes to the embed alone via opening + closing format markers, so they don't bleed into subsequent content. `ContentEmbed` was already on the wire format (tag 5); this finally exposes the API to use it.

- **`ToDelta` now emits embeds correctly**. Pre-fix, `ContentEmbed` items were silently dropped from `ToDelta` output. Now they appear as their own Delta entries with the embed value carried in `Insert`.

- **`YText.Format` cleans up overlapping same-key markers** (#71 vector A1). Repeated `Format` toggles (bold on/off, applied multiple times) previously accumulated dead opening/closing marker pairs in the linked list without bound. `Format` now walks the target range and tombstones pre-existing same-key markers before inserting new ones. Matches Yjs JS `YText.formatText`.

- **`YText.Delete` cleans up dangling format markers** (#71 vector A4). After deleting a range, `ContentFormat` markers whose effect zone now contains no live content are tombstoned. Two cases handled: openers with empty scope (until the next same-key marker), and closers without a matching preceding opener. Matches Yjs JS `cleanupFormattingGap`. There's a small per-call perf overhead (~0.8µs added latency in a worst-case single-char-delete benchmark); correctness takes priority and the absolute cost is well below user-perceivable thresholds for realistic edit patterns.

- **Incidental fix:** `computeDelta` (observer event computation) was producing a phantom attribute diff on the retain preceding any format marker tombstoned during the same transaction. The diff is now computed against the correct pre-transaction state. Surfaced by the A1 work; fixed alongside it.

## Install

```
go get github.com/reearth/ygo@v1.12.0
```

See [CHANGELOG.md](https://github.com/reearth/ygo/blob/main/CHANGELOG.md) for full details.
