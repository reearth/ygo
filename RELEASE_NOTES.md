## v1.31.1

A CRDT correctness patch. No API changes.

### Fixed

- **YMap key silently lost on `ApplyUpdateV1` ([#149](https://github.com/reearth/ygo/issues/149)).**
  When a map-keyed item's origin was authored by a higher-clientID peer, the
  item decoded before its origin's client group within the same update and took
  the V1 deferred-parent retry path, which resolved the parent but did not
  inherit `ParentSub`. The item integrated keyless and vanished from the map —
  a single document's own full-state encode could fail to round-trip
  (`EncodeStateAsUpdateV1` → `ApplyUpdateV1` dropping a live key), and two peers
  applying the same updates in different orders could diverge. The V1
  within-update resolver now inherits `ParentSub`, matching the sibling
  resolution sites and the V2 decoder.

This bug was found by ygo's new randomized convergence fuzzer (#70) on its first
run and confirmed by an independent byte-exact isolation. It affects all V1
decode paths since the Yjs wire-format work and is present in v1.31.0; upgrading
is recommended for anyone using `YMap` across concurrent peers.

## Install

```
go get github.com/reearth/ygo@v1.31.1
```

See [CHANGELOG.md](https://github.com/reearth/ygo/blob/main/CHANGELOG.md) for full details.
