## What's new

- **`Doc.TransactE` and `Doc.TransactContextE` (#14).** Error-returning variants of the existing transaction methods. Callers who detect a logical error inside `fn` can now return it cleanly instead of panicking or threading an out-of-band channel. `fn`'s returned error becomes the method's return value. Mutations still commit regardless of error (no rollback — matches Yjs JS and yrs); observers fire before the error returns. For `TransactContextE`, ctx cancellation wins over fn error when both fire.
- **Strictly additive.** Existing `Transact` and `TransactContext` keep their signatures and behavior unchanged.

## Install

```
go get github.com/reearth/ygo@v1.3.0
```

See [CHANGELOG.md](https://github.com/reearth/ygo/blob/main/CHANGELOG.md) for full details.
