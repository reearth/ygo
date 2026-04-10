## Fixes

- **Nil panic on reconnect**: Server no longer panics when a reconnecting client sends state containing GC'd YMap replacement items. The `delete()` path now guards against orphaned items with no parent.
- **Cross-browser sync with emoji**: ContentString offset encoding now correctly uses UTF-16 code units instead of Unicode code points. Fixes corrupt binary output when strings contain emoji or supplementary characters, which caused Yjs decode failures across different browser engines (V8 vs JavaScriptCore).

## Install

```
go get github.com/reearth/ygo@v1.0.4
```

See [CHANGELOG.md](https://github.com/reearth/ygo/blob/main/CHANGELOG.md) for full details.
