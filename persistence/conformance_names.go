package persistence

// conformanceRoomNames is the room-name set both RunRoomListerConformance and
// RunSnapshotStoreConformance probe, kept in one place so the two suites cannot
// drift apart. They had: the room suite exercised odd names while the snapshot
// suite used the literal "room" for all of its call sites, so no implementation
// was ever checked against a name needing escaping on the snapshot path — where
// IDs are often recovered by parsing them back out of an object name, and a
// name containing the implementation's delimiter silently maps one room's IDs
// onto another's rather than failing loudly (issue #211).
//
// A backend may encode these however it likes on disk, but every operation must
// round-trip the ORIGINAL name and must keep distinct names distinct.
var conformanceRoomNames = []string{
	"plain",
	"with/slash",
	"with:colon",
	"with space",
	// Precomposed (NFC).
	"\u00fc\u00f1\u00ef\u00e7\u00f6d\u00e9",
	// The same glyphs decomposed (NFD): distinct bytes that collide if a backend
	// lets a normalizing filesystem name the object (HFS+ normalizes to NFD),
	// conflating two different rooms. Escapes on both entries deliberately — as
	// literals the two lines are indistinguishable in every editor and diff, and
	// tooling may silently normalize one into the other, destroying the only
	// thing this pair tests.
	"u\u0308n\u0303i\u0308c\u0327o\u0308de\u0301",
	// roomname.Valid rejects exactly "." and "..", but accepts this, so a
	// backend that uses raw names as path segments traverses out of its root.
	"../escape",
}
