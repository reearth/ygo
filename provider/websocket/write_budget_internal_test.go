package websocket

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestPeer_WithinBudget_Boundary(t *testing.T) {
	// The exact boundary of Server.PeerWriteQueueBytes, which the integration
	// tests can only approach: withinBudget is the whole decision, and the
	// enqueue that follows it is the frame it was asked about.
	tests := []struct {
		name   string
		budget int64
		queued int64
		frame  int
		want   bool
	}{
		{
			// The documented rule at the boundary: a peer holding nothing
			// takes the next frame whatever its size, so a room whose full
			// state is larger than the budget still answers a SyncStep1 and
			// no peer is closed for a frame it could not have avoided.
			name:   "a frame larger than the budget is taken on an empty queue",
			budget: 100, queued: 0, frame: 1_000, want: true,
		},
		{
			// And the price of that rule: the peer is over budget, so the
			// next frame closes it. One peer holds budget + one frame.
			name:   "the frame after an over-budget one is refused",
			budget: 100, queued: 1_000, frame: 1, want: false,
		},
		{
			name:   "a frame that reaches the budget exactly is taken",
			budget: 100, queued: 40, frame: 60, want: true,
		},
		{
			name:   "one byte past the budget is refused",
			budget: 100, queued: 40, frame: 61, want: false,
		},
		{
			name:   "budget 0 refuses nothing",
			budget: 0, queued: 1 << 40, frame: 1 << 20, want: true,
		},
		{
			name:   "a negative budget is off, like 0",
			budget: -1, queued: 1 << 40, frame: 1 << 20, want: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := &peer{writeBudget: tt.budget, queuedBytes: tt.queued}
			assert.Equal(t, tt.want, p.withinBudget(tt.frame))
		})
	}
}

func TestPeer_Write_ChargesAndCreditsQueuedBytes(t *testing.T) {
	// The debit is the frame's own length, and only an enqueue that happened
	// charges it: a write to a peer already marked closed charges nothing.
	p := &peer{server: &Server{}, roomName: "r", writeCh: make(chan []byte, 2), writeBudget: 1 << 20}

	p.write([]byte{1, 2, 3})
	p.write([]byte{4, 5})
	p.wmu.Lock()
	queued := p.queuedBytes
	p.wmu.Unlock()
	assert.Equal(t, int64(5), queued, "both frames are charged, by their own lengths")
	assert.Len(t, p.writeCh, 2, "and both are queued")

	p.wmu.Lock()
	p.closed = true
	p.wmu.Unlock()
	p.write([]byte{6, 7, 8, 9})
	p.wmu.Lock()
	queued = p.queuedBytes
	p.wmu.Unlock()
	assert.Equal(t, int64(5), queued, "a frame a closed peer never queues is never charged")
}
