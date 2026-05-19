## What's new

First in a series of fixes from the cross-reference audit against Yjs JS and yrs (see the [`gaps` label](https://github.com/reearth/ygo/issues?label=gaps)).

- **`sync.WithErrorHandler` option for `ApplySyncMessage`** (#79). Previously, a single malformed update inside a sync message would propagate the error out of `ApplySyncMessage` and force any caller using it as the dispatcher in a transport read loop to tear down the connection. y-protocols' `readSyncMessage` wraps `applyUpdate` in try/catch and routes failures to an optional `errorHandler` callback while keeping the loop alive. ygo now matches:

  ```go
  sync.ApplySyncMessage(doc, msg, origin,
      sync.WithErrorHandler(func(err error) { log.Warn(err) }))
  ```

  When set, the dispatcher routes `ApplyUpdateV1` errors to the handler and returns `(nil, nil)` — the read loop continues processing subsequent messages. Without the option, the existing return-the-error behavior is preserved (back-compat for every existing caller). Header-level decode errors (truncated frames, unknown message types, malformed state vectors in Step1) are still returned regardless — those signal transport-level corruption and should disconnect.

  9 tests cover the contract: single-call semantics, read-loop continuation across good→bad→good sequences, multi-error reporting, header/Step1 error routing, panic propagation, and nil-handler defensive case.

- **Benchmark CI scoped to hot-path PRs**. The benchmark job no longer runs on every PR — only when the diff touches `crdt/**`, `encoding/**`, `benchmarks/**`, or the workflow file. Adds a nightly cron schedule (`06:00 UTC`) that records absolute numbers on `main` as a 30-day artifact for trend tracking, plus a `workflow_dispatch` trigger for on-demand runs. Reduces typical PR CI time by ~20 minutes.

- **`CONTRIBUTING.md`** documents the GetText-inside-Transact deadlock pitfall for contributors writing tests — the most common cause of hanging test runs.

## Install

```
go get github.com/reearth/ygo@v1.9.0
```

See [CHANGELOG.md](https://github.com/reearth/ygo/blob/main/CHANGELOG.md) for full details.
