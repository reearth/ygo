# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Added
- Initial repository structure and CI/CD pipeline
- Project architecture documentation
- `sync.ReadSyncMessage` — parses incoming y-protocol messages into type + payload
- `awareness.StartAutoExpiry` — background goroutine that removes stale peer states after a configurable timeout
- `provider/websocket`: `PersistenceAdapter` interface, `MemoryPersistence` in-memory implementation, and `NewServerWithPersistence` constructor for pluggable document storage
- B4 editing-trace benchmark suite (`BenchmarkB4_Apply/Encode/EncodeV2/Decode/Size`) with baseline results in `benchmarks/README.md`
- LRU position cache (80 entries) in `abstractType` for O(1) average-case index lookups

### Changed
- `Doc.OnUpdate` callback signature changed from `func(origin any)` to `func(update []byte, origin any)` — the incremental binary update is now passed directly to observers
- `ClientID` generation changed from `rand.Uint64()` to `rand.Uint32()` to stay within the Yjs wire protocol's 53-bit VarUint limit

### Fixed
- ClientID values ≥ 2^53 caused encode/decode round-trip failures (~1 in 256 documents with the old random generation)
- Sequential insertions into large documents degraded to O(n²) because the LRU position cache was cleared on every insertion; cache is now only invalidated on middle insertions
- Crafted binary inputs could trigger multi-GB allocations in all V1/V2 decoder loops; OOM guards added throughout
- `RunGC` rewrote with a correct two-pass algorithm: first pass replaces deleted content with tombstones, second pass merges adjacent tombstones without breaking linked-list references

[Unreleased]: https://github.com/reearth/ygo/compare/HEAD...HEAD
