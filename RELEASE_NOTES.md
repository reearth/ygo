## What's new

Provider security hardening. The HTTP provider gains the auth hook, room-name
validation, and configurable body-size cap the WebSocket provider already had, and
the WebSocket provider gains optional per-peer message rate limiting. No breaking
changes — every new field defaults to today's behaviour.

### Added

- **`http.Server.AuthFunc`** — reject unauthenticated requests with 401 before any
  document is read or mutated (parity with the WebSocket provider). (#50)
- **`http.Server.MaxUpdateBytes`** — configurable POST-body cap; oversize bodies get
  413. Zero keeps the 64 MiB default. (#50)
- **`websocket.Server.MessageRateLimit` / `MessageRateBurst`** — optional per-peer
  inbound rate limit. A peer that floods past it is disconnected — not silently
  dropped, which would diverge it. Zero is unlimited. (#51)

### Changed

- **The HTTP provider now validates room names** (empty / oversized / `.` / `..` /
  control chars → 400), using the same rule as the WebSocket provider, now shared
  via the `internal/roomname` package. (#50)

## Install

```
go get github.com/reearth/ygo@v1.29.0
```

See [CHANGELOG.md](https://github.com/reearth/ygo/blob/main/CHANGELOG.md) for full details.
