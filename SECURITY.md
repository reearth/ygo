# Security Policy

## Supported Versions

Only the latest minor release receives security fixes.

| Version | Supported |
|---------|-----------|
| latest  | ✅        |
| older   | ❌        |

## Reporting a Vulnerability

**Please do not open a public GitHub issue for security vulnerabilities.**

Report vulnerabilities by emailing **security@reearth.io**. Include:

- A description of the vulnerability and its potential impact
- Steps to reproduce or a minimal proof-of-concept
- Any suggested mitigations, if known

You can expect an acknowledgement within **48 hours** and a resolution timeline within **90 days** (coordinated disclosure). We will credit reporters in the release notes unless you prefer to remain anonymous.

## Threat Model

- **Wire format**: ygo validates all incoming binary updates (V1 and V2) but does **not** authenticate them. Applying an update from an untrusted peer is safe in the sense that it cannot crash the process or exhaust unbounded memory, but it can modify the document. Authentication and authorisation are the responsibility of the transport layer (e.g. the WebSocket `AuthFunc` hook).
- **Cross-Site WebSocket Hijacking (CSWSH)**: setting `Server.AllowedOrigins` to `"*"` disables same-origin protection. A malicious page visited by the user can then open a WebSocket to this server and exercise the user's session if authentication is carried by a cookie. Mitigations: (a) use a specific allow-list of origins rather than `"*"`; (b) use `AuthFunc` with tokens carried explicitly (subprotocol or query parameter), not cookies; (c) deploy behind a reverse proxy that enforces origin checks at the edge. See #49.
- **Denial of service**: the following resource limits are enforced on untrusted input:
  - Binary update: max 1 048 576 items per update (`maxV2Items`); max `math.MaxInt32` length per field
  - HTTP POST body and WebSocket frame: max 64 MiB (`maxUpdateBytes` / `maxWSMessageBytes`)
  - Awareness update: max 100 000 client entries (`maxAwarenessClients`); max 1 MiB per client state (`maxAwarenessStateBytes`); max 1 000 top-level keys per decoded state (`maxStateKeys`, added v1.8.1 #48 vector A — prevents a small payload like `{"k1":1,...,"k65535":1}` from materialising into a multi-MB map). Per-Awareness cumulative wire-state cap configurable via `awareness.Awareness.SetMaxBytes` (default unlimited); `provider/websocket.Server.MaxAwarenessBytesPerRoom` forwards this to each room (#48 vector B)
  - `ReadAny` recursion: max 100 levels deep (`maxAnyDepth`)
  - Per-document pending-items queue: max 100 000 parked items by default (`defaultMaxPendingItems`; configurable via `crdt.WithMaxPendingItems` or `Server.MaxPendingItems`). Items whose `Origin`/`OriginRight` references a clock not yet integrated are parked in this queue and retried when the dependency arrives — the cap prevents a crafted update full of far-future-clock items from growing the queue without bound. Updates that would exceed the cap return `ErrInvalidUpdate`. (Added in v1.8.0; see #46.)
  - WebSocket idle connections: peers that complete the handshake but don't send a first message are disconnected after `Server.HandshakeTimeout` (default 30s). Without this, an attacker could exhaust goroutines and per-connection buffers via slow-loris-style idle connections. (Added in v1.8.0; see #47.)
### ClientID semantics

`crdt.ClientID` is a 32-bit value generated via `crypto/rand` and used to
distinguish authoring peers during YATA conflict resolution. It is **not** an
authentication token — the protocol does not validate that incoming updates
match a peer's declared ClientID. Authentication and authorization are the
embedder's responsibility (via `Server.AuthFunc`, request-level checks, etc.).

- **Known limitations**:
  - No built-in cryptographic signatures or MACs on updates — add these at the transport layer if needed.
  - Subdocuments (`ContentDoc`) are structurally present in the wire format but not exposed as a user-facing API in this release.
  - `UndoManager` cannot restore items whose content was freed by `RunGC`. Either disable GC (`WithGC(false)`) or avoid calling `RunGC` while an UndoManager is active.
