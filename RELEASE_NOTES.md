## What's new

Correctness hardening. This patch fixes five CRDT convergence/interop defects
surfaced by an internal architecture review. Each is reproduced by a new
regression test and verified against `yjs@13.6.30`; there are no public API changes.

### Fixed

- **Concurrent `YMap` / XML-attribute writes no longer lose a key depending on
  apply order.** Last-writer-wins previously decided the winner from the
  immediate linked-list right neighbour, so an unrelated-key write landing
  between two same-key writes could make an item falsely consider itself the
  current value. Receivers diverged on the key, and a subsequent full-state
  cross-sync deleted it outright. The winner is now the rightmost same-key item
  in YATA order — which is order-independent — so all receivers converge.

- **Out-of-order updates with a missing right origin now park instead of
  integrating at the wrong position.** An item whose `rightOrigin` referenced a
  not-yet-integrated client was placed incorrectly (permanent text divergence)
  because the future-clock dependency check was skipped for root types. It now
  defers and retries when the missing client arrives, matching Yjs `getMissing`.

- **A document no longer fails to decode its own full-state encode.** When a
  lower-clientID peer wrote into a higher-clientID peer's nested type (e.g. an
  XML element attribute), the child's parent-by-ID reference decoded before its
  parent and hard-failed `ApplyUpdate` — breaking persistence reload and the
  initial sync handshake for such documents. The reference is now deferred and
  resolved once the container integrates, mirroring Yjs `pendingStructs`.

- **`RelativePosition` and `Snapshot` are now wire-compatible with Yjs.** The
  `RelativePosition` encoding used the wrong type tags, so shared cursors
  exchanged with a JS peer mis-decoded or crashed lib0; `Snapshot` wrote its
  blocks in the wrong order with extra length prefixes. Both now match
  `Y.encodeRelativePosition` / `Y.encodeSnapshot` and are verified by encoding
  in Go and decoding/resolving in `yjs@13.6.30`.

### Compatibility

No API changes. The internal `RelativePosition` and `Snapshot` byte formats
changed to become Yjs-compatible — if you persisted those bytes from v1.23.0
(uncommon; document updates are unaffected), re-capture them. Document update
and state-vector formats are unchanged. Drop-in upgrade from v1.23.0.

## Install

```
go get github.com/reearth/ygo@v1.23.1
```

See [CHANGELOG.md](https://github.com/reearth/ygo/blob/main/CHANGELOG.md) for full details.
