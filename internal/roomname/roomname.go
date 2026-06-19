// Package roomname centralises the room-name validation rule shared by the
// HTTP and WebSocket providers so both enforce identical limits (issue #50).
package roomname

// Valid reports whether name is a safe, non-empty room identifier. The rule
// (originally the WebSocket provider's isValidRoomName) rejects: the empty
// string, names longer than 255 bytes, the path-traversal names "." and "..",
// and any name containing a control character (rune < 0x20). All other
// printable content — including spaces and Unicode — is permitted, matching the
// permissive behaviour of the y-websocket JS server.
func Valid(name string) bool {
	if len(name) == 0 || len(name) > 255 {
		return false
	}
	if name == "." || name == ".." {
		return false
	}
	for _, r := range name {
		if r < 0x20 {
			return false
		}
	}
	return true
}
