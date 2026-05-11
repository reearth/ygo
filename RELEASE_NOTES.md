## What's new

Three small improvements bundled as a security + observability mini-release, plus a documentation polish from an external contributor.

- **`Doc.PendingStats()` (#24)** — snapshot of the pending-structs machinery. Operators can now monitor pending-queue depth to detect adversarial peers, slow convergence, or persistent gaps from misbehaving clients.
- **Hard-cap connections in `provider/websocket` (#23)** — `MaxConnections` and `MaxPeersPerRoom` now enforced via `semaphore.Weighted` rather than optimistic atomic counters. Closes the race window where bursts of concurrent connections could briefly exceed the configured cap.
- **`crypto/rand` for ClientID generation (#28)** — replaces `math/rand` at both ClientID generation sites. New public helper `crdt.NewClientID()`. `SECURITY.md` clarifies that ClientIDs are collision-avoidance, not authentication.
- **godoc and invariant comment polish (#30)** — contributed by @Jah-yee. Cleaner docs for the `Origin` tag convention, `InjectInfo` struct, `YArray.prepareFire`, and `applyToPartial`'s contiguity invariant.

## Install

```
go get github.com/reearth/ygo@v1.5.0
```

See [CHANGELOG.md](https://github.com/reearth/ygo/blob/main/CHANGELOG.md) for full details.
