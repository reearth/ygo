package crdt

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"maps"
	"os"
	"path/filepath"
	"testing"
)

type svFromUpdateFixture struct {
	Name string `json:"name"`
	V1   string `json:"v1"`
	V2   string `json:"v2"`
	SV   string `json:"sv"`
	// From and To are what yjs parseUpdateMeta returns for the update.
	From StateVector `json:"from"`
	To   StateVector `json:"to"`
}

func svFromUpdateFixtures(t *testing.T) []svFromUpdateFixture {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", "svfromupdate_yjs_fixtures.json"))
	if err != nil {
		t.Fatal(err)
	}
	var fixtures []svFromUpdateFixture
	if err := json.Unmarshal(raw, &fixtures); err != nil {
		t.Fatal(err)
	}
	if len(fixtures) == 0 {
		t.Fatal("no fixtures")
	}
	return fixtures
}

// yjs counts each client from clock 0 up to its first Skip and keeps the
// update's client order; ygo must write the same bytes.
func TestConformance_StateVectorFromUpdate_MatchesYjs(t *testing.T) {
	fixtures := svFromUpdateFixtures(t)
	for _, fx := range fixtures {
		for _, ver := range []struct {
			tag    string
			update string
			encode func([]byte) ([]byte, error)
		}{
			{"v1", fx.V1, EncodeStateVectorFromUpdate},
			{"v2", fx.V2, EncodeStateVectorFromUpdateV2},
		} {
			t.Run(fx.Name+"/"+ver.tag, func(t *testing.T) {
				update, err := hex.DecodeString(ver.update)
				if err != nil {
					t.Fatal(err)
				}
				got, err := ver.encode(update)
				if err != nil {
					t.Fatal(err)
				}
				if hex.EncodeToString(got) != fx.SV {
					t.Errorf("state vector = %x, want %s", got, fx.SV)
				}
			})
		}
	}
}

// yjs parseUpdateMeta reads every struct, Skips included, and gives each
// client the clock its first struct starts at and the clock past its last;
// ParseUpdateMeta and ParseUpdateMetaV2 must give the same maps.
func TestConformance_ParseUpdateMeta_MatchesYjs(t *testing.T) {
	for _, fx := range svFromUpdateFixtures(t) {
		for _, ver := range []struct {
			tag    string
			update string
			parse  func([]byte) (StateVector, StateVector, error)
		}{
			{"v1", fx.V1, ParseUpdateMeta},
			{"v2", fx.V2, ParseUpdateMetaV2},
		} {
			t.Run(fx.Name+"/"+ver.tag, func(t *testing.T) {
				update, err := hex.DecodeString(ver.update)
				if err != nil {
					t.Fatal(err)
				}
				from, to, err := ver.parse(update)
				if err != nil {
					t.Fatal(err)
				}
				if !maps.Equal(from, fx.From) || !maps.Equal(to, fx.To) {
					t.Errorf("from %v to %v, want from %v to %v", from, to, fx.From, fx.To)
				}
			})
		}
	}
}

// The state vector reader keeps the decoders' bound on the structs an update
// declares, so 23 V2 bytes whose run-length columns declare 1,048,577 GC
// structs are refused rather than walked.
func TestUnit_StateVectorFromUpdate_RefusesMoreStructsThanTheDecodersRead(t *testing.T) {
	update, err := hex.DecodeString("0000010100000100010000000440ffff3f018180400000")
	if err != nil {
		t.Fatal(err)
	}
	if sv, err := EncodeStateVectorFromUpdateV2(update); !errors.Is(err, ErrInvalidUpdate) {
		t.Fatalf("state vector %x, err %v; want ErrInvalidUpdate", sv, err)
	}
}
