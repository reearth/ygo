## What's new

Native mobile bindings. This release adds a new `mobile/` package: a
gomobile-bindable façade over ygo's `crdt` and `awareness` packages, so you can
embed ygo directly in iOS and Android apps via `gomobile bind` — no JavaScript
runtime and no CGo. v1 scope is **sync + render**: apply peer updates, encode
state/diffs, and read the current document and presence; on-device editing
(mutators) is a planned follow-up.

### Added — `mobile/` (gomobile bindings for iOS/Android)

The package exposes two bound types with a **gomobile-safe surface** —
gomobile bind only supports a restricted set of types across the language
boundary, so every method uses only `string`, `int64`, `bool`, `[]byte`,
`error`, and the bound pointers `*Doc` / `*Awareness` (no unsigned ints, maps,
non-byte slices, `any`, variadics, or callbacks; the package translates at the
boundary):

- **`Doc`** — `NewDoc` / `NewDocWithClientID`, `ClientID`, `ApplyUpdate`,
  `EncodeStateAsUpdate`, `EncodeStateVector`, `EncodeDiff` (incremental sync from
  a remote state vector), `GetText`, `GetTextJSON`, `GetMapJSON`, `GetArrayJSON`,
  and `Close`.
- **`Awareness`** — `NewAwareness`, `ClientID`, `SetLocalState`,
  `ClearLocalState`, `LocalStateJSON`, `StatesJSON`, `EncodeAll`, `ApplyUpdate`,
  and `Close`.

Build the frameworks with the standard gomobile tooling (a build-time tool, not
a dependency — `go.mod` is unchanged):

```
gomobile bind -target=ios     ./mobile   # → Mobile.xcframework
gomobile bind -target=android -androidapi 21 ./mobile   # → mobile.aar
```

All methods are synchronous and blocking and copy `[]byte` across the boundary,
so call `ApplyUpdate` / `Encode*` off the UI thread (Kotlin `Dispatchers.IO` /
a Swift background queue) and prefer incremental `EncodeDiff` over full-state
encodes on the hot path. Call `Close()` when done (`ViewModel.onCleared` /
Swift `deinit`) rather than relying on cross-binding finalization.

The package is pure-Go / CGo-free, so it builds with `CGO_ENABLED=0` like the
rest of ygo (guarded in CI). Note one known caveat: `GetTextJSON` and
`StatesJSON` currently return ygo's internal struct shapes (Go field names),
not idiomatic Yjs JSON; stable/idiomatic shapes are a planned follow-up, so
don't hard-code against them long-term. See [`mobile/README.md`](https://github.com/reearth/ygo/blob/main/mobile/README.md)
for the full integration guide (threading, lifecycle, binary size / ABI,
error handling, change notifications, and Kotlin/Swift snippets).

## Install

```
go get github.com/reearth/ygo@v1.24.0
```

See [CHANGELOG.md](https://github.com/reearth/ygo/blob/main/CHANGELOG.md) for full details.
