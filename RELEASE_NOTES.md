## Yjs Compatibility Fixes

- **V1 GC struct & skip struct decoding**: Updates from Yjs clients containing garbage-collected (tag 0) or skipped (tag 10) items no longer misalign the decoder
- **Cross-client parent resolution**: Items referencing origins from a later client group are now retried after all groups are decoded, fixing sync failures in multi-client documents
- **Subdocument GUID preserved**: `ContentDoc` round-trips now retain the subdocument identity instead of discarding it
- **Room names with spaces/Unicode**: Relaxed validation to match y-websocket's permissive behavior
- **y-websocket auth message**: Type 2 messages are now silently handled instead of being dropped as unknown

## Install

```
go get github.com/reearth/ygo@v1.0.2
```

See [CHANGELOG.md](https://github.com/reearth/ygo/blob/main/CHANGELOG.md) for full details.
