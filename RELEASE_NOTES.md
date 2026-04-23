## What's new

- **Cross-update Origin resolution on out-of-order delivery (#11).** Peers that received delta updates out of dependency order used to silently orphan items whose `Origin` references hadn't yet integrated — producing permanent convergence gaps. Updates now park unresolved items in a doc-level pending queue and retry them automatically on each subsequent apply. Same-client clock gaps and delete-set entries targeting not-yet-integrated items follow the same path.
- **Convergence parity with Yjs JS and yrs.** The pending-structs machinery matches the upstream implementations semantically. State vector still reports integrated-only clocks, so remote peers continue to detect gaps and re-send automatically.

## Install

```
go get github.com/reearth/ygo@v1.2.0
```

See [CHANGELOG.md](https://github.com/reearth/ygo/blob/main/CHANGELOG.md) for full details.
