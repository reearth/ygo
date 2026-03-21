# ygo

[![CI](https://github.com/reearth/ygo/actions/workflows/ci.yml/badge.svg)](https://github.com/reearth/ygo/actions/workflows/ci.yml)
[![Go Reference](https://pkg.go.dev/badge/github.com/reearth/ygo.svg)](https://pkg.go.dev/github.com/reearth/ygo)
[![Go Report Card](https://goreportcard.com/badge/github.com/reearth/ygo)](https://goreportcard.com/report/github.com/reearth/ygo)
[![License](https://img.shields.io/badge/license-Apache%202.0-blue.svg)](LICENSE)
[![OpenSSF Best Practices](https://www.bestpractices.dev/projects/placeholder/badge)](https://www.bestpractices.dev/projects/placeholder)

**ygo** is a pure-Go implementation of the [Yjs](https://github.com/yjs/yjs) CRDT (Conflict-free Replicated Data Type) library, enabling real-time collaborative applications in Go backends without CGO or embedded runtimes.

It is **binary-compatible** with the JavaScript Yjs reference implementation — updates produced by ygo can be applied by Yjs clients, and vice versa.

## Status

> **This library is under active development and is not yet ready for production use.**
> See the [project roadmap](https://github.com/reearth/ygo/issues) for current progress.

| Component              | Status          |
|------------------------|-----------------|
| `encoding/`            | ✅ Complete     |
| `crdt/` core           | ✅ Complete     |
| `crdt/types/`          | ✅ Complete     |
| Update encoding V1     | ✅ Complete     |
| Update encoding V2     | ✅ Complete     |
| Sync protocol          | ✅ Complete     |
| Awareness              | ✅ Complete     |
| WebSocket handler      | ✅ Complete     |
| HTTP handler           | ✅ Complete     |
| Snapshots / GC         | ✅ Complete     |

## Features

- **Pure Go** — no CGO, no V8, no embedded JavaScript engine
- **Binary-compatible** — interoperates with JS Yjs, Yrs (Rust), and any compliant Yjs client
- **Full type support** — YText, YArray, YMap, YXmlFragment, YXmlElement, YXmlText
- **Both update formats** — UpdateV1 and UpdateV2 (with V1↔V2 conversion)
- **Sync protocol** — implements [y-protocols](https://github.com/yjs/y-protocols) SyncStep1/2 and incremental updates
- **Awareness** — presence, cursor sharing, and ephemeral state
- **Snapshots** — point-in-time document history and restore
- **Transport-agnostic** — core logic has no transport dependency; WebSocket and HTTP handlers are addons
- **Idiomatic API** — designed for Go developers, not a transliteration of the JS API

## Requirements

- Go 1.23 or later

## Installation

```bash
go get github.com/reearth/ygo
```

## Quick Start

```go
package main

import (
    "fmt"
    "github.com/reearth/ygo/crdt"
)

func main() {
    // Create two peers
    alice := crdt.New()
    bob := crdt.New()

    // Alice makes edits
    alice.Transact(func(txn *crdt.Transaction) {
        text := alice.GetText("content")
        text.Insert(txn, 0, "Hello, world!")
    })

    // Encode Alice's state and send to Bob
    update := alice.EncodeStateAsUpdate()

    // Bob applies the update — both docs now converge
    if err := bob.ApplyUpdate(update); err != nil {
        panic(err)
    }

    fmt.Println(bob.GetText("content").String()) // "Hello, world!"
}
```

## WebSocket Server

```go
package main

import (
    "net/http"
    "github.com/reearth/ygo/provider/websocket"
)

func main() {
    server := websocket.NewServer()
    http.Handle("/yjs/{room}", server)
    http.ListenAndServe(":8080", nil)
}
```

## Architecture

See [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md) for a detailed explanation of the CRDT algorithm, data model, and package design.

## Compatibility

ygo targets compatibility with:

- [Yjs](https://github.com/yjs/yjs) v13.x (JavaScript reference implementation)
- [y-protocols](https://github.com/yjs/y-protocols) sync and awareness protocol
- [lib0](https://github.com/dmonad/lib0) binary encoding format

Compatibility is verified by golden-file tests that compare binary output byte-for-byte with Yjs-generated fixtures.

## Contributing

Contributions are welcome! Please read [CONTRIBUTING.md](CONTRIBUTING.md) before submitting a pull request.

For significant changes, open an issue first to discuss what you'd like to change.

## Security

Please report security vulnerabilities by following the process in [SECURITY.md](SECURITY.md). Do not open public issues for security problems.

## License

Apache License 2.0 — see [LICENSE](LICENSE).

This project is not affiliated with the Yjs authors. Yjs is developed by [Kevin Jahns](https://github.com/dmonad) and contributors.
