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
- **Denial of service**: the following resource limits are enforced on untrusted input:
  - Binary update: max 1 048 576 items per update (`maxV2Items`); max `math.MaxInt32` length per field
  - HTTP POST body and WebSocket frame: max 64 MiB (`maxUpdateBytes` / `maxWSMessageBytes`)
  - Awareness update: max 100 000 client entries (`maxAwarenessClients`); max 1 MiB per client state (`maxAwarenessStateBytes`)
  - `ReadAny` recursion: max 100 levels deep (`maxAnyDepth`)
- **Known limitations**:
  - No built-in cryptographic signatures or MACs on updates — add these at the transport layer if needed.
  - Subdocuments (`ContentDoc`) are structurally present in the wire format but not exposed as a user-facing API in this release.
  - `UndoManager` cannot restore items whose content was freed by `RunGC`. Either disable GC (`WithGC(false)`) or avoid calling `RunGC` while an UndoManager is active.
