// Package roomname centralises the room-name validation rule shared by the
// HTTP and WebSocket providers so both enforce identical limits (issue #50).
package roomname

import "unicode/utf8"

// Valid reports whether name is a safe, non-empty room identifier. The rule
// (originally the WebSocket provider's isValidRoomName) rejects: the empty
// string, names longer than 255 bytes, the path-traversal names "." and "..",
// any name containing a control character (rune < 0x20), and any name that is
// not valid UTF-8 (it would be unencodable on the wire — issue #209). All
// other printable content — including spaces and Unicode — is permitted,
// matching the permissive behaviour of the y-websocket JS server.
func Valid(name string) bool {
	if len(name) == 0 || len(name) > 255 {
		return false
	}
	if name == "." || name == ".." {
		return false
	}
	if !utf8.ValidString(name) {
		// Ranging over a string below yields utf8.RuneError (U+FFFD) for
		// invalid bytes, which is above the control-character floor, so
		// without this explicit check invalid UTF-8 would slip through the
		// loop and later panic WriteVarString on the wire.
		return false
	}
	for _, r := range name {
		if r < 0x20 {
			return false
		}
	}
	return true
}
