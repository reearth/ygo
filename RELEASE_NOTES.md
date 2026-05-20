## What's new

Third in the [`gaps` label](https://github.com/reearth/ygo/issues?label=gaps) series from the cross-reference audit against Yjs JS and yrs. Closes the awareness self-state protection and clock-semantics gaps in the `awareness/` package.

- **Remote peers can no longer wipe local state** (#73 vector C1, HIGH). A `(self.clientID, ?, "null")` entry from a remote peer previously cleared the local awareness state — any peer could effectively deauthenticate any other by broadcasting a null for their clientID. `ApplyUpdate` now detects this case, bumps the local clock past the incoming value, and re-emits the current local state so peers learn the new clock.

- **Equal-clock null removals are honored for active clients** (#73 vector C2, HIGH). The strict `<= current.Clock` gate dropped the canonical "I'm going offline at the clock you already know" disconnect message, leaving phantom presence indicators. The gate now uses `<` for stale-drop and additionally accepts equal-clock null when the client is active. Stale and equal-clock-non-null entries are still dropped (no new info).

- **Local clock follows remote echoes** (#73 vector C3, MEDIUM). `SetLocalState` now reconciles `a.clock` against `states[a.clientID].Clock` before incrementing, so a remote echo of our clientID (e.g. another tab) doesn't cause subsequent updates to emit a clock peers have already seen.

- **`RemoveExpired` no longer evicts the local client** (#73 vector C4, MEDIUM). The expiry sweep now skips our own clientID — peers can't reliably tell whether we've gone silent, so it's our job to refresh presence via `SetLocalState` or the new `Heartbeat`.

- **New `Awareness.Heartbeat()` method** (#73 vector C5). Re-emits the local state with an incremented clock so peers see us as still alive even when state hasn't changed. Pairs with their `StartAutoExpiry` to keep us visible in a quiet room. No observers fired (state didn't change, only the clock).

## Install

```
go get github.com/reearth/ygo@v1.11.0
```

See [CHANGELOG.md](https://github.com/reearth/ygo/blob/main/CHANGELOG.md) for full details.
