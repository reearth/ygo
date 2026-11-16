//go:build !linux

package crdt

import (
	"testing"
	"time"
)

// threadCPUClock skips the test: it reads a thread's CPU time only on Linux.
func threadCPUClock(tb testing.TB) func(f func()) time.Duration {
	tb.Skip("needs the per-thread CPU clock (CLOCK_THREAD_CPUTIME_ID), read only on Linux")
	return nil
}
