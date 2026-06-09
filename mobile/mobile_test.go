package mobile

import "testing"

func TestCheckClientID(t *testing.T) {
	cases := []struct {
		name string
		id   int64
		ok   bool
	}{
		{"zero", 0, true},
		{"one", 1, true},
		{"max safe integer", 1 << 53, true},
		{"negative", -1, false},
		{"above max safe integer", (1 << 53) + 1, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := checkClientID(c.id)
			if (err == nil) != c.ok {
				t.Fatalf("checkClientID(%d): err=%v, want ok=%v", c.id, err, c.ok)
			}
		})
	}
}
