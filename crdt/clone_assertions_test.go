package crdt

import (
	"fmt"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// cloneKeepsSourceStructs reports whether the applied form a clone holds is
// the source's own structs: the squash merged no run of the source's, and no
// struct lost a parent an update cannot name. Where it does not hold, the
// clone is the encode-and-apply copy of the source rather than a struct-for-
// struct image of it, and the two are compared by their encodings and their
// reads instead.
func cloneKeepsSourceStructs(src, clone *Doc) bool {
	if len(src.store.clients) != len(clone.store.clients) {
		return false
	}
	for client, items := range src.store.clients {
		copies := clone.store.clients[client]
		if len(copies) != len(items) {
			return false
		}
		for i, it := range items {
			if (it.Parent == nil) != (copies[i].Parent == nil) {
				return false
			}
		}
	}
	return true
}

// liveDigest renders what a document reads: every named root that holds
// anything, and under it the live content of each type - its sequence
// elements in list order and its keyed entries by key. Two documents that
// read the same have the same digest, whatever order their lists hold entries
// of different keys in, which integration order decides and nothing reads.
func liveDigest(d *Doc) string {
	var sb strings.Builder
	names := make([]string, 0, len(d.share))
	for name := range d.share {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		t := d.share[name].baseType()
		if t.start == nil && len(t.itemMap) == 0 {
			continue // an empty root, which no update names
		}
		fmt.Fprintf(&sb, "root %q ", name)
		digestType(&sb, t, 0)
		sb.WriteByte('\n')
	}
	return sb.String()
}

// digestType renders one type's live content.
func digestType(sb *strings.Builder, t *abstractType, depth int) {
	if depth > 32 {
		sb.WriteString("{…}")
		return
	}
	fmt.Fprintf(sb, "{len=%d seq=[", t.length)
	for it := t.start; it != nil; it = it.Right {
		if it.Deleted || it.ParentSub != nil {
			continue
		}
		digestItem(sb, it, depth)
		sb.WriteByte(' ')
	}
	sb.WriteString("] keys=[")
	keys := make([]string, 0, len(t.itemMap))
	for key := range t.itemMap {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		it := t.itemMap[key]
		if it == nil || it.Deleted {
			continue
		}
		fmt.Fprintf(sb, "%q=", key)
		digestItem(sb, it, depth)
		sb.WriteByte(' ')
	}
	sb.WriteString("]}")
}

// digestItem renders one live struct's content.
func digestItem(sb *strings.Builder, it *Item, depth int) {
	switch content := it.Content.(type) {
	case *ContentString:
		fmt.Fprintf(sb, "str(%q)", content.Str)
	case *ContentAny:
		fmt.Fprintf(sb, "any(%v)", content.Vals)
	case *ContentJSON:
		fmt.Fprintf(sb, "json(%v)", content.Vals)
	case *ContentEmbed:
		fmt.Fprintf(sb, "embed(%v)", content.Val)
	case *ContentFormat:
		fmt.Fprintf(sb, "format(%s=%v)", content.Key, content.Val)
	case *ContentBinary:
		fmt.Fprintf(sb, "binary(%x)", content.Data)
	case *ContentMove:
		fmt.Fprintf(sb, "move(%s,%d)", idValue(content.Target), content.TargetLen)
	case *ContentDoc:
		guid := ""
		if content.Doc != nil {
			guid = content.Doc.guid
		}
		fmt.Fprintf(sb, "doc(%s)", guid)
	case *ContentType:
		if content.Type == nil {
			sb.WriteString("type(<nil>)")
			return
		}
		fmt.Fprintf(sb, "type(%T)", content.Type.owner)
		digestType(sb, content.Type, depth+1)
	default:
		fmt.Fprintf(sb, "%T", it.Content)
	}
}

// requireCloneIsomorphic checks that clone holds src's structure - every
// struct, every link between structs, every type and its bookkeeping - in
// objects of its own. It is called for the documents whose applied form is
// the source's own structs (cloneKeepsSourceStructs).
func requireCloneIsomorphic(t *testing.T, src, clone *Doc) {
	t.Helper()

	// types pairs each type of the source with the clone's counterpart, found
	// where the two documents are walked in step: a named root, the parent of
	// a struct, and the type a nested-type content holds. A type is reached
	// by more than one of these, and a source type that resolved to two
	// different types in the clone would be a copy that split it in two.
	types := make(map[*abstractType]*abstractType)
	pair := func(s, c *abstractType) {
		if s == nil || c == nil {
			return
		}
		if prev, ok := types[s]; ok {
			require.Same(t, prev, c, "one source type copied as two")
			return
		}
		types[s] = c
	}

	// The named roots: same names, same classes.
	require.Len(t, clone.share, len(src.share), "root types")
	for name, st := range src.share {
		other, ok := clone.share[name]
		require.True(t, ok, "root %q missing from the clone", name)
		require.Equal(t, fmt.Sprintf("%T", st), fmt.Sprintf("%T", other), "root %q class", name)
		pair(st.baseType(), other.baseType())
	}

	// Every struct, in clock order, and the types its parent and its content
	// name.
	require.Len(t, clone.store.clients, len(src.store.clients), "clients")
	cloneStructs := make(map[*Item]bool)
	for client, items := range src.store.clients {
		copies, ok := clone.store.clients[client]
		require.True(t, ok, "client %d missing from the clone", client)
		require.Len(t, copies, len(items), "client %d struct count", client)
		for i, it := range items {
			requireStructCopy(t, it, copies[i], fmt.Sprintf("client %d struct %d", client, i))
			cloneStructs[copies[i]] = true
			pairTypesOf(pair, it, copies[i])
		}
	}

	// Every link of the clone points at a struct of the clone.
	for _, items := range clone.store.clients {
		for _, it := range items {
			for _, link := range []*Item{it.Left, it.Right, it.MovedBy} {
				if link != nil {
					require.True(t, cloneStructs[link], "a clone's link leaves the clone")
				}
			}
		}
	}

	// The parked structs, which hold types of their own until they integrate.
	if src.store.pending == nil {
		require.Nil(t, clone.store.pending)
	} else {
		require.NotNil(t, clone.store.pending)
		require.Len(t, clone.store.pending.items, len(src.store.pending.items))
		for i, it := range src.store.pending.items {
			cp := clone.store.pending.items[i]
			if it == nil {
				require.Nil(t, cp)
				continue
			}
			requireStructCopy(t, it, cp, fmt.Sprintf("parked struct %d", i))
			pairTypesOf(pair, it, cp)
		}
		require.Equal(t, src.store.pending.missing, clone.store.pending.missing)
	}

	// Every type the walk paired: its own object, on the clone, holding the
	// source type's list, key map and counters.
	for st, cp := range types {
		require.NotSame(t, st, cp, "the clone shares the source's type")
		require.Same(t, clone, cp.doc, "a clone's type belongs to the clone")
		requireTypeCopy(t, st, cp, typeKey(st))
	}

	// The parked delete ranges and the deferred moves.
	require.Equal(t, src.store.pendingDsRanges, clone.store.pendingDsRanges)
	require.Equal(t, src.store.pendingDs.clients, clone.store.pendingDs.clients)
	require.Equal(t, src.store.pendingDsMoved, clone.store.pendingDsMoved)
	require.Len(t, clone.store.pendingMoves, len(src.store.pendingMoves))
	for client, moves := range src.store.pendingMoves {
		require.Len(t, clone.store.pendingMoves[client], len(moves))
		for i, move := range moves {
			require.Equal(t, move.clock, clone.store.pendingMoves[client][i].clock)
			requireStructCopy(t, move.move, clone.store.pendingMoves[client][i].move, "deferred move")
		}
	}
}

// requireCloneDisjoint checks that a clone shares no object its documents
// write through with its source: no struct, no type, no content and no
// subdocument. Strings, ids and the values inside a plain-value content are
// shared on purpose, and are not objects ygo writes through (see Clone).
func requireCloneDisjoint(t *testing.T, src, clone *Doc) {
	t.Helper()
	srcItems, srcTypes, srcContents, srcDocs := docObjects(src)
	cloneItems, cloneTypes, cloneContents, cloneDocs := docObjects(clone)
	for it := range cloneItems {
		require.False(t, srcItems[it], "the clone holds a struct of the source")
	}
	for ty := range cloneTypes {
		require.False(t, srcTypes[ty], "the clone holds a type of the source")
		require.Same(t, clone, ty.doc, "a clone's type belongs to the clone")
	}
	for c := range cloneContents {
		require.False(t, srcContents[c], "the clone holds a content of the source")
	}
	for d := range cloneDocs {
		require.False(t, srcDocs[d], "the clone holds a subdocument of the source")
	}
}

// docObjects returns every struct, type, content and subdocument a document
// holds, integrated or parked.
func docObjects(d *Doc) (map[*Item]bool, map[*abstractType]bool, map[Content]bool, map[*Doc]bool) {
	items := make(map[*Item]bool)
	types := make(map[*abstractType]bool)
	contents := make(map[Content]bool)
	docs := make(map[*Doc]bool)
	for _, st := range d.share {
		types[st.baseType()] = true
	}
	visit := func(list []*Item) {
		for _, it := range list {
			if it == nil {
				continue
			}
			items[it] = true
			contents[it.Content] = true
			if it.Parent != nil {
				types[it.Parent] = true
			}
			switch content := it.Content.(type) {
			case *ContentType:
				if content.Type != nil {
					types[content.Type] = true
				}
			case *ContentDoc:
				if content.Doc != nil {
					docs[content.Doc] = true
				}
			}
		}
	}
	for _, list := range d.store.clients {
		visit(list)
	}
	if d.store.pending != nil {
		visit(d.store.pending.items)
	}
	return items, types, contents, docs
}

// pairTypesOf pairs the types a struct and its copy name: the type they are
// parented to, and the nested type their content holds.
func pairTypesOf(pair func(s, c *abstractType), it, cp *Item) {
	pair(it.Parent, cp.Parent)
	if ct, ok := it.Content.(*ContentType); ok {
		if cct, ok := cp.Content.(*ContentType); ok {
			pair(ct.Type, cct.Type)
		}
	}
}

// requireStructCopy checks that cp is it's copy: the same struct in its own
// object, with every link pointing at the copy of what it pointed at.
func requireStructCopy(t *testing.T, it, cp *Item, where string) {
	t.Helper()
	require.NotSame(t, it, cp, "%s: the clone shares the source's struct", where)
	require.Equal(t, it.ID, cp.ID, "%s: id", where)
	require.Equal(t, it.Deleted, cp.Deleted, "%s: deleted", where)
	require.Equal(t, idValue(it.Origin), idValue(cp.Origin), "%s: origin", where)
	require.Equal(t, idValue(it.OriginRight), idValue(cp.OriginRight), "%s: right origin", where)
	require.Equal(t, idValue(it.parentID), idValue(cp.parentID), "%s: parent id", where)
	require.Equal(t, idValue(it.redone), idValue(cp.redone), "%s: redone", where)
	require.Equal(t, parentSubValue(it.ParentSub), parentSubValue(cp.ParentSub), "%s: parent key", where)
	require.Equal(t, itemKey(it.Left), itemKey(cp.Left), "%s: left", where)
	require.Equal(t, itemKey(it.Right), itemKey(cp.Right), "%s: right", where)
	require.Equal(t, itemKey(it.MovedBy), itemKey(cp.MovedBy), "%s: moved by", where)
	require.Equal(t, typeKey(it.Parent), typeKey(cp.Parent), "%s: parent", where)
	requireContentCopy(t, it.Content, cp.Content, where)
}

// requireContentCopy checks that cp holds what c holds, in its own object.
func requireContentCopy(t *testing.T, c, cp Content, where string) {
	t.Helper()
	require.Equal(t, fmt.Sprintf("%T", c), fmt.Sprintf("%T", cp), "%s: content class", where)
	require.NotSame(t, c, cp, "%s: the clone shares the source's content", where)
	require.Equal(t, c.Len(), cp.Len(), "%s: content length", where)
	switch v := c.(type) {
	case *ContentString:
		require.Equal(t, v.Str, cp.(*ContentString).Str, "%s: string", where)
	case *ContentBinary:
		other := cp.(*ContentBinary)
		require.Equal(t, v.Data, other.Data, "%s: bytes", where)
		if len(v.Data) > 0 {
			require.NotSame(t, &v.Data[0], &other.Data[0], "%s: shared byte array", where)
		}
	case *ContentAny:
		other := cp.(*ContentAny)
		require.Equal(t, v.Vals, other.Vals, "%s: values", where)
		if len(v.Vals) > 0 {
			require.NotSame(t, &v.Vals[0], &other.Vals[0], "%s: shared value slice", where)
		}
	case *ContentJSON:
		other := cp.(*ContentJSON)
		require.Equal(t, v.Vals, other.Vals, "%s: values", where)
		if len(v.Vals) > 0 {
			require.NotSame(t, &v.Vals[0], &other.Vals[0], "%s: shared value slice", where)
		}
	case *ContentEmbed:
		require.Equal(t, v.Val, cp.(*ContentEmbed).Val, "%s: embed", where)
	case *ContentFormat:
		other := cp.(*ContentFormat)
		require.Equal(t, v.Key, other.Key, "%s: format key", where)
		require.Equal(t, v.Val, other.Val, "%s: format value", where)
	case *ContentMove:
		other := cp.(*ContentMove)
		require.Equal(t, idValue(v.Target), idValue(other.Target), "%s: move target", where)
		require.Equal(t, v.TargetLen, other.TargetLen, "%s: move target length", where)
	case *ContentType:
		other := cp.(*ContentType)
		require.Equal(t, typeKey(v.Type), typeKey(other.Type), "%s: nested type", where)
		require.NotSame(t, v.Type, other.Type, "%s: shared nested type", where)
		require.Equal(t, fmt.Sprintf("%T", v.Type.owner), fmt.Sprintf("%T", other.Type.owner),
			"%s: nested type class", where)
	case *ContentDoc:
		other := cp.(*ContentDoc)
		require.NotSame(t, v.Doc, other.Doc, "%s: shared subdocument", where)
		require.Equal(t, v.Doc.guid, other.Doc.guid, "%s: subdocument guid", where)
		require.Equal(t, v.Doc.gc, other.Doc.gc, "%s: subdocument gc", where)
		require.Equal(t, v.Doc.autoLoad, other.Doc.autoLoad, "%s: subdocument autoload", where)
		require.Equal(t, v.Doc.meta, other.Doc.meta, "%s: subdocument meta", where)
		wantCollection := ""
		if other.Doc.parent != nil {
			wantCollection = other.Doc.parent.CollectionID()
		}
		require.Equal(t, wantCollection, other.Doc.CollectionID(), "%s: subdocument inherits clone collection", where)
	}
}

// requireTypeCopy checks a type's own state: its list, its key map, its
// container item and its counters.
func requireTypeCopy(t *testing.T, st, cp *abstractType, where string) {
	t.Helper()
	require.NotSame(t, st, cp, "%s: the clone shares the source's type", where)
	require.Equal(t, st.name, cp.name, "%s: name", where)
	require.Equal(t, st.length, cp.length, "%s: length", where)
	require.Equal(t, itemKey(st.item), itemKey(cp.item), "%s: container item", where)
	require.Equal(t, listKeys(st), listKeys(cp), "%s: list", where)
	require.Equal(t, itemMapKeys(st), itemMapKeys(cp), "%s: key map", where)
	require.Equal(t, st.hasFormatting, cp.hasFormatting, "%s: has formatting", where)
	require.Equal(t, st.cleanFormatting, cp.cleanFormatting, "%s: clean formatting", where)
	if e, ok := st.owner.(*YXmlElement); ok {
		require.Equal(t, e.NodeName, cp.owner.(*YXmlElement).NodeName, "%s: node name", where)
	}
}

// listKeys renders a type's linked list as the ids on it, in order.
func listKeys(t *abstractType) []string {
	var out []string
	for item := t.start; item != nil; item = item.Right {
		out = append(out, itemKey(item))
	}
	return out
}

// itemMapKeys renders a type's key map as key=id pairs, sorted.
func itemMapKeys(t *abstractType) []string {
	out := make([]string, 0, len(t.itemMap))
	for key, item := range t.itemMap {
		out = append(out, key+"="+itemKey(item))
	}
	sort.Strings(out)
	return out
}

// itemKey identifies a struct by id, for comparing two documents' links.
func itemKey(it *Item) string {
	if it == nil {
		return "<nil>"
	}
	return fmt.Sprintf("%d:%d", it.ID.Client, it.ID.Clock)
}

// typeKey identifies a type by its root name or by its container item's id.
func typeKey(t *abstractType) string {
	switch {
	case t == nil:
		return "<nil>"
	case t.item != nil:
		return "under " + itemKey(t.item)
	default:
		return "root " + t.name
	}
}

// idValue renders an optional id for comparison.
func idValue(id *ID) string {
	if id == nil {
		return "<nil>"
	}
	return fmt.Sprintf("%d:%d", id.Client, id.Clock)
}

// parentSubValue renders an optional map key for comparison: a nil key (a
// sequence element) is not the empty-string key.
func parentSubValue(s *string) string {
	if s == nil {
		return "<nil>"
	}
	return "=" + *s
}

// deleteSetKey renders DeleteSetFromDoc as a comparable string.
func deleteSetKey(d *Doc) string {
	ds := DeleteSetFromDoc(d)
	clients := ds.Clients()
	sort.Slice(clients, func(i, j int) bool { return clients[i] < clients[j] })
	var sb strings.Builder
	for _, c := range clients {
		fmt.Fprintf(&sb, "%d:", c)
		for _, r := range ds.Ranges(c) {
			fmt.Fprintf(&sb, "%d+%d,", r.Clock, r.Len)
		}
		sb.WriteByte(';')
	}
	return sb.String()
}
