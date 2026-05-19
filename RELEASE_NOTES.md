## What's new

Second in the [`gaps` label](https://github.com/reearth/ygo/issues?label=gaps) series from the cross-reference audit against Yjs JS and yrs. Closes the lib0 `Any` tagged-union parity gaps in `encoding/`.

- **Integer dispatch now matches lib0 by magnitude** (#77). ygo previously emitted tag 125 (int + VarInt) for any `int`/`int64` up to 2^55-1. lib0 only uses tag 125 for int32-range values; larger integers go to tag 123 (float64) or — in ygo's case, where Go's int64 has more range than JS Number — tag 122 (BigInt) to preserve precision. ygo now matches:

  | Range | Tag |
  |-------|-----|
  | `[-2^31, 2^31)` | 125 (VarInt) |
  | safe-int range (outside int32) | 123 (float64) |
  | beyond float64 safe-int | 122 (BigInt) |

  Yjs JS readers see byte-for-byte parity with their own writer for the first two ranges and receive `bigint` for the third (which is the natural cross-impl representation when an integer is too large for `Number`).

  Compatibility implication for Go callers: an `int64(2^35)` now round-trips as `float64`, and `int64(2^55)` (which previously panicked in `WriteVarInt`) now round-trips as `encoding.BigInt`. See CHANGELOG for the full table.

- **`WriteAny(float64)` narrows to float32 when lossless** (#77). Values like `1.5`, `-0`, and small integer-valued floats now emit tag 124 (4 bytes on the wire) instead of always tag 123 (8 bytes). Matches lib0's `isFloat32` dispatch — halves the wire size for these values.

- **`ReadVarString` rejects invalid UTF-8** (#77). Returns the new `encoding.ErrInvalidUTF8` rather than silently producing a corrupt Go string. Matches lib0's `TextDecoder('utf-8', { fatal: true })`. There's a ~4ns per-call cost from the UTF-8 scan; correctness takes priority — silent corruption surfaces as untraceable bugs downstream.

- **`WriteAny` accepts the rest of Go's numeric tower** (#77). `uint`, `uint8`, `uint16`, `uint32`, `uint64`, `int8`, `int16`, `int32` previously panicked; they're now promoted to `int64` and dispatched normally. `uint64` values exceeding `math.MaxInt64` fall back to tag 123 (float64) with documented precision loss, matching lib0's behavior for very-large `Number`s.

## Install

```
go get github.com/reearth/ygo@v1.10.0
```

See [CHANGELOG.md](https://github.com/reearth/ygo/blob/main/CHANGELOG.md) for full details.
