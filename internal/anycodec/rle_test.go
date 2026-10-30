package anycodec_test

import (
	"testing"

	"github.com/reearth/ygo/encoding"
	"github.com/reearth/ygo/internal/anycodec"
)

// ReadUnits steps over each string as Read does and counts the UTF-16 units
// Read returns, also when the lengths column disagrees with the pool.
func TestUnit_StringDecoder_ReadUnitsCountsWhatReadDecodes(t *testing.T) {
	for _, tc := range []struct {
		name string
		pool string
		lens []uint64
	}{
		{"exact", "héllo😀z", []uint64{1, 1, 3, 2, 1}},
		{"split supplementary character", "a😀bc", []uint64{2, 1, 1}},
		{"short pool", "ab", []uint64{10, 3}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			enc := encoding.NewEncoder()
			enc.WriteVarString(tc.pool)
			var lens encoding.UintOptRleEncoder
			for _, l := range tc.lens {
				lens.Write(l)
			}
			read, err := anycodec.NewStringDecoder(append(enc.Bytes(), lens.Bytes()...))
			if err != nil {
				t.Fatal(err)
			}
			units := read
			// The last string is read by both, to check that they end at the same place.
			for i := range len(tc.lens) - 1 {
				s, err := read.Read()
				if err != nil {
					t.Fatal(err)
				}
				n, err := units.ReadUnits()
				if err != nil {
					t.Fatal(err)
				}
				var want uint64
				for _, r := range s {
					want++
					if r > 0xffff {
						want++
					}
				}
				if n != want {
					t.Fatalf("string %d: ReadUnits gave %d units, Read %q of %d", i, n, s, want)
				}
			}
			want, _ := read.Read()
			if got, _ := units.Read(); got != want {
				t.Fatalf("after ReadUnits the last string is %q, want %q", got, want)
			}
		})
	}
}
