package crdt

import (
	"fmt"
	"math/rand"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/reearth/ygo/encoding"
)

// Every group except a client's last is superseded, across bitset words and
// for ordered, reversed and shuffled wires. Groups carry every content kind,
// so a framing slip would misread the following group headers.
func TestUnit_SupersededGroups_MatchLastGroupPerClient(t *testing.T) {
	source := New()
	defer source.Destroy()
	subdoc := New()
	defer subdoc.Destroy()
	contents := []Content{
		NewContentString("А🐷z"), NewContentDeleted(3), NewContentJSON("a", nil, 7), NewContentBinary([]byte{1, 2}),
		NewContentEmbed(map[string]any{"n": []any{1, true}}), NewContentFormat("bold", true),
		NewContentType(&NewMapPrelim().abstractType), NewContentType(&NewYXmlElement("div").abstractType),
		NewContentAny([]any{1, "a"}, map[string]any{"key": 2}), NewContentDoc(subdoc), NewContentMove(&ID{Client: 9}, 2),
	}
	parent := ID{Client: 999}
	rng := rand.New(rand.NewSource(305))
	for _, order := range []string{"ascending", "descending", "repeats", "shuffled"} {
		for _, version := range []int{1, 2} {
			t.Run(fmt.Sprintf("%s/V%d", order, version), func(t *testing.T) {
				clients := make([]ClientID, 0, 200)
				for i := 0; i < 200; i++ {
					switch order {
					case "ascending":
						clients = append(clients, ClientID(i+1))
					case "descending":
						clients = append(clients, ClientID(200-i))
					default:
						clients = append(clients, ClientID(1+rng.Intn(80)))
					}
				}
				if order == "shuffled" {
					rng.Shuffle(len(clients), func(i, j int) { clients[i], clients[j] = clients[j], clients[i] })
				}
				groups := make([][]*Item, len(clients))
				for i, c := range clients {
					var clock uint64
					for j := 0; j < 3; j++ {
						key := fmt.Sprint("k", (i+j)%5)
						content := contents[(i*3+j)%len(contents)]
						groups[i] = append(groups[i], &Item{ID: ID{Client: c, Clock: clock}, parentID: &parent, ParentSub: &key, Content: content})
						clock += uint64(content.Len())
					}
				}
				update := pendingGuardUpdate(source, groups, nil, version)
				var v2 *v2Decoder
				var rest encoding.Decoder
				if version == 2 {
					var err error
					v2, err = newV2Decoder(update)
					require.NoError(t, err)
					rest = *v2.restDec
				}
				set, err := supersededGroups(update, v2, rest)
				require.NoError(t, err)
				last := map[ClientID]int{}
				for i, c := range clients {
					last[c] = i
				}
				for i, c := range clients {
					require.Equal(t, last[c] != i, set.has(uint64(i)), "group %d client %d", i, c)
				}
				if order == "ascending" || order == "descending" {
					require.Nil(t, set, "a strictly ordered wire needs no index")
				}
			})
		}
	}
}

// repeatedGroupsUpdate encodes client 9's "abc", then client 9 again with only
// "a", then, if malformed, a group whose struct has an unknown content tag.
func repeatedGroupsUpdate(t *testing.T, version int, malformed bool) []byte {
	t.Helper()
	src := New(WithClientID(9))
	defer src.Destroy()
	text := src.GetText("t")
	for _, s := range []string{"a", "b", "c"} {
		src.Transact(func(txn *Transaction) { text.Insert(txn, text.Len(), s, nil) })
	}
	items := src.store.clients[9]
	groups := uint64(2)
	if malformed {
		groups = 3
	}
	if version == 1 {
		enc := encoding.NewEncoder()
		enc.WriteVarUint(groups)
		enc.WriteVarUint(uint64(len(items)))
		enc.WriteVarUint(9)
		enc.WriteVarUint(0)
		for _, it := range items {
			encodeItem(enc, it, 0, src.store)
		}
		enc.WriteVarUint(1)
		enc.WriteVarUint(9)
		enc.WriteVarUint(0)
		encodeItem(enc, items[0], 0, src.store)
		if malformed {
			enc.WriteVarUint(1)
			enc.WriteVarUint(10)
			enc.WriteVarUint(0)
			enc.WriteUint8(0x1f)
			enc.WriteUint8(1)
			enc.WriteVarString("t")
		}
		encodeDeleteSet(enc, newDeleteSet())
		return enc.Bytes()
	}
	e2 := newV2Encoder()
	e2.restEnc.WriteVarUint(groups)
	e2.restEnc.WriteVarUint(uint64(len(items)))
	e2.writeClient(9)
	e2.restEnc.WriteVarUint(0)
	for _, it := range items {
		encodeItemV2(e2, it, 0, src.store)
	}
	e2.restEnc.WriteVarUint(1)
	e2.writeClient(9)
	e2.restEnc.WriteVarUint(0)
	encodeItemV2(e2, items[0], 0, src.store)
	if malformed {
		e2.restEnc.WriteVarUint(1)
		e2.writeClient(10)
		e2.restEnc.WriteVarUint(0)
		e2.writeInfo(0x1f)
		e2.writeParentInfo(true)
		e2.writeString("t")
	}
	encodeDeleteSetV2(e2, newDeleteSet())
	return e2.toBytes()
}

func applyVersion(doc *Doc, version int, update []byte) error {
	if version == 1 {
		return ApplyUpdateV1(doc, update, nil)
	}
	return ApplyUpdateV2(doc, update, nil)
}

func TestUnit_SupersededGroups_TwoGroupsKeepLast(t *testing.T) {
	for _, version := range []int{1, 2} {
		doc := New()
		require.NoError(t, applyVersion(doc, version, repeatedGroupsUpdate(t, version, false)))
		require.Equal(t, "a", doc.GetText("t").ToString(), "V%d", version)
		require.Equal(t, uint64(1), doc.StateVector().Clock(9), "V%d", version)
		doc.Destroy()
	}
}

// A malformed later group must reject the update before any group integrates,
// or the superseded groups would already be applied.
func TestUnit_SupersededGroups_MalformedLaterGroupAppliesNothing(t *testing.T) {
	for _, version := range []int{1, 2} {
		doc := New()
		require.Error(t, applyVersion(doc, version, repeatedGroupsUpdate(t, version, true)), "V%d", version)
		require.Empty(t, doc.GetText("t").ToString(), "V%d", version)
		require.Empty(t, doc.StateVector(), "V%d", version)
		require.Empty(t, doc.store.clients[9], "V%d", version)
		doc.Destroy()
	}
}

func TestUnit_SupersededGroups_StateVectorFromUpdate(t *testing.T) {
	for _, version := range []int{1, 2} {
		update := repeatedGroupsUpdate(t, version, false)
		encode := EncodeStateVectorFromUpdate
		if version == 2 {
			encode = EncodeStateVectorFromUpdateV2
		}
		raw, err := encode(update)
		require.NoError(t, err)
		sv, err := DecodeStateVectorV1(raw)
		require.NoError(t, err)
		require.Equal(t, StateVector{9: 1}, sv, "V%d", version)
	}
}
