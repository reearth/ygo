// Package awareness implements the Yjs awareness protocol for ephemeral
// state such as user presence, cursor positions, and selections.
//
// Awareness state is not persisted, not replayed on reconnect, and
// expires after a period of inactivity. It is separate from document updates.
//
// Reference: https://github.com/yjs/y-protocols/blob/master/awareness.js
package awareness
