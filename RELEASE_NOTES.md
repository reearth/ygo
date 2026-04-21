## What's new

- **`Doc.Transact` is now panic-safe (#9).** Previously, a panic inside the transaction callback left the document's write lock held forever, wedging every subsequent operation on the doc — including `websocket.Server.Apply`'s cleanup path. Transact now releases the lock on every exit path.
- **Documented panic semantics.** On panic, observers fire with the partial state that was committed before the panic (matching Yjs JS and `yrs`), then the original panic is re-raised. Rollback is not supported; callers needing atomicity should recover and reconcile.
- **`websocket.Server.Apply` no longer wedges rooms on panic.** The "fn MUST NOT panic" caveat is softened — panics now broadcast partial state to peers and trigger persistence, just like any other mutation.

## Install

```
go get github.com/reearth/ygo@v1.1.1
```

See [CHANGELOG.md](https://github.com/reearth/ygo/blob/main/CHANGELOG.md) for full details.
