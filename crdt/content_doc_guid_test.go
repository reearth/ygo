package crdt

import (
	"errors"
	"testing"

	"github.com/reearth/ygo/encoding"
)

// A V1 subdocument struct whose guid is not UTF-8 is refused with an error by
// every function that decodes it, where WithGUID panicked.
func TestUnit_ContentDoc_AGuidThatIsNotUTF8IsAnError(t *testing.T) {
	enc := encoding.NewEncoder()
	enc.WriteVarUint(1)
	enc.WriteVarUint(1)
	enc.WriteVarUint(5)
	enc.WriteVarUint(0)
	enc.WriteUint8(wireDoc)
	enc.WriteUint8(1)
	enc.WriteVarString("a")
	enc.WriteVarBytes([]byte{0xff, 0xfe})
	enc.WriteAny(map[string]any{})
	enc.WriteVarUint(0)
	update := enc.Bytes()

	for name, decode := range map[string]func() error{
		"ApplyUpdateV1": func() error { return ApplyUpdateV1(New(), update, nil) },
		"EncodeStateVectorFromUpdate": func() error {
			_, err := EncodeStateVectorFromUpdate(update)
			return err
		},
		"UpdateV1ToV2": func() error {
			_, err := UpdateV1ToV2(update)
			return err
		},
		"MergeUpdatesV1": func() error {
			_, err := MergeUpdatesV1(update)
			return err
		},
	} {
		t.Run(name, func(t *testing.T) {
			defer func() {
				if r := recover(); r != nil {
					t.Fatalf("panicked: %v", r)
				}
			}()
			if err := decode(); !errors.Is(err, ErrInvalidUpdate) {
				t.Fatalf("err %v, want ErrInvalidUpdate for the guid", err)
			}
		})
	}
}
