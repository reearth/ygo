package websocket

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestUnit_Peer_OverBudget(t *testing.T) {
	tests := []struct {
		name        string
		budget      int64
		handshaking bool
		queued      int64
		want        bool
	}{
		{name: "below the budget", budget: 100, queued: 99, want: false},
		{name: "at the budget", budget: 100, queued: 100, want: true},
		{name: "past the budget", budget: 100, queued: 1_000, want: true},
		{name: "during the handshake", budget: 100, handshaking: true, queued: 1_000, want: false},
		{name: "budget 0 is off", budget: 0, queued: 1 << 40, want: false},
		{name: "a negative budget is off", budget: -1, queued: 1 << 40, want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := &peer{writeBudget: tt.budget, handshaking: tt.handshaking, queuedBytes: tt.queued}
			assert.Equal(t, tt.want, p.overBudgetLocked())
		})
	}
}

func TestUnit_Peer_Write_ChargesQueuedBytes(t *testing.T) {
	queued := func(p *peer) int64 {
		p.wmu.Lock()
		defer p.wmu.Unlock()
		return p.queuedBytes
	}

	p := &peer{server: &Server{}, writeCh: make(chan []byte, 3), writeBudget: 4}
	p.write([]byte{1, 2, 3})
	p.write([]byte{4, 5})
	assert.Equal(t, int64(5), queued(p), "each frame is charged its own length, and the one that crosses the budget is queued")
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

func TestUnit_Peer_HandshakeIsNotCharged(t *testing.T) {
	p := &peer{server: &Server{}, writeCh: make(chan []byte, 4), writeBudget: 4}
	p.setHandshaking(true)
	p.write([]byte{1, 2, 3, 4, 5})
	p.write([]byte{6})
	p.setHandshaking(false)
	p.write([]byte{7, 8})

	p.wmu.Lock()
	defer p.wmu.Unlock()
	assert.Equal(t, int64(2), p.queuedBytes, "only the frame after the handshake is charged")
	for range 2 {
		p.creditLocked(len(<-p.writeCh))
	}
	assert.Equal(t, int64(2), p.queuedBytes, "the handshake frames leave the queue first, uncharged")
	p.creditLocked(len(<-p.writeCh))
	assert.Equal(t, int64(0), p.queuedBytes)
}
