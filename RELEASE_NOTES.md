## Fix: GC'd YMap origins no longer crash the server

Repeated `YMap.Set` on the same key (e.g., typing into a title field) creates GC'd items in the Yjs wire format. Previously, ygo rejected the entire update with "N items with unresolvable parents", breaking persistence and real-time sync for the whole room. Now handled gracefully — matching the y-websocket JS server behavior.

- **Decoder**: orphaned items from GC'd origins are stored without error; multi-client documents resolve parents from other clients' items
- **Encoder**: GC'd origins fall back to explicit parent info; orphaned items re-encode as GC structs

**Note**: Single-client documents where ALL YMap predecessors are GC'd will lose the YMap value (a [known Yjs wire-format limitation](https://github.com/yjs/yjs/issues) shared by y-websocket). All other shared types (YText, YArray) are unaffected. In normal real-time sync (incremental updates), data is fully preserved.

## Install

```
go get github.com/reearth/ygo@v1.0.3
```

See [CHANGELOG.md](https://github.com/reearth/ygo/blob/main/CHANGELOG.md) for full details.
