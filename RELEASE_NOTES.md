## v1.31.2

A CRDT convergence patch. No API changes. Both fixes were found by ygo's new
randomized convergence fuzzer (#70) and confirmed by independent harness-free
isolation. Upgrading is recommended for anyone syncing via the V2 wire format or
using `MergeUpdates`/`DiffUpdate`.

### Fixed

- **`ApplyUpdateV2` mis-positioned an item whose `OriginRight` was a
  not-yet-integrated clock ([#151](https://github.com/reearth/ygo/issues/151)).**
  V2 decodes client groups in descending client order, so a higher-clientID item
  could integrate before its lower-clientID right neighbour existed and land at
  the wrong position — `ApplyUpdateV2` diverging from `ApplyUpdateV1` for an
  identical logical update. `applyV2Txn` now defers such an item, matching the V1
  apply path (the C-2 `rightOrigin`-parking fix, #65/#68, that had never been
  ported to V2).

- **`MergeUpdates`/`DiffUpdate` stripped a real item's origin when its parent was
  outside the input set — both V1 and V2
  ([#152](https://github.com/reearth/ygo/issues/152)).** The re-encoders cleared
  `Origin`/`OriginRight` whenever the referenced neighbour's `Parent` was nil,
  but that is also true when the neighbour's anchor lives in the receiver's base
  rather than in the update being merged. Stripping detached the item, so the
  receiver integrated it at the type head (reorder/loss). The encoders now strip
  only for genuine GC orphans; the #125 GC-origin handling is preserved.

## Install

```
go get github.com/reearth/ygo@v1.31.2
```

See [CHANGELOG.md](https://github.com/reearth/ygo/blob/main/CHANGELOG.md) for full details.
