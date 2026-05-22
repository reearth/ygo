## What's new

Closes #71 entirely. This is the deferred companion to v1.12.0's partial YText overhaul: `YText.Insert` now computes `currentAttributes` at the cursor and uses a diff-based approach to emit opening and negating markers, matching Yjs JS's `insertText` exactly.

**Two behavior changes** vs v1.12.0:

- **Inheritance (A2)**: typing with `nil`/empty attrs at the end of a formatted span now properly tracks `currentAttributes` from the linked-list state. Functionally users see the same continuation-of-bold behavior as before, but the underlying mechanism is now correct (and `currentAttributes` is available for future features like relative cursors).

- **No more right-bleed (A3)**: `Insert` with explicit attrs now emits negating closing markers after the text. Without them, formatting bled rightward through subsequent retained text — visible to a peer doing `ToDelta` after sync as runs of incorrectly-formatted plain text.

The wire format now matches Yjs JS byte-for-byte for `Insert`-with-attrs scenarios. Cross-peer convergence tests confirm a fresh peer applying these updates produces the same `ToDelta` as the originating peer.

Plus an incidental bug fix in the closer's `Origin` reference (was being set to the first clock of the wrapped text item instead of the last, which placed closers mid-text after YATA integration on a fresh peer).

## Install

```
go get github.com/reearth/ygo@v1.13.0
```

See [CHANGELOG.md](https://github.com/reearth/ygo/blob/main/CHANGELOG.md) for full details.
