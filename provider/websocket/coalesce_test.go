package websocket

import (
	"testing"
	"time"
)

func TestResolveCoalesceConfig(t *testing.T) {
	cases := []struct {
		name         string
		window, wait time.Duration
		wantEnabled  bool
		wantWin      time.Duration
		wantMax      time.Duration
	}{
		{"defaults", 0, 0, true, 2 * time.Second, 10 * time.Second},
		{"disabled negative window", -1, 0, false, 0, 0},
		{"custom window default wait", 500 * time.Millisecond, 0, true, 500 * time.Millisecond, 10 * time.Second},
		{"custom both", time.Second, 3 * time.Second, true, time.Second, 3 * time.Second},
		{"maxwait clamped up to window", 5 * time.Second, time.Second, true, 5 * time.Second, 5 * time.Second},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			enabled, win, max := resolveCoalesceConfig(c.window, c.wait)
			if enabled != c.wantEnabled || win != c.wantWin || max != c.wantMax {
				t.Fatalf("resolveCoalesceConfig(%v,%v) = (%v,%v,%v), want (%v,%v,%v)",
					c.window, c.wait, enabled, win, max, c.wantEnabled, c.wantWin, c.wantMax)
			}
		})
	}
}
