## What's new

The attribution API (issue #56): a Go port of yjs-v14's `IdSet`/`IdMap`/
`ContentMap` primitives for stamping CRDT content with per-item authorship
metadata — who inserted or deleted which item, plus any attributes you want
to attach (`userid`, `ts`, a request ID, …) — without integrating the update
into a doc. This is the pattern y-redis (also known as y/hub, the Yjs
cluster backend) uses to store per-character authorship in Postgres.

`IDSet`/`IDMap` encode/decode byte-for-byte compatibly with published yjs v14
(pinned `npm:yjs@14.0.0-16`). `ContentMap`/`ContentIDs` follow yjs-main's
`writeContentMap`/`writeContentIds` composition (two `IdMap`s/`IdSet`s,
inserts then deletes) — each half is byte-verified against that same pin,
but no published yjs v14 rc yet exposes a top-level `ContentMap` encoder to
pin the wrapper itself against; that lands only once yjs v14.0.0 final ships
(tracked as a follow-up).

### Added

- **`crdt.IDSet` / `crdt.IDMap`** — yjs-v14 `IdSet`/`IdMap`: per-client
  sorted item-ID runs, with `IDMap` additionally carrying attribution data
  per range (overlaps split, attributes joined on read). Wire codecs
  `EncodeIDSet`/`DecodeIDSet` and `EncodeIDMap`/`DecodeIDMap` are
  byte-compatible with published yjs v14 (`npm:yjs@14.0.0-16`).
- **`crdt.ContentAttribute`** — one `{Name, Value}` attribution fact via
  `NewContentAttribute`/`MustContentAttribute`, validated against the lib0
  `any` domain up front.
- **`crdt.ContentIDs` / `crdt.ContentMap`** — the insert/delete pair of
  `IDSet`s or `IDMap`s for an update or doc, with matching
  `EncodeContentIDs`/`DecodeContentIDs` and
  `EncodeContentMap`/`DecodeContentMap` codecs.
- **Builders** — `ContentIDsFromUpdateV1`/`V2` (stamp an incoming update
  without integrating it), `InsertSetFromDoc`/`DeleteSetFromDoc` (extract
  from a live doc), `CreateContentMapFromContentIDs` (attach attributes to
  extracted IDs).
- **Set algebra** — `MergeIDSets`/`MergeIDMaps`,
  `ExcludeIDSet`/`ExcludeIDMap`, `IntersectIDSets`/`IntersectIDMaps`,
  `FilterIDMap`, and the `ContentMap`-level
  `MergeContentMaps`/`ExcludeContentMap`/`IntersectContentMaps`/`FilterContentMap`
  wrappers.
- A runnable godoc example, `crdt.Example_attribution`
  (`crdt/example_attribution_test.go`).

### Security

- All four new decoders (`DecodeIDSet`, `DecodeIDMap`, `DecodeContentIDs`,
  `DecodeContentMap`) bound every length-prefixed count against the
  decoder's remaining input before allocating, so malformed input fails fast
  rather than triggering an oversized allocation.

### Scope

- No storage integration — callers persist `EncodeContentMap(cm)` themselves.
- `diffDocsToDelta` (yjs-main's delta-with-attribution renderer) is not
  implemented; it depends on yjs v14's delta/renderer subsystem, still
  changing on `main`. Tracked as a follow-up.
- Garbage collection erases attributed history: `IDSet`/`IDMap` reference
  items by `(client, clock)`, so once GC frees a deleted item's content, its
  attributed range no longer corresponds to retrievable data. Retain raw
  updates, or create the doc `WithGC(false)`, to render attributed history
  later.

See the [README Attribution section](https://github.com/reearth/ygo#attribution)
for the full primitive list and a worked example.

## Install

```
go get github.com/reearth/ygo@v1.31.0
```

See [CHANGELOG.md](https://github.com/reearth/ygo/blob/main/CHANGELOG.md) for full details.
