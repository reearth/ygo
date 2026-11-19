package websocket

import (
	"bytes"
	"testing"

	"github.com/reearth/ygo/encoding"
	ygsync "github.com/reearth/ygo/sync"
)

// The relay frame of an update is the frame ygsync.EncodeUpdate and an
// encoder built, at every length of varint, and it is built in one
// allocation of its exact length (none for a frame small enough for the
// compiler to keep on the stack).
func TestUnit_SyncUpdateFrame_IsTheEncodersFrameBuiltOnce(t *testing.T) {
	for _, n := range []int{0, 1, 127, 128, 16383, 16384, 1 << 21} {
		update := bytes.Repeat([]byte{7}, n)
		want := encoding.EncodeBytes(func(enc *encoding.Encoder) {
			enc.WriteVarUint(msgSync)
			enc.WriteRaw(ygsync.EncodeUpdate(update))
		})
		got := syncUpdateFrame(update)
		if !bytes.Equal(got, want) {
			t.Fatalf("a %d-byte update's frame is %d bytes unlike the encoders' %d", n, len(got), len(want))
		}
		if len(got) != cap(got) {
			t.Fatalf("a %d-byte update's frame has capacity %d for %d bytes", n, cap(got), len(got))
		}
		if allocs := testing.AllocsPerRun(10, func() { syncUpdateFrame(update) }); allocs > 1 {
			t.Fatalf("a %d-byte update's frame took %v allocations, want 1", n, allocs)
		}
	}
}
