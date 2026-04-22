## What's new

- **`Transaction.Ctx()` for cooperative cancellation (#10).** `fn` inside `Transact` or `TransactContext` can now poll `txn.Ctx()` to detect cancellation and return early. Mutations made before the early return commit; those that would follow do not. Closes a long-standing gap where `TransactContext` promised more than it delivered — Go cannot safely interrupt arbitrary `fn` code, so cooperative polling is the mechanism both Yjs JS and the Rust yrs implementation rely on too.
- **`TransactContext` godoc rewritten** to document the contract explicitly. No behavior change for callers that ignore `txn.Ctx()`.

## Install

```
go get github.com/reearth/ygo@v1.1.2
```

See [CHANGELOG.md](https://github.com/reearth/ygo/blob/main/CHANGELOG.md) for full details.
