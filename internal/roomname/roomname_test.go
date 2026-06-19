package roomname

import (
	"strings"
	"testing"
)

func TestValid(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want bool
	}{
		{"simple", "my-room", true},
		{"with spaces", "room one", true},
		{"unicode", "café-室", true},
		{"max length", strings.Repeat("a", 255), true},
		{"empty", "", false},
		{"too long", strings.Repeat("a", 256), false},
		{"dot", ".", false},
		{"dotdot", "..", false},
		{"newline", "room\n1", false},
		{"tab", "room\t1", false},
		{"null", "room\x00", false},
		{"leading dot ok", ".hidden", true},
	}
	for _, c := range cases {
		if got := Valid(c.in); got != c.want {
			t.Errorf("%s: Valid(%q) = %v, want %v", c.name, c.in, got, c.want)
		}
	}
}
