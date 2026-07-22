package websocket

import "time"

// wsTimer is the minimal timer surface the persistence worker needs. Backed by
// *time.Timer in production; a fake in tests fires channels deterministically.
type wsTimer interface {
	ch() <-chan time.Time
	stop()
}

// wsClock creates wsTimers. Server.clock is nil in production and resolves to
// realClock{}; tests inject a fake for deterministic debounce behaviour.
type wsClock interface {
	newTimer(d time.Duration) wsTimer
}

type realClock struct{}

func (realClock) newTimer(d time.Duration) wsTimer { return &realTimer{t: time.NewTimer(d)} }

type realTimer struct{ t *time.Timer }

func (r *realTimer) ch() <-chan time.Time { return r.t.C }
func (r *realTimer) stop()                { r.t.Stop() }

const (
	defaultCoalesceWindow  = 2 * time.Second
	defaultCoalesceMaxWait = 10 * time.Second
)

// resolveCoalesceConfig turns the raw Server fields into effective values.
// window<0 disables coalescing (strict per-update). window==0 uses the default.
// maxWait==0 uses the default and is clamped to be at least the window.
func resolveCoalesceConfig(window, maxWait time.Duration) (enabled bool, win, maxW time.Duration) {
	if window < 0 {
		return false, 0, 0
	}
	win = window
	if win == 0 {
		win = defaultCoalesceWindow
	}
	maxW = maxWait
	if maxW == 0 {
		maxW = defaultCoalesceMaxWait
	}
	if maxW < win {
		maxW = win
	}
	return true, win, maxW
}
