// Package mobile provides a gomobile-bindable façade over ygo's crdt and
// awareness packages, for embedding ygo natively in iOS and Android apps via
// `gomobile bind` — no JavaScript runtime, no CGo.
//
// # gomobile-safe surface
//
// gomobile bind only supports a restricted set of types across the language
// boundary. Every exported function and method in this package therefore uses
// ONLY: string, int64, bool, []byte, error, and the bound pointers *Doc and
// *Awareness. It never exposes unsigned ints, maps, non-byte slices, `any`,
// variadics, or callbacks. (crdt/awareness use uint64 IDs, maps, and []uint64
// internally; this package translates at the boundary.)
//
// # Threading
//
// All methods are safe to call from any goroutine/thread, but they are
// SYNCHRONOUS and BLOCKING and copy []byte across the bind boundary. Call
// ApplyUpdate / Encode* off the UI thread (e.g. Kotlin Dispatchers.IO, a Swift
// background queue); a large update on the main thread can jank or ANR. Prefer
// incremental EncodeDiff over full-state EncodeStateAsUpdate on the hot path.
//
// # Lifecycle
//
// Call Close() when done (e.g. ViewModel.onCleared / Swift deinit) to release
// the heavy Go-side state promptly, rather than relying on cross-binding
// finalization. After Close, error-returning methods return ErrClosed and
// value-returning methods return zero values; nothing panics.
package mobile

import "errors"

// ErrClosed is returned by methods called after Close.
var ErrClosed = errors.New("ygo/mobile: used after Close")

// maxSafeInteger is JavaScript's Number.MAX_SAFE_INTEGER (2^53). Client IDs must
// not exceed it, or they cannot be represented by JS Yjs peers.
const maxSafeInteger = int64(1) << 53

// checkClientID validates a caller-supplied client ID for cross-language safety:
// non-negative and within JS safe-integer range.
func checkClientID(id int64) error {
	if id < 0 || id > maxSafeInteger {
		return errors.New("ygo/mobile: client ID must be in [0, 2^53]")
	}
	return nil
}
