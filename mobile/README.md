# ygo/mobile — native iOS/Android bindings

`mobile/` is a [gomobile](https://pkg.go.dev/golang.org/x/mobile)-bindable façade
over ygo's `crdt` and `awareness` packages. It lets you embed ygo's Yjs-compatible
CRDT engine directly in native iOS and Android apps via `gomobile bind` — no
JavaScript runtime and no CGo. **v1 scope is sync + render**: apply peer updates,
encode state and incremental diffs, and read the current document and presence.
On-device editing (mutators) is a planned follow-up.

## gomobile-safe type constraint

`gomobile bind` only supports a restricted set of types across the language
boundary, so **every exported function and method in this package uses only**:
`string`, `int64`, `bool`, `[]byte`, `error`, and the bound pointers `*Doc` and
`*Awareness`. It never exposes unsigned ints, maps, non-byte slices, `any`,
variadics, or callbacks. (The underlying `crdt` / `awareness` packages use
`uint64` client IDs, maps, and `[]uint64` internally; this package translates at
the boundary — e.g. client IDs are `int64` and constrained to `[0, 2^53]`, the
JS safe-integer range, and structured values cross the boundary as JSON `[]byte`.)

The bound surface is:

- **`Doc`** — `NewDoc`, `NewDocWithClientID(int64)`, `ClientID`, `ApplyUpdate`,
  `EncodeStateAsUpdate`, `EncodeStateVector`, `EncodeDiff`, `GetText`,
  `GetTextJSON`, `GetMapJSON`, `GetArrayJSON`, `Close`.
- **`Awareness`** — `NewAwareness(int64)`, `ClientID`, `SetLocalState`,
  `ClearLocalState`, `LocalStateJSON`, `StatesJSON`, `EncodeAll`, `ApplyUpdate`,
  `Close`.

## Building the bindings

`gomobile bind` is a **build-time tool**, not a runtime dependency — it is not in
`go.mod`, and you install it separately. Generate the native artifacts with:

```sh
# iOS → Mobile.xcframework
gomobile bind -target=ios ./mobile

# Android → mobile.aar (minimum API level 21)
gomobile bind -target=android -androidapi 21 ./mobile
```

### Pinned toolchain matrix

| Component                | Version / setting                          |
| ------------------------ | ------------------------------------------ |
| Go                       | 1.23                                       |
| `golang.org/x/mobile`    | a recent release (build-time only — not in `go.mod`) |
| Android NDK              | a recent installed NDK                     |
| Android min API level    | `-androidapi 21`                           |

`gomobile bind` is **verified manually** — it needs Xcode (iOS) and the Android
NDK, which are not available in CI. **CI only guards the CGo-free build**
(`CGO_ENABLED=0 go build ./mobile/...`), which catches the most common breakage
(accidentally pulling in CGo or a non-pure-Go dependency). Run the actual
`gomobile bind` locally on a machine with the SDKs before shipping.

## Threading / ANR

All methods are **synchronous and blocking**, and they **copy `[]byte` across the
binding boundary** (the bytes are duplicated into the Go heap and back). A large
update on the UI thread can jank or trigger an ANR. Therefore:

- Call `ApplyUpdate` and the `Encode*` methods **off the UI thread** — Kotlin
  `Dispatchers.IO`, or a Swift background `DispatchQueue` / `Task.detached`.
- On the hot path, prefer the **incremental** `EncodeDiff(remoteStateVector)`
  over full-state `EncodeStateAsUpdate`, so you only ship what the peer is missing.

Each method is safe to call from any thread; the wrappers guard their state
internally.

## Lifecycle

Call **`Close()`** when you are done with a `Doc` or `Awareness` — e.g. from
`ViewModel.onCleared()` on Android or `deinit` on Swift. This releases the heavy
Go-side state promptly. **Do not rely on cross-binding finalization** to collect
it for you; the Go GC and the host runtime's GC do not coordinate.

`Close()` is idempotent. After `Close`:

- error-returning methods return an error (`ErrClosed`), and
- value-returning methods return zero values (`""`, `0`, `nil`).

Nothing panics after `Close`.

## Binary size & ABI

The generated `mobile.aar` is **multi-arch and several MB** — it bundles a copy of
the Go runtime plus ygo for every target ABI. To keep your app download small:

- Use **ABI splits** or publish an **Android App Bundle (AAB)** so Play delivers
  only the device's ABI.
- Ship **`arm64-v8a`** for real devices, and add **`x86_64`** if you need emulator
  support. Google Play **requires 64-bit**, so don't ship a 32-bit-only build.

## Error handling

A `(T, error)` return surfaces as a **thrown exception** in Kotlin and Swift. In
particular, `ApplyUpdate` can throw if a peer sends a **corrupt or malformed
update** — `catch` it and skip/log the bad message rather than letting it crash
the app:

```kotlin
try {
    doc.applyUpdate(peerBytes)
} catch (e: Exception) {
    Log.w("ygo", "dropping bad update", e)
}
```

## Change notifications (pull model)

This package does **not** push change events across the boundary (an
observer/Flow callback is a planned follow-up — callbacks aren't part of the
gomobile-safe surface). The app owns the apply call site, so the recommended
pattern is a **pull model**:

1. Apply the incoming update on an IO/background thread (`doc.applyUpdate(...)`).
2. Emit a signal to the UI layer — a Kotlin `StateFlow` / Swift `@Published`.
3. The UI re-reads the document via `GetText` / `GetMapJSON` / `GetArrayJSON`.

## JSON shapes (known limitation)

The JSON-returning methods currently expose **ygo's internal Go struct shapes**,
not idiomatic Yjs JSON:

- **`GetTextJSON`** returns ygo's `crdt.Delta` struct shape — Go field names —
  e.g.

  ```json
  [{ "Op": 0, "Insert": "hi", "Delete": 0, "Retain": 0, "Attributes": { "bold": true } }]
  ```

  not the idiomatic Yjs delta `[{ "insert": "hi", "attributes": { "bold": true } }]`.

- **`StatesJSON`** returns a map keyed by client ID whose values expose the
  internal clock and capitalized keys — e.g.

  ```json
  { "12345": { "Clock": 7, "State": { "user": "alice" } } }
  ```

These are functional but **not stable/idiomatic**, and idiomatic shapes are a
**planned follow-up**. Consumers should **not hard-code against these shapes
long-term**. (`GetText`, `GetMapJSON`, and `GetArrayJSON` return the plain text
and the natural JSON of the map/array contents and are unaffected.)

- **`LocalStateJSON`** yields JSON `null` when there is no local presence yet
  (freshly constructed, or after `ClearLocalState`), which is **distinct from
  `{}`** (a present-but-empty state set via `SetLocalState`). Treat `null` vs
  `{}` as a meaningful absent/present distinction, not interchangeable.

## Examples

### Kotlin (Android)

```kotlin
import mobile.Mobile  // gomobile package object
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.withContext

// Create a document with a random client ID.
val doc = Mobile.newDoc()

// Apply a peer update off the UI thread, then re-read on the main thread.
suspend fun onPeerUpdate(update: ByteArray) {
    withContext(Dispatchers.IO) {
        try {
            doc.applyUpdate(update)      // throws on a corrupt update
        } catch (e: Exception) {
            Log.w("ygo", "dropping bad update", e)
            return@withContext
        }
    }
    val text = doc.getText("content")     // re-read for the UI (does not throw)
    render(text)
}

// Presence via awareness. newAwareness / setLocalState / statesJSON all throw,
// so wrap them; getText / clientID above are pure-value calls and do not.
var awareness: Awareness? = null
try {
    awareness = Mobile.newAwareness(doc.clientID())
    awareness.setLocalState("""{"user":"alice"}""".toByteArray())
    val states = String(awareness.statesJSON())  // { "<id>": { "Clock": N, "State": {...} } }
} catch (e: Exception) {
    Log.w("ygo", "awareness setup failed", e)
}

// Release when the screen goes away.
override fun onCleared() {
    awareness?.close()
    doc.close()
}
```

### Swift (iOS)

```swift
import Mobile  // Mobile.xcframework

// Create a document with a random client ID.
let doc = MobileNewDoc()!

// Apply a peer update off the main thread, then re-read on the main thread.
func onPeerUpdate(_ update: Data) {
    DispatchQueue.global(qos: .userInitiated).async {
        do {
            try doc.applyUpdate(update)   // throws on a corrupt update
        } catch {
            print("dropping bad update: \(error)")
            return
        }
        let text = doc.getText("content")
        DispatchQueue.main.async { self.render(text) }
    }
}

// Presence via awareness.
var error: NSError?
let awareness = MobileNewAwareness(doc.clientID(), &error)!
try? awareness.setLocalState(#"{"user":"alice"}"#.data(using: .utf8)!)
let states = String(data: try! awareness.statesJSON(), encoding: .utf8)!

// Release in deinit.
deinit {
    awareness.close()
    doc.close()
}
```

> The exact generated symbol names (e.g. `Mobile.newDoc` / `MobileNewDoc`) depend
> on your `gomobile bind` package/prefix settings; adjust to match your build.
