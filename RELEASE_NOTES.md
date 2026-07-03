## What's new

Two additive features.

**Read-only WebSocket connections (#59).** The WebSocket provider gains a richer
auth entry point, `Server.Authorize`, that can mark a connection read-only — it
receives document and awareness broadcasts but its inbound writes are dropped
server-side. This matches Hocuspocus's `readOnly` connection flag and covers
public-read docs, viewer roles, and monitoring connections. Additive: the
existing `AuthFunc` is unchanged and connections stay read-write unless you opt in.

**Attribution API (#56).** A Go port of yjs-v14's `IdSet`/`IdMap`/`ContentMap`
primitives for stamping CRDT content with per-item authorship metadata — who
inserted or deleted which item, plus any attributes you attach (`userid`, `ts`, a
request ID, …) — without integrating the update into a doc. This is the pattern
y-redis (also known as y/hub, the Yjs cluster backend) uses to store per-character
authorship in Postgres. `IDSet`/`IDMap` encode/decode byte-for-byte compatibly
with published yjs v14 (pinned `npm:yjs@14.0.0-16`); `ContentMap`/`ContentIDs`
follow yjs-main's `writeContentMap`/`writeContentIds` composition (two
`IdMap`s/`IdSet`s, inserts then deletes) — each half byte-verified against that
pin, with the top-level wrapper pinned once yjs v14.0.0 final ships (follow-up).

### Added

- **`websocket.Server.Authorize func(*http.Request) (ConnectionConfig, bool)`** —
  accepts/rejects a connection (false → 401) *and* reports its config; takes
  precedence over `AuthFunc` when both are set. **`websocket.ConnectionConfig{
  ReadOnly bool }`** carries the per-connection config, extensible for future
  settings. (#59)
- **`crdt.IDSet` / `crdt.IDMap`** — per-client sorted item-ID runs, with `IDMap`
  additionally carrying attribution data per range (overlaps split, attributes
  joined on read). `EncodeIDSet`/`DecodeIDSet` and `EncodeIDMap`/`DecodeIDMap`
  are byte-compatible with published yjs v14 (`npm:yjs@14.0.0-16`). (#56)
- **`crdt.ContentAttribute`** — one `{Name, Value}` attribution fact via
  `NewContentAttribute`/`MustContentAttribute`, validated against the lib0 `any`
  domain up front. (#56)
- **`crdt.ContentIDs` / `crdt.ContentMap`** — the insert/delete pair of `IDSet`s
  or `IDMap`s for an update or doc, with matching `EncodeContentIDs`/`DecodeContentIDs`
  and `EncodeContentMap`/`DecodeContentMap` codecs. (#56)
- **Builders** — `ContentIDsFromUpdateV1`/`V2` (stamp an incoming update without
  integrating it), `InsertSetFromDoc`/`DeleteSetFromDoc`,
  `CreateContentMapFromContentIDs`. (#56)
- **Set algebra** — `MergeIDSets`/`MergeIDMaps`, `ExcludeIDSet`/`ExcludeIDMap`,
  `IntersectIDSets`/`IntersectIDMaps`, `FilterIDMap`, and the `ContentMap`-level
  `MergeContentMaps`/`ExcludeContentMap`/`IntersectContentMaps`/`FilterContentMap`
  wrappers. (#56)
- A runnable godoc example, `crdt.Example_attribution`. (#56)

### Behaviour

- A **read-only** peer still receives broadcasts, gets a `SyncStep2` in reply to
  its `SyncStep1`, and can query awareness — but its inbound document writes
  (`SyncStep2`/`Update`, Hocuspocus `SyncReply`) and awareness updates are dropped
  server-side. Stateless signals are not gated. Connections authorized via
  `AuthFunc` (or with no auth hook) remain read-write. (#59)

### Security

- All four attribution decoders bound every length-prefixed count against the
  decoder's remaining input before allocating, and `DecodeIDMap` additionally
  caps the per-range attribute pre-allocation (N-C2 analogue), so malformed input
  fails fast rather than triggering an oversized allocation. (#56)

### Scope / caveats

- Attribution ships primitives only — no storage integration (callers persist
  `EncodeContentMap(cm)` themselves) and no provider wiring.
- `diffDocsToDelta` is not implemented (depends on yjs v14's still-changing
  delta/renderer subsystem); tracked as a follow-up.
- GC erases attributed history (`IDSet`/`IDMap` reference items by
  `(client, clock)`): retain raw updates, or create the doc `WithGC(false)`, to
  render attributed history later.

## Install

```
go get github.com/reearth/ygo@v1.30.0
```

See [CHANGELOG.md](https://github.com/reearth/ygo/blob/main/CHANGELOG.md) for full details.
