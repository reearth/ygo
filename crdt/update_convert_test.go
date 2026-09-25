package crdt

import (
	"encoding/hex"
	"testing"

	"github.com/stretchr/testify/require"
)

// The V1/V2 converters must translate the bytes, not the document state they
// describe: an incremental update names clocks the converter has never seen,
// and a delete-only update names items it does not hold. Expected bytes are
// yjs 13.6.30's convertUpdateFormatV1ToV2 on the identical V1 input (client
// 7: set m.a, then insert "hello", then delete 3 chars).
func TestUnit_UpdateConvert_MatchesYjs(t *testing.T) {
	for _, tc := range []struct {
		name, v1, v2 string
	}{
		{"first update (clock 0)", "010107002801016d01610177013100", "000001070000012805026d614100010100010101010077013100"},
		{"incremental (clock 1)", "01010701040101740568656c6c6f00", "000001070000010409067468656c6c6f01050101000001010100"},
		{"delete-only", "000107010203", "0000000000000100000000000107010202"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			v1, err := hex.DecodeString(tc.v1)
			require.NoError(t, err)
			v2, err := hex.DecodeString(tc.v2)
			require.NoError(t, err)

			gotV2, err := UpdateV1ToV2(v1)
			require.NoError(t, err)
			require.Equal(t, tc.v2, hex.EncodeToString(gotV2), "UpdateV1ToV2")

			gotV1, err := UpdateV2ToV1(v2)
			require.NoError(t, err)
			require.Equal(t, tc.v1, hex.EncodeToString(gotV1), "UpdateV2ToV1")
		})
	}
}
