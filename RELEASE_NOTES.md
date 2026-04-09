## Fixes

- **Room-splitting race**: concurrent join + disconnect could fork a document into two rooms for the same name
- **Awareness validation bypass**: invalid awareness payloads were broadcast to all peers even after server-side rejection
- **Silent persistence failures**: `LoadDoc`/`StoreUpdate` errors were swallowed; writes are now serialised per room with error logging, and `Shutdown` waits for in-flight persistence

## Install

```
go get github.com/reearth/ygo@v1.0.1
```

See [CHANGELOG.md](https://github.com/reearth/ygo/blob/main/CHANGELOG.md) for full details.
