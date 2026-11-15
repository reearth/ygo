package crdt_test

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/reearth/ygo/crdt"
)

type seed42Step struct {
	Op    string          `json:"op"`
	Peer  int             `json:"peer"`
	To    int             `json:"to"`
	Pos   int             `json:"pos"`
	N     int             `json:"n"`
	Text  string          `json:"text"`
	Attrs crdt.Attributes `json:"attrs"`
}

type seed42Scenario struct {
	Steps []seed42Step `json:"steps"`
}
type seed42Char struct {
	Text  string          `json:"text"`
	Attrs crdt.Attributes `json:"attrs"`
}

// TestCompat_Seed42Formatting compares the original minimized scenario with
// Yjs, including a nested XML text node used by rich-text editors.
func TestCompat_Seed42Formatting(t *testing.T) {
	node, dir := requireConformance(t, "yjs")
	input, err := os.ReadFile(filepath.Join(dir, "fixtures", "seed42_formatting.json"))
	require.NoError(t, err)
	var scenarios []seed42Scenario
	require.NoError(t, json.Unmarshal(input, &scenarios))
	require.Len(t, scenarios, 1)
	for _, mode := range []string{"text", "xml"} {
		t.Run(mode, func(t *testing.T) {
			args := []string{filepath.Join(dir, "seed42_formatting.cjs")}
			if mode == "xml" {
				args = append(args, "xml")
			}
			cmd := exec.Command(node, args...)
			cmd.Dir, cmd.Stdin = dir, bytes.NewReader(input)
			output, err := cmd.Output()
			require.NoError(t, err)
			var expected [][][]seed42Char
			require.NoError(t, json.Unmarshal(output, &expected))
			require.Len(t, expected, 1)
			require.Equal(t, expected[0], replaySeed42(t, scenarios[0], mode == "xml"))
		})
	}
}

func replaySeed42(t *testing.T, scenario seed42Scenario, xml bool) [][]seed42Char {
	t.Helper()
	var base []byte
	if xml {
		seed := crdt.New(crdt.WithClientID(100))
		fragment := seed.GetXmlFragment("prosemirror")
		paragraph, text := crdt.NewYXmlElement("paragraph"), crdt.NewYXmlText()
		seed.Transact(func(txn *crdt.Transaction) {
			fragment.InsertElement(txn, 0, paragraph)
			paragraph.InsertText(txn, 0, text)
		})
		base = crdt.EncodeStateAsUpdateV1(seed, nil)
	}
	docs, texts := make([]*crdt.Doc, 3), make([]*crdt.YText, 3)
	for i := range docs {
		docs[i] = crdt.New(crdt.WithClientID(crdt.ClientID(i + 1)))
		if xml {
			fragment := docs[i].GetXmlFragment("prosemirror")
			require.NoError(t, crdt.ApplyUpdateV1(docs[i], base, nil))
			paragraph := fragment.Children()[0].(*crdt.YXmlElement)
			texts[i] = &paragraph.Children()[0].(*crdt.YXmlText).YText
		} else {
			texts[i] = docs[i].GetText("t")
		}
	}
	sync := func(a, b int) {
		require.NoError(t, crdt.ApplyUpdateV1(docs[b], crdt.EncodeStateAsUpdateV1(docs[a], nil), nil))
	}
	for _, step := range scenario.Steps {
		if step.Op == "sync" {
			sync(step.Peer, step.To)
			continue
		}
		text := texts[step.Peer]
		pos := min(step.Pos, text.Len())
		n := min(step.N, text.Len()-pos)
		attrs := step.Attrs
		if attrs == nil {
			attrs = crdt.Attributes{}
		}
		docs[step.Peer].Transact(func(txn *crdt.Transaction) {
			switch step.Op {
			case "insert":
				text.Insert(txn, pos, step.Text, attrs)
			case "delete":
				if n > 0 {
					text.Delete(txn, pos, n)
				}
			case "format":
				if n > 0 {
					text.Format(txn, pos, n, attrs)
				}
			}
		})
	}
	for range 2 {
		for a := range docs {
			for b := range docs {
				if a != b {
					sync(a, b)
				}
			}
		}
	}
	peers := make([][]seed42Char, 3)
	for i, text := range texts {
		peers[i] = []seed42Char{}
		for _, delta := range text.ToDelta() {
			attrs := delta.Attributes
			if attrs == nil {
				attrs = crdt.Attributes{}
			}
			for _, r := range delta.Insert.(string) {
				peers[i] = append(peers[i], seed42Char{string(r), attrs})
			}
		}
	}
	return peers
}
