## What's new

YMap wire-format conformance with the Yjs reference implementation. A
cross-reference run of a yjs-generated fixture suite against ygo surfaced three
YMap interop bugs that round-tripped fine ygo↔ygo but diverged from genuine
`yjs` bytes. All three are fixed and verified in **both** directions against
`yjs@13.6.30`.

> Independent of the in-flight v1.21.0 (`cluster/redis`) release — v1.22.0
> touches only the `crdt` package.

### Fixed — duplicate map keys (last-write-wins)

Overwriting a map value (`m.Set("k", 1)` then `m.Set("k", 2)` — the most common
map operation) creates a second item that carries an *origin*, and Yjs therefore
writes **no `parentSub` string** for it on the wire, even though the "has key"
flag is set. ygo read the key string anyway, misaligning the byte stream:

- **V1** decode aborted (`unknown Any tag` / `unexpected end of input`) and
  rejected the entire update — total data loss.
- **V2** decode silently dropped the overwritten key.

The decoder now reads the key only when no origin is present and inherits it
from the origin item during integration; the encoder is fixed symmetrically.
ygo↔ygo round-trips were green before only because the encoder had the
mirror-image bug — self-consistent, but non-conformant.

### Fixed — empty-string map keys

`m.Set("", v)` is valid in Yjs, but ygo used `""` internally to mean "no key"
(a sequence element), so empty-keyed entries were dropped in both wire versions.
The internal key representation is now `*string` (`nil` = sequence element,
`&""` = genuine empty key), and empty keys survive encode/decode.

### Conformance coverage

- 10 YMap scenarios captured from `yjs@13.6.30` decoded as genuine reference
  bytes, plus ygo→ygo round-trip stability — 30 new conformance subtests.
- Go→JS interop fixtures (`ymap_lww`, `ymap_empty_key`) prove ygo's encoder
  output decodes correctly in real Yjs.

## ⚠️ Upgrade note (no code change required)

The public API is unchanged. The **on-wire encoding** of overwritten and
empty-keyed map entries changed to match Yjs, so **V1/V2 snapshots persisted by
ygo ≤ v1.20.0 that contain those patterns will not decode correctly on this
version**. Live-sync deployments are unaffected (peers upgrade together); only
stored snapshots with overwritten/empty map keys are. That data was never
readable by real Yjs anyway — re-encode affected snapshots once from a ≤ v1.20.0
instance, or re-sync.

## Install

```
go get github.com/reearth/ygo@v1.22.0
```

See [CHANGELOG.md](https://github.com/reearth/ygo/blob/main/CHANGELOG.md) for full details.
