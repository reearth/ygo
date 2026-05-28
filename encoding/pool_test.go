package encoding

import (
	"bytes"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// #52 — pooled encoders must come back Reset, so callers don't have to
// remember to call Reset themselves and don't inherit stale bytes.
func TestUnit_GetEncoder_ReturnsResetEncoder(t *testing.T) {
	// Prime the pool with an encoder carrying bytes.
	first := GetEncoder()
	first.WriteVarUint(42)
	require.NotEmpty(t, first.Bytes(), "setup: encoder must hold bytes")
	PutEncoder(first)

	// Re-getting from the pool must give back a Reset encoder.
	second := GetEncoder()
	defer PutEncoder(second)
	assert.Empty(t, second.Bytes(),
		"GetEncoder must Reset the encoder before returning it; "+
			"otherwise pool consumers inherit previous bytes")
}

// #52 — EncodeBytes returns a buffer that is independent of the pooled
// encoder's underlying storage. The next GetEncoder must NOT alias the
// returned slice.
func TestUnit_EncodeBytes_ReturnsIndependentCopy(t *testing.T) {
	out := EncodeBytes(func(enc *Encoder) {
		enc.WriteVarUint(1)
		enc.WriteVarUint(2)
		enc.WriteVarUint(3)
	})
	original := bytes.Clone(out)

	// Force the pool to hand back the same encoder for a second use.
	next := GetEncoder()
	next.WriteVarUint(0xFFFFFFFF) // big value to ensure buffer reuse mutates same backing
	PutEncoder(next)

	assert.True(t, bytes.Equal(out, original),
		"the bytes returned by EncodeBytes must NOT alias the pooled buffer; "+
			"a subsequent encoder use must not mutate the returned slice")
}

// #52 — concurrent EncodeBytes calls from many goroutines must produce
// independent, non-corrupted outputs. This stresses the pool under load.
func TestUnit_EncodeBytes_ConcurrentSafe(t *testing.T) {
	const goroutines = 50
	const valuesPerGoroutine = 100

	var wg sync.WaitGroup
	wg.Add(goroutines)
	results := make([][]byte, goroutines)
	for g := 0; g < goroutines; g++ {
		g := g
		go func() {
			defer wg.Done()
			results[g] = EncodeBytes(func(enc *Encoder) {
				for i := 0; i < valuesPerGoroutine; i++ {
					enc.WriteVarUint(uint64(g*1000 + i))
				}
			})
		}()
	}
	wg.Wait()

	// Each goroutine's output must decode back to the values it wrote.
	for g, out := range results {
		dec := NewDecoder(out)
		for i := 0; i < valuesPerGoroutine; i++ {
			v, err := dec.ReadVarUint()
			require.NoError(t, err, "goroutine %d output %d: decode must succeed", g, i)
			require.Equal(t, uint64(g*1000+i), v,
				"goroutine %d output %d: pool corruption?", g, i)
		}
	}
}

// #53 A — RemainingBytes is documented to alias the decoder buffer.
// Verify that explicitly so future maintainers know not to "fix" it by
// adding a copy.
func TestUnit_RemainingBytes_AliasesUnderlyingBuffer(t *testing.T) {
	buf := []byte{0x01, 0x02, 0x03, 0x04, 0x05}
	dec := NewDecoder(buf)
	_, _ = dec.ReadUint8() // advance past byte 0
	rem := dec.RemainingBytes()
	require.Equal(t, []byte{0x02, 0x03, 0x04, 0x05}, rem)

	// Aliasing: mutating the original buffer must surface in the
	// returned slice. (The documented contract: callers must treat
	// the result as read-only.)
	buf[1] = 0xFF
	assert.Equal(t, byte(0xFF), rem[0],
		"RemainingBytes must alias the underlying buffer — this is the "+
			"zero-copy contract documented on the function")
}

// #53 A — RemainingBytesCopy returns an independent buffer that callers
// can retain across decoder buffer mutations.
func TestUnit_RemainingBytesCopy_IsIndependent(t *testing.T) {
	buf := []byte{0x01, 0x02, 0x03}
	dec := NewDecoder(buf)
	_, _ = dec.ReadUint8()
	cp := dec.RemainingBytesCopy()
	require.Equal(t, []byte{0x02, 0x03}, cp)

	buf[1] = 0xFF
	assert.Equal(t, byte(0x02), cp[0],
		"RemainingBytesCopy must be independent of the source buffer")
}
