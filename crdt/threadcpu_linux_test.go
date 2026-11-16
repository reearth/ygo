//go:build linux

package crdt

import (
	"runtime"
	"runtime/debug"
	"syscall"
	"testing"
	"time"
	"unsafe"
)

// clockThreadCPUTimeID is CLOCK_THREAD_CPUTIME_ID, which counts the calling
// thread's CPU time to the nanosecond; getrusage(RUSAGE_THREAD) counts it too,
// but can lag behind for the running thread.
const clockThreadCPUTimeID = 3

// threadCPUClock measures the CPU time f runs on one locked thread, with the
// collector run before f and paused during it, so neither a wait for a core
// nor the marking f happens to assist changes the result.
func threadCPUClock(testing.TB) func(f func()) time.Duration {
	return func(f func()) time.Duration {
		runtime.LockOSThread()
		defer runtime.UnlockOSThread()
		runtime.GC()
		defer debug.SetGCPercent(debug.SetGCPercent(-1))
		start := threadCPUNow()
		f()
		return threadCPUNow() - start
	}
}

func threadCPUNow() time.Duration {
	var ts syscall.Timespec
	if _, _, errno := syscall.Syscall(syscall.SYS_CLOCK_GETTIME, clockThreadCPUTimeID, uintptr(unsafe.Pointer(&ts)), 0); errno != 0 {
		panic(errno)
	}
	return time.Duration(ts.Nano())
}
