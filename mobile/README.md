# ygo/mobile — native iOS/Android bindings

`mobile/` is a [gomobile](https://pkg.go.dev/golang.org/x/mobile)-bindable façade
over ygo's `crdt` and `awareness` packages. It lets you embed ygo's Yjs-compatible
CRDT engine directly in native iOS and Android apps via `gomobile bind` — no
JavaScript runtime and no CGo. It is a **full on-device editor**: apply peer
updates, encode state and incremental diffs, read the current document and
presence, **mutate** text/array/map roots locally, and **observe** committed
changes to drive the UI.

## gomobile-safe type constraint

`gomobile bind` only supports a restricted set of types across the language
boundary, so **every exported function and method in this package uses only**:
`string`, `int64`, `bool`, `[]byte`, `error`, and the bound pointers `*Doc` and
`*Awareness`. It never exposes unsigned ints, maps, non-byte slices, `any`,
variadics, or callbacks. (The underlying `crdt` / `awareness` packages use
`uint64` client IDs, maps, and `[]uint64` internally; this package translates at
the boundary — e.g. client IDs are `int64` and constrained to `[0, 2^53 - 1]`,
the JS safe-integer range (`Number.MAX_SAFE_INTEGER`), and structured values
cross the boundary as JSON `[]byte`.)

The bound surface is:

- **`Doc`** — `NewDoc`, `NewDocWithClientID(int64)`, `ClientID`, `ApplyUpdate`,
  `EncodeStateAsUpdate`, `EncodeStateVector`, `EncodeDiff`, `GetText`,
  `GetTextJSON`, `GetMapJSON`, `GetArrayJSON`, the on-device mutators
  `InsertText`, `InsertTextWithAttributes`, `DeleteText`, `FormatText`,
  `InsertArray`, `DeleteArray`, `SetMap`, `DeleteMapKey`, plus
  `Observe(DocObserver)` and `Close`.
- **`Awareness`** — `NewAwareness(int64)`, `ClientID`, `SetLocalState`,
  `ClearLocalState`, `LocalStateJSON`, `StatesJSON`, `EncodeAll`, `ApplyUpdate`,
  `Observe(AwarenessObserver)`, and `Close`.
- **Observing** — `Observe` returns a `*Subscription` (`Close()` detaches it);
  `DocObserver.OnChange(updateV1 []byte, local bool)` and
  `AwarenessObserver.OnChange(changesJSON []byte)` are the callback interfaces
  your app implements.

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

## Editing

`Doc` is a full editor: mutate its text, array, and map roots on-device and the
changes flow out through `EncodeStateAsUpdate` / `EncodeDiff` (and to observers)
like any other update. Each mutator **validates its arguments, wraps the change in
a single transaction, and returns an `error`** on bad input (out-of-range index,
malformed JSON) — it never panics, so a bad edit surfaces as a thrown exception in
Kotlin/Swift rather than a crash. Like every other blocking call, **run mutators
off the UI thread**.

- **Text** — `insertText(name, index, text)`,
  `insertTextWithAttributes(name, index, text, attrsJSON)`,
  `deleteText(name, index, length)`, `formatText(name, index, length, attrsJSON)`.
  `attrsJSON` is a JSON object (`{"bold":true}`); a `null` attribute value follows
  Yjs's formatting-removal convention.
- **Array** — `insertArray(name, index, valuesJSON)` (`valuesJSON` is a JSON
  array of elements), `deleteArray(name, index, length)`.
- **Map** — `setMap(name, key, valueJSON)` (`valueJSON` is any JSON value),
  `deleteMapKey(name, key)`.

```kotlin
// Off the UI thread — e.g. inside withContext(Dispatchers.IO).
try {
    doc.insertText("content", 0, "Hello")
    doc.formatText("content", 0, 5, """{"bold":true}""".toByteArray())
    doc.setMap("meta", "title", "\"Untitled\"".toByteArray())
} catch (e: Exception) {
    Log.w("ygo", "rejected edit", e)   // bad index / malformed JSON — never a crash
}
```

> **Limitation:** JSON objects/arrays passed to `setMap` / `insertArray` decode to
> **plain values**, not nested `YMap` / `YText` / `YArray` shared types — the
> mobile layer cannot construct nested shared types across the bind boundary. A
> `{"a":1}` value is stored as a plain JSON object, not a live nested `YMap`.

## Observing changes

Rather than polling, subscribe for **push notifications** after every committed
change. `Doc.Observe` and `Awareness.Observe` each take an observer and return a
`*Subscription`; call `subscription.close()` to detach one, and `Doc.Close()` /
`Awareness.Close()` detach every observer automatically.

**Callbacks arrive on a background goroutine** — never the UI thread, and never
under a lock. Marshal to the main thread before touching UI (a Kotlin
`StateFlow` / Swift `@Published`); do not assume `onChange` runs on the main
thread.

- **`DocObserver.onChange(updateV1, local)`** fires after each committed
  transaction. `updateV1` is the incremental V1 update (feed it straight to your
  server/peers); `local == true` marks a change that originated from a mobile
  mutator on this `Doc` (vs a remote `applyUpdate`), so you can **skip
  re-broadcasting your own edits** and avoid echo loops when also syncing to a
  server.
- **`AwarenessObserver.onChange(changesJSON)`** fires after each presence change
  with `{"added":[…],"updated":[…],"removed":[…]}` (client-id arrays, sorted,
  always present). The sets are **advisory** — re-read `statesJSON()` for the
  authoritative presence snapshot.

```kotlin
class DocBridge(private val doc: Doc, private val scope: CoroutineScope) {
    private val _text = MutableStateFlow(doc.getText("content"))
    val text: StateFlow<String> = _text

    private val sub = doc.observe(object : DocObserver {
        override fun onChange(updateV1: ByteArray, local: Boolean) {
            // Background goroutine → hop to the main thread before touching UI.
            scope.launch(Dispatchers.Main) { _text.value = doc.getText("content") }
            if (local) scope.launch(Dispatchers.IO) { server.send(updateV1) } // push our own edits out
        }
    })

    fun dispose() { sub.close(); doc.close() }  // sub.close() also implied by doc.close()
}
```

For Swift, implement the same `DocObserver` protocol and publish via `@Published`,
hopping to `DispatchQueue.main` inside `onChange` before touching any observable
state.

You can still use a **pull model** instead (apply on a background thread, signal
the UI, re-read via `GetText` / `GetMapJSON` / `GetArrayJSON`) where that fits
better — observers are additive, not required.

## JSON shapes

The JSON-returning accessors emit **idiomatic Yjs JSON**, safe to hand straight to
a JS/Quill consumer or decode natively:

- **`GetTextJSON`** returns an idiomatic Yjs **delta** — a JSON array of ops, each
  carrying exactly one of `insert` / `retain` / `delete` plus optional
  `attributes` (a full-content read yields `insert` ops only), e.g.

  ```json
  [{ "insert": "hi", "attributes": { "bold": true } }]
  ```

  An absent/empty root returns `[]` (never `null`), so you can iterate it
  unconditionally.

- **`StatesJSON`** returns a JSON object keyed by stringy client ID whose value is
  that client's raw state object (the internal clock is not exposed), e.g.

  ```json
  { "12345": { "user": "alice" } }
  ```

  An empty set returns `{}`.

- **`LocalStateJSON`** yields JSON `null` when there is no local presence yet
  (freshly constructed, or after `ClearLocalState`), which is **distinct from
  `{}`** (a present-but-empty state set via `SetLocalState`). Treat `null` vs
  `{}` as a meaningful absent/present distinction, not interchangeable.

(`GetText`, `GetMapJSON`, and `GetArrayJSON` return the plain text and the natural
JSON of the map/array contents.)

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
    val states = String(awareness.statesJSON())  // { "<id>": { ...state } }
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
