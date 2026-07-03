package crdt

// ContentIDs records which item IDs an update (or doc) inserted and deleted.
// It is the yjs-v14 ContentIds (src/utils/meta.js). Not goroutine-safe.
type ContentIDs struct {
	Inserts *IDSet
	Deletes *IDSet
}

// ContentMap attaches attribution metadata to inserted and deleted IDs. It is
// the yjs-v14 ContentMap (formerly TwosetAttributionManager). Not goroutine-safe.
type ContentMap struct {
	Inserts *IDMap
	Deletes *IDMap
}

// contentIDsFromStructs converts a lazy-reader result into ContentIDs.
func contentIDsFromStructs(perClient map[ClientID][]*Item, ds DeleteSet) ContentIDs {
	inserts := NewIDSet()
	for client, items := range perClient {
		for _, it := range items {
			inserts.Add(client, it.ID.Clock, uint64(it.Content.Len()))
		}
	}
	deletes := NewIDSet()
	for client, ranges := range ds.clients {
		for _, r := range ranges {
			deletes.Add(client, r.Clock, r.Len)
		}
	}
	return ContentIDs{Inserts: inserts, Deletes: deletes}
}

// ContentIDsFromUpdateV1 extracts the inserted and deleted item IDs from a V1
// update without integrating it — the entry point for stamping an incoming
// update with attribution (issue #56). Skip structs are placeholders and are
// excluded.
func ContentIDsFromUpdateV1(update []byte) (ContentIDs, error) {
	perClient, ds, err := decodeStructsV1(New(), update)
	if err != nil {
		return ContentIDs{}, err
	}
	return contentIDsFromStructs(perClient, ds), nil
}

// ContentIDsFromUpdateV2 is ContentIDsFromUpdateV1 for V2 updates.
func ContentIDsFromUpdateV2(update []byte) (ContentIDs, error) {
	perClient, ds, err := decodeStructsV2(New(), update)
	if err != nil {
		return ContentIDs{}, err
	}
	return contentIDsFromStructs(perClient, ds), nil
}

// InsertSetFromDoc returns the IDSet of every item run in the doc's store
// (yjs createInsertSetFromStructStore). With filterDeleted, runs of deleted
// items are excluded; consecutive surviving items coalesce into one range.
func InsertSetFromDoc(doc *Doc, filterDeleted bool) *IDSet {
	out := NewIDSet()
	doc.mu.Lock()
	defer doc.mu.Unlock()
	for client, items := range doc.store.clients {
		for i := 0; i < len(items); i++ {
			it := items[i]
			if filterDeleted && it.Deleted {
				continue
			}
			clock := it.ID.Clock
			length := uint64(it.Content.Len())
			for i+1 < len(items) && (!filterDeleted || !items[i+1].Deleted) {
				i++
				length += uint64(items[i].Content.Len())
			}
			out.Add(client, clock, length)
		}
	}
	return out
}

// DeleteSetFromDoc returns the doc's delete set as an IDSet
// (yjs createDeleteSetFromStructStore).
func DeleteSetFromDoc(doc *Doc) *IDSet {
	doc.mu.Lock()
	ds := buildDeleteSet(doc.store)
	doc.mu.Unlock()
	out := NewIDSet()
	for client, ranges := range ds.clients {
		for _, r := range ranges {
			out.Add(client, r.Clock, r.Len)
		}
	}
	return out
}

// CreateContentMapFromContentIDs stamps every insert range with insertAttrs and
// every delete range with deleteAttrs (yjs createContentMapFromContentIds).
// A nil deleteAttrs defaults to insertAttrs, matching the yjs signature default.
func CreateContentMapFromContentIDs(ids ContentIDs, insertAttrs, deleteAttrs []*ContentAttribute) ContentMap {
	if deleteAttrs == nil {
		deleteAttrs = insertAttrs
	}
	return ContentMap{
		Inserts: IDMapFromIDSet(ids.Inserts, insertAttrs),
		Deletes: IDMapFromIDSet(ids.Deletes, deleteAttrs),
	}
}

// MergeContentMaps merges both halves of every input (yjs mergeContentMaps).
func MergeContentMaps(maps ...ContentMap) ContentMap {
	ins := make([]*IDMap, 0, len(maps))
	dels := make([]*IDMap, 0, len(maps))
	for _, m := range maps {
		ins = append(ins, m.Inserts)
		dels = append(dels, m.Deletes)
	}
	return ContentMap{Inserts: MergeIDMaps(ins...), Deletes: MergeIDMaps(dels...)}
}

// ExcludeContentMap removes ids' ranges from both halves (yjs excludeContentMap).
func ExcludeContentMap(m ContentMap, ids ContentIDs) ContentMap {
	return ContentMap{
		Inserts: ExcludeIDMap(m.Inserts, ids.Inserts),
		Deletes: ExcludeIDMap(m.Deletes, ids.Deletes),
	}
}

// IntersectContentMaps intersects both halves (yjs intersectContentMap).
func IntersectContentMaps(a, b ContentMap) ContentMap {
	return ContentMap{
		Inserts: IntersectIDMaps(a.Inserts, b.Inserts),
		Deletes: IntersectIDMaps(a.Deletes, b.Deletes),
	}
}

// FilterContentMap filters both halves by their predicates (yjs filterContentMap).
func FilterContentMap(m ContentMap, insertPred, deletePred func([]*ContentAttribute) bool) ContentMap {
	return ContentMap{
		Inserts: FilterIDMap(m.Inserts, insertPred),
		Deletes: FilterIDMap(m.Deletes, deletePred),
	}
}
