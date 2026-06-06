## What's new

Yjs wire-format conformance. A cross-reference run of a yjs-generated fixture
suite — and a follow-on source-level diff of ygo's wire codec against the Yjs
reference — surfaced a cluster of interop bugs (YMap entries and several content
types) that round-tripped fine ygo↔ygo but diverged from genuine `yjs` bytes.
All are fixed and verified in **both** directions against `yjs@13.6.30`.

> Independent of v1.21.0 (`cluster/redis`) — v1.22.0 touches only the `crdt`
> package.

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

### Fixed — content types (embed, subdoc, XML hook)

A source-level diff of ygo's whole wire codec against the canonical Yjs source
(the method the YMap bugs were found with) surfaced three more cross-library
breaks, all reproduced with genuine `yjs@13.6.30` bytes:

- **`YText` embeds** — Yjs's `writeJSON` is a JSON-text varstring in V1 but a
  structured `writeAny` in V2. ygo used `WriteAny` for both, so V1 embeds
  (`InsertEmbed`) neither decoded from nor encoded to real Yjs. V1 now uses
  JSON text.
- **Subdocument `opts`** — Yjs writes `guid` + `writeAny(opts)`. ygo's V1
  omitted `opts` (stream desync); V2 wrote `null` (genuine Yjs crashes on
  `opts.shouldLoad`). V1 now reads/writes `opts`; V2 writes `{}`.
- **`YXmlHook`** — the V1 decoder didn't consume the hook's name string, so a
  Yjs document containing a hook corrupted the rest of the update. It now
  degrades gracefully like the V2 decoder.

### Fixed — UTF-16 mid-surrogate splitting

`Y.Text` is indexed in UTF-16 code units, so an emoji (supplementary character)
occupies 2 units. When an index bisects a surrogate pair, Yjs slices the pair
and replaces each lone half with U+FFFD — e.g. `"a😀c"`, insert `"X"` at 2 →
`"a�X�c"`. ygo previously rounded the split forward to the next whole rune,
yielding different content and item-clock boundaries than a JS peer. A shared
`splitUTF16` helper now matches Yjs on the split and both encoder tail-slice
paths (V1 + V2), verified against `yjs@13.6.30`. Clean (between-character) splits
are unaffected; this only ever triggered on indices interior to a surrogate pair.

### Conformance coverage

- 10 YMap + 3 content-type scenarios captured from `yjs@13.6.30`, decoded as
  genuine reference bytes with ygo→ygo round-trip stability.
- Go→JS interop fixtures (`ymap_lww`, `ymap_empty_key`, `ytext_embed`, and a
  `subdoc` re-encode that proves Yjs no longer crashes on ygo's output) confirm
  the encoder is conformant in both directions.

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
