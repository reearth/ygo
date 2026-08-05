package persistence

// conformanceRoomNames is the room-name set both RunRoomListerConformance and
// RunSnapshotStoreConformance probe, shared so the two cannot drift apart again:
// the room suite exercised awkward names while the snapshot suite used the
// literal "room" everywhere (issue #211).
//
// A backend may encode these however it likes on disk, but every operation must
// round-trip the ORIGINAL name and keep distinct names distinct.
var conformanceRoomNames = []string{
	"plain",
	"with/slash",
	"with:colon",
	"with space",
	// Precomposed (NFC).
	"\u00fc\u00f1\u00ef\u00e7\u00f6d\u00e9",
	// The same glyphs decomposed (NFD): distinct bytes that collide if a backend
	// lets a normalizing filesystem name the object (HFS+ normalizes to NFD).
	// Escaped on both entries because as literals they are indistinguishable in
	// any editor or diff, and tooling that normalized one into the other would
	// silently destroy the only thing the pair tests.
	"u\u0308n\u0303i\u0308c\u0327o\u0308de\u0301",
	// roomname.Valid rejects exactly "." and "..", but accepts this, so a backend
	// using raw names as path segments traverses out of its root.
	"../escape",
}
