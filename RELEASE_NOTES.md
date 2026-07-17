## v1.32.0

A minor release: one new public API for working inside transactions, plus two
substantial testing/CI hardening efforts that lock in ygo's convergence and
cross-language Yjs conformance. No breaking changes.

### Added

- **Transaction-scoped root accessors** — `Transaction.GetText`, `GetMap`,
  `GetArray`, `GetXmlFragment` resolve (and create on first use) root types from
  inside a `Transact` callback without re-locking, reusing the lock the
  transaction already holds ([#138](https://github.com/reearth/ygo/issues/138),
  thanks @frodex). Previously the natural `doc.GetText(...)` call inside a
  callback self-deadlocked the non-reentrant document lock. The accessors are
  valid only inside the callback that received the transaction and **panic once
  the transaction has committed** (e.g. from an `OnAfterTransaction` observer or
  a retained `*Transaction`) rather than silently touching document state
  without the lock.

- **Randomized convergence fuzz framework
  ([#70](https://github.com/reearth/ygo/issues/70)).** `testutil/fuzz` generates
  seed-reproducible multi-peer scenarios (random ops across Text/Array/Map/XML,
  nested containers, delivery via Apply/Merge/Diff on V1 and V2, plus GC),
  asserts all ygo peers converge, and — when Node + `yjs` are present — checks
  ygo against real Yjs both logically and by round-trip. Failures print a seed
  (`FUZZ_SEED=<n>` to replay) and shrink to a minimal scenario; a regression
  corpus locks in known cases. CI runs a fixed 1,000-seed set. This framework
  found the convergence divergences fixed in v1.31.5 and v1.31.6.

- **Cross-language Yjs conformance is now enforced in CI
  ([#99](https://github.com/reearth/ygo/issues/99)).** Cross-impl tests hard-fail
  (no silent skip) under `YGO_REQUIRE_NODE=1`; a symmetric Map/Array/Text/XML
  fixture matrix (V1+V2) decodes genuine `yjs@13.6.30` bytes node-free; and a
  fixture-drift CI job regenerates from the pinned yjs and fails on divergence.

### Changed

- **Documented the locking contract honestly** on `Transact` and the Doc-level
  accessors: anything that takes the document lock still deadlocks silently when
  called inside a `Transact` callback (Go exposes no goroutine identity with
  which to detect it). Use the transaction accessors above, or resolve handles
  before the transaction.

## Install

```
go get github.com/reearth/ygo@v1.32.0
```

See [CHANGELOG.md](https://github.com/reearth/ygo/blob/main/CHANGELOG.md) for full details.
