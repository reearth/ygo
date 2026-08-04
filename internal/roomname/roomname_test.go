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

// TestUnit_Valid_RejectsInvalidUTF8 guards issue #209: ranging over a string
// yields utf8.RuneError (U+FFFD) for invalid bytes, which is above the
// control-character floor, so the old rule accepted these. Room names reach
// the wire (WriteVarString, which now rejects invalid UTF-8) and the Redis
// relay, so Valid must reject them outright rather than let them panic a
// connection goroutine later.
func TestUnit_Valid_RejectsInvalidUTF8(t *testing.T) {
	if Valid(string([]byte{0xff, 0xfe})) {
		t.Fatal("invalid UTF-8 room name must be rejected")
	}
	if !Valid("документ 📄") {
		t.Fatal("valid Unicode room names must still be accepted")
	}
}
