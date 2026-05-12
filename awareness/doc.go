// Package awareness implements the Yjs awareness protocol for ephemeral
// state such as user presence, cursor positions, and selections.
//
// Awareness state is not persisted, not replayed on reconnect, and
// expires after a period of inactivity. It is separate from document updates.
//
// Reference: https://github.com/yjs/y-protocols/blob/master/awareness.js
//
// # Quick start
//
//	a := awareness.New(clientID)
//	a.SetLocalState(map[string]any{"name": "Alice", "cursor": 42})
//	a.OnChange(func(evt awareness.ChangeEvent) {
//	    // react to added/updated/removed peers
//	})
//	update := a.EncodeUpdate(nil)
//	// send update to peers; peers call a.ApplyUpdate(update, origin)
//
// See the Example* functions for canonical usage patterns.
//
// # Stability
//
// ygo follows semantic versioning. The v1.x public API is considered
// stable: new functionality lands as minor releases; bug fixes as patch
// releases; breaking changes are deferred to v2.
package awareness
