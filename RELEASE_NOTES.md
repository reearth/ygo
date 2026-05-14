## What's new

- **`encoding.Encoder.WriteVarIntE` (#26)** — error-returning sibling for the panicking `WriteVarInt`. Returns `ErrVarIntOutOfRange` when input exceeds the lib0 55-bit limit instead of panicking. Existing `WriteVarInt` unchanged. Pattern matches v1.3.0's `TransactE`.
- **`provider/websocket.PersistenceAdapterContext` (#35)** — optional extension interface. Adapters that implement it receive a context cancelled when `Server.Shutdown` begins, letting them abort in-flight network/DB writes rather than blocking shutdown indefinitely. Existing adapters work unchanged.

Both changes are strictly additive — no breaking API changes.

## Install

```
go get github.com/reearth/ygo@v1.7.0
```

See [CHANGELOG.md](https://github.com/reearth/ygo/blob/main/CHANGELOG.md) for full details.
