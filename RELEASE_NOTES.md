## What's new

Deployment-security and wire-handling hardening. This release makes the
`ygo-server` binary secure by default, lets large documents sync without hitting
an artificial field-size ceiling, surfaces malformed frames in the logs, and
aligns `RelativePosition` resolution with the Yjs reference. No breaking API
changes.

### Security

- **`ygo-server` is secure by default.** The server has no built-in
  authentication, so it now binds `127.0.0.1:1234` (loopback only) by default
  instead of all interfaces. A non-loopback bind still works but logs a loud
  `SECURITY` warning — any host that can reach the port could otherwise read and
  modify every document. Front a public deployment with an authenticating
  reverse proxy.

### Fixed

- **Large single fields no longer fail to sync below the message cap.** A single
  wire field was capped at a fixed 16 MiB even though every message layer allows
  64 MiB by default, so a document with one >16 MiB text node or binary embed
  was silently rejected inside an otherwise-valid message. A field is now bounded
  by the size of the message that carries it (policed by the provider's own
  `MaxMessageBytes`), removing the silent failure without weakening the
  out-of-memory guard.
- **`RelativePosition` resolves to the end of a type, matching Yjs.** A position
  anchored to a root type (the form `CreateRelativePositionFromIndex` produces
  for an end-of-type cursor) now resolves to the end of the type for
  `Assoc >= 0` (and to the start for `Assoc < 0`), matching `toAbsolutePosition`
  in Yjs. Previously it always resolved to index 0, snapping an end-of-document
  cursor back to the start.

### Added

- **Malformed inbound frames are logged.** The websocket server now logs each
  dropped unreadable / unappliable message at `Debug` level (with the room and
  error) so an operator can diagnose why a peer's edits never land. `Debug`, not
  `Warn`, because the rate is attacker-controlled.

### Compatibility

No breaking API changes. The `ygo-server` binary's default `-addr` changes from
`:1234` to `127.0.0.1:1234`; a deployment that relied on the previous
all-interfaces default must now pass `-addr :1234` (or a specific address)
explicitly, and will then see the new security warning.

## Install

```
go get github.com/reearth/ygo@v1.26.0
```

See [CHANGELOG.md](https://github.com/reearth/ygo/blob/main/CHANGELOG.md) for full details.
