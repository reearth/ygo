package websocket

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestUnit_Peer_WithinBudget(t *testing.T) {
	tests := []struct {
		name   string
		budget int64
		armed  bool
		queued int64
		frame  int
		want   bool
	}{
		{
			name:   "the handshake is taken whatever its size",
			budget: 100, armed: false, queued: 1_000, frame: 1_000, want: true,
		},
		{
			name:   "a frame larger than the budget is taken on an empty queue",
			budget: 100, armed: true, queued: 0, frame: 1_000, want: true,
		},
		{
			name:   "the frame after an over-budget one is refused",
			budget: 100, armed: true, queued: 1_000, frame: 1, want: false,
		},
		{
			name:   "a frame that reaches the budget exactly is taken",
			budget: 100, armed: true, queued: 40, frame: 60, want: true,
		},
		{
			name:   "one byte past the budget is refused",
			budget: 100, armed: true, queued: 40, frame: 61, want: false,
		},
		{
			name:   "budget 0 refuses nothing",
			budget: 0, armed: true, queued: 1 << 40, frame: 1 << 20, want: true,
		},
		{
			name:   "a negative budget is off, like 0",
			budget: -1, armed: true, queued: 1 << 40, frame: 1 << 20, want: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := &peer{writeBudget: tt.budget, budgetArmed: tt.armed, queuedBytes: tt.queued}
			assert.Equal(t, tt.want, p.withinBudget(tt.frame))
		})
	}
}

func TestUnit_Peer_Write_ChargesQueuedBytes(t *testing.T) {
	queued := func(p *peer) int64 {
		p.wmu.Lock()
		defer p.wmu.Unlock()
		return p.queuedBytes
	}

	p := &peer{server: &Server{}, writeCh: make(chan []byte, 2), writeBudget: 1 << 20}
	p.write([]byte{1, 2, 3})
	p.write([]byte{4, 5})
	assert.Equal(t, int64(5), queued(p), "each frame is charged its own length")
	assert.Len(t, p.writeCh, 2)

	p.wmu.Lock()
	p.closed = true
	p.wmu.Unlock()
	p.write([]byte{6, 7, 8, 9})
	assert.Equal(t, int64(5), queued(p), "a write to a closed peer is not queued or charged")

	off := &peer{server: &Server{}, writeCh: make(chan []byte, 1)}
	off.write([]byte{1, 2, 3})
	assert.Equal(t, int64(0), queued(off), "with no budget nothing is charged")
	assert.Len(t, off.writeCh, 1)
}
