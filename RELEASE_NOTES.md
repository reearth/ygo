## What's new

Server-side awareness hardening. This release closes a memory-exhaustion DoS in
the awareness subsystem and reclaims stale presence, with new knobs on the
websocket server and the `ygo-server` binary. No breaking API changes.

### Security

- **Bounded awareness memory growth.** A peer could exhaust a room's memory by
  sending awareness updates that invent unbounded client IDs — including
  null-state entries, which bypassed the existing per-room byte cap. A new
  distinct-entry cap stops this: once a room is at capacity, previously-unseen
  client IDs are dropped while already-tracked clients keep updating.
  - `awareness.Awareness.SetMaxClients(n)` — library-level cap.
  - `websocket.Server.MaxAwarenessClientsPerRoom` — per-room cap.

### Added

- **Server-side awareness expiry.** `websocket.Server.AwarenessExpiry` (when
  > 0) reclaims a remote client's presence after it goes idle, clearing "ghost"
  presence left by peers that died silently (mobile sleep, NAT timeout) without
  a clean disconnect. The per-room sweep goroutine is stopped on room eviction.
- **`ygo-server` flags** `-max-awareness-clients` (default 10000) and
  `-awareness-expiry` (default 30s) enable both protections out of the box.

### Compatibility

No breaking API changes — the new fields and methods are additive and default
to off at the library level (0 = unlimited / disabled), preserving existing
behavior. The `ygo-server` binary now enables both protections by default;
pass `-max-awareness-clients 0` / `-awareness-expiry 0` to restore the prior
unbounded behavior.

## Install

```
go get github.com/reearth/ygo@v1.25.0
```

See [CHANGELOG.md](https://github.com/reearth/ygo/blob/main/CHANGELOG.md) for full details.
