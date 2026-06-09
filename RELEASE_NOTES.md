## What's new

Durable storage and a runnable server. This release adds a pure-Go SQLite
persistence backend and a ready-to-run WebSocket collaboration server binary, so
you can stand up a restart-surviving, optionally horizontally-scaled Yjs server
without writing any glue code.

### Added — `persistence/sqlite` (pure-Go durable backend)

A `VersionedPersistence` implementation backed by SQLite through
`modernc.org/sqlite` — **CGo-free**, so it cross-compiles and builds with
`CGO_ENABLED=0` like the rest of ygo. It runs in WAL mode, keeps full versioned
history, and prunes old versions with a crash-safe two-phase delete. It passes
the shared `RunConformance` suite, so it behaves identically to the in-memory
reference store. Wiring it in is a one-liner:

```go
store, err := sqlite.Open("data.db")
```

### Added — `cmd/ygo-server` (ready-to-run server)

A single-binary Yjs-compatible WebSocket collaboration server built on
`provider/websocket`. It exposes flags for the listen address, allowed origins,
connection / per-room / room-count limits, and max message size; optional Redis
cluster relay via `-redis` (multiple instances share one logical document per
room); and SQLite persistence via `-store` (rooms survive restarts). It shuts
down gracefully on SIGINT/SIGTERM — draining in-flight work, detaching the relay,
and closing the store. Run it straight from the module:

```
go run github.com/reearth/ygo/cmd/ygo-server -addr :1234 -store data.db
```

## Install

```
go get github.com/reearth/ygo@v1.23.0
```

See [CHANGELOG.md](https://github.com/reearth/ygo/blob/main/CHANGELOG.md) for full details.
