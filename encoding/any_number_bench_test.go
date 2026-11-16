package encoding_test

import (
	"math"
	"testing"

	"github.com/reearth/ygo/encoding"
)

var anyNumbers = []struct {
	name string
	v    any
}{
	{"int64_7", int64(7)},
	{"int64_2^40", int64(1 << 40)},
	{"int64_2^40+1", int64(1<<40 + 1)},
	{"float64_1.5", 1.5},
	{"float64_pi", math.Pi},
}

func BenchmarkWriteAny_Numbers(b *testing.B) {
	for _, n := range anyNumbers {
		b.Run(n.name, func(b *testing.B) {
			e := encoding.NewEncoder()
			b.ReportAllocs()
			for range b.N {
				e.Reset()
				for range 1000 {
					e.WriteAny(n.v)
				}
			}
		})
	}
}

func BenchmarkReadAny_Numbers(b *testing.B) {
	for _, n := range anyNumbers {
		e := encoding.NewEncoder()
		for range 1000 {
			e.WriteAny(n.v)
		}
		buf := e.Bytes()
		b.Run(n.name, func(b *testing.B) {
			b.ReportAllocs()
			for range b.N {
				d := encoding.NewDecoder(buf)
				for range 1000 {
					if _, err := d.ReadAny(); err != nil {
						b.Fatal(err)
					}
				}
			}
		})
	}
}
