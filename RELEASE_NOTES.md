## What's new

- **`crdt` internal refactor + architecture note (#22)** — extracted the post-insertion housekeeping in `Item.integrate` into `postIntegrateHousekeeping`. The hot conflict-scan and link phases stay inline because their live state spans both — Yjs JS and yrs (Rust) keep their equivalents monolithic for the same reason. The `Item.integrate` godoc now documents why, so future maintainers don't re-discover this constraint independently. Pure refactor — zero behavior change, `benchstat` over n=10 confirms no perf regression.

## Install

```
go get github.com/reearth/ygo@v1.6.2
```

See [CHANGELOG.md](https://github.com/reearth/ygo/blob/main/CHANGELOG.md) for full details.
