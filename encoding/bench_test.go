package encoding_test

import (
	"strings"
	"testing"

	"github.com/reearth/ygo/encoding"
)

// ---------------------------------------------------------------------------
// Pre-built encoded payloads used by decode benchmarks.
// These are initialised once at package level so the encoding cost is paid
// outside the measured loop.
// ---------------------------------------------------------------------------

var (
	encodedVarUintSmall []byte // value 42 (1 byte)
	encodedVarUintLarge []byte // value 1<<28 (5 bytes)

	encodedVarStringShort []byte // 10-char string
	encodedVarStringLong  []byte // 1000-char string

	encodedVarBytes256 []byte // 256-byte payload

	encodedAnyString []byte // WriteAny("hello world")

	shortStr = "helloworld"              // exactly 10 ASCII chars
	longStr  = strings.Repeat("x", 1000) // 1000-char string
	payload  = make([]byte, 256)
)

func init() {
	enc := encoding.NewEncoder()

	enc.WriteVarUint(42)
	encodedVarUintSmall = copyBytes(enc.Bytes())
	enc.Reset()

	enc.WriteVarUint(1 << 28)
	encodedVarUintLarge = copyBytes(enc.Bytes())
	enc.Reset()

	enc.WriteVarString(shortStr)
	encodedVarStringShort = copyBytes(enc.Bytes())
	enc.Reset()

	enc.WriteVarString(longStr)
	encodedVarStringLong = copyBytes(enc.Bytes())
	enc.Reset()

	for i := range payload {
		payload[i] = byte(i)
	}
	enc.WriteVarBytes(payload)
	encodedVarBytes256 = copyBytes(enc.Bytes())
	enc.Reset()

	enc.WriteAny("hello world")
	encodedAnyString = copyBytes(enc.Bytes())
	enc.Reset()
}

func copyBytes(b []byte) []byte {
	cp := make([]byte, len(b))
	copy(cp, b)
	return cp
}

// ---------------------------------------------------------------------------
// WriteVarUint
// ---------------------------------------------------------------------------

func BenchmarkWriteVarUint_Small(b *testing.B) {
	b.ReportAllocs()
	b.SetBytes(1) // 1 byte per encoded value
	enc := encoding.NewEncoder()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		enc.Reset()
		enc.WriteVarUint(42)
	}
}

func BenchmarkWriteVarUint_Large(b *testing.B) {
	b.ReportAllocs()
	b.SetBytes(5) // 1<<28 needs 5 bytes
	enc := encoding.NewEncoder()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		enc.Reset()
		enc.WriteVarUint(1 << 28)
	}
}

// ---------------------------------------------------------------------------
// ReadVarUint
// ---------------------------------------------------------------------------

func BenchmarkReadVarUint_Small(b *testing.B) {
	b.ReportAllocs()
	b.SetBytes(int64(len(encodedVarUintSmall)))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		d := encoding.NewDecoder(encodedVarUintSmall)
		if _, err := d.ReadVarUint(); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkReadVarUint_Large(b *testing.B) {
	b.ReportAllocs()
	b.SetBytes(int64(len(encodedVarUintLarge)))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		d := encoding.NewDecoder(encodedVarUintLarge)
		if _, err := d.ReadVarUint(); err != nil {
			b.Fatal(err)
		}
	}
}

// ---------------------------------------------------------------------------
// WriteVarString
// ---------------------------------------------------------------------------

func BenchmarkWriteVarString_Short(b *testing.B) {
	b.ReportAllocs()
	b.SetBytes(int64(len(shortStr)))
	enc := encoding.NewEncoder()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		enc.Reset()
		enc.WriteVarString(shortStr)
	}
}

func BenchmarkWriteVarString_Long(b *testing.B) {
	b.ReportAllocs()
	b.SetBytes(int64(len(longStr)))
	enc := encoding.NewEncoder()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		enc.Reset()
		enc.WriteVarString(longStr)
	}
}

// ---------------------------------------------------------------------------
// ReadVarString
// ---------------------------------------------------------------------------

func BenchmarkReadVarString_Short(b *testing.B) {
	b.ReportAllocs()
	b.SetBytes(int64(len(shortStr)))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		d := encoding.NewDecoder(encodedVarStringShort)
		if _, err := d.ReadVarString(); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkReadVarString_Long(b *testing.B) {
	b.ReportAllocs()
	b.SetBytes(int64(len(longStr)))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		d := encoding.NewDecoder(encodedVarStringLong)
		if _, err := d.ReadVarString(); err != nil {
			b.Fatal(err)
		}
	}
}

// ---------------------------------------------------------------------------
// WriteVarBytes / ReadVarBytes  (256-byte payload)
// ---------------------------------------------------------------------------

func BenchmarkWriteVarBytes(b *testing.B) {
	b.ReportAllocs()
	b.SetBytes(int64(len(payload)))
	enc := encoding.NewEncoder()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		enc.Reset()
		enc.WriteVarBytes(payload)
	}
}

func BenchmarkReadVarBytes(b *testing.B) {
	b.ReportAllocs()
	b.SetBytes(int64(len(payload)))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		d := encoding.NewDecoder(encodedVarBytes256)
		if _, err := d.ReadVarBytes(); err != nil {
			b.Fatal(err)
		}
	}
}

// ---------------------------------------------------------------------------
// WriteAny / ReadAny  (string variant)
// ---------------------------------------------------------------------------

func BenchmarkWriteAny_String(b *testing.B) {
	b.ReportAllocs()
	b.SetBytes(int64(len("hello world")))
	enc := encoding.NewEncoder()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		enc.Reset()
		enc.WriteAny("hello world")
	}
}

func BenchmarkReadAny_String(b *testing.B) {
	b.ReportAllocs()
	b.SetBytes(int64(len(encodedAnyString)))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		d := encoding.NewDecoder(encodedAnyString)
		if _, err := d.ReadAny(); err != nil {
			b.Fatal(err)
		}
	}
}

// ---------------------------------------------------------------------------
// Round-trip: encode then decode in the same timed loop.
// This exercises the allocator path end-to-end and gives a realistic picture
// of the cost per update operation.
// ---------------------------------------------------------------------------

func BenchmarkRoundTrip_VarUint(b *testing.B) {
	b.ReportAllocs()
	enc := encoding.NewEncoder()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		enc.Reset()
		enc.WriteVarUint(uint64(i))
		d := encoding.NewDecoder(enc.Bytes())
		if _, err := d.ReadVarUint(); err != nil {
			b.Fatal(err)
		}
	}
}

// ---------------------------------------------------------------------------
// Encoder reset / reuse vs allocating a new Encoder each iteration.
// BenchmarkEncoder_Reset measures the reset-and-reuse path; compare with
// BenchmarkEncoder_New to quantify the allocation savings.
// ---------------------------------------------------------------------------

func BenchmarkEncoder_Reset(b *testing.B) {
	b.ReportAllocs()
	enc := encoding.NewEncoder()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		enc.Reset()
		enc.WriteVarUint(12345)
		enc.WriteVarString(shortStr)
	}
}

func BenchmarkEncoder_New(b *testing.B) {
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		enc := encoding.NewEncoder()
		enc.WriteVarUint(12345)
		enc.WriteVarString(shortStr)
	}
}
