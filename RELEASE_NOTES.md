## What's new

Four items bundled — two from an external contributor's lib0 wire-compat fix, two from a security audit, plus a documentation hardening.

- **Lib0 `Any` `BigInt` tag support + float byte-order fix** (#45, contributed by @zombiek731). Adds `encoding.BigInt` and `WriteAny`/`ReadAny` support for lib0's `Any` tag 122. Fixes a long-standing wire-compat bug: ygo encoded `float32`/`float64` `Any` values in little-endian, but lib0 and yrs use big-endian. The README has always claimed Yjs binary compatibility; this finally honors it for documents containing float or BigInt values. See the compatibility note in CHANGELOG for the persisted-update implication.
- **Cap on the pending-items queue** (#46). `StructStore.pending.items` was previously unbounded. A malicious peer could craft a single max-size update full of items referencing far-future clocks and OOM the server. Now capped at 100,000 by default. Configurable via `crdt.WithMaxPendingItems` and `Server.MaxPendingItems` on both providers.
- **WebSocket initial read deadline** (#47). The read loop had no deadline before the first message; with `MaxConnections == 0` (default), an attacker could complete handshakes and sit idle, exhausting goroutines. New `Server.HandshakeTimeout` (default 30s) closes idle connections; deadline is cleared after the first successful read.
- **CSWSH warning on `AllowedOrigins`** (#49). Documentation update: setting `AllowedOrigins` to `"*"` disables same-origin protection and enables Cross-Site WebSocket Hijacking. The godoc and `SECURITY.md` now make this explicit with mitigation guidance.

## Install

```
go get github.com/reearth/ygo@v1.8.0
```

See [CHANGELOG.md](https://github.com/reearth/ygo/blob/main/CHANGELOG.md) for full details.
