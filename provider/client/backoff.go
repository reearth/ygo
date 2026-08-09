package client

import (
	"math/rand"
	"time"
)

// randFloat is a package-level indirection over math/rand's Float64, purely
// so backoff_test.go can substitute a deterministic source of "randomness"
// instead of asserting statistical properties of the real generator (flaky
// by construction, and this package already has a documented flake
// history — see harness_test.go). Production code never touches this
// directly; only backoff.next reads it.
var randFloat = rand.Float64

// maxBackoffShift bounds how far backoff.next will attempt to left-shift
// base by attempt before giving up and using max outright. Any real
// max/base pair saturates (see next's doc) long before attempt reaches this,
// so the bound is purely defensive: it keeps a backoff that has been asked
// for thousands of delays across a very long outage from ever computing a
// shift wide enough to matter, rather than relying on the overflow guard in
// next to catch every case at every possible width.
const maxBackoffShift = 32

// backoff computes reconnect delays using Full Jitter — the schedule AWS's
// "Exponential Backoff And Jitter" piece popularized specifically to avoid
// the failure mode a fixed or additive-jitter schedule doesn't fix: every
// client of a recovering server retrying in near lockstep and knocking it
// back over. Each call to next draws a delay uniformly from
// [0, min(max, base*2^attempt)), so both the ceiling AND the amount of
// spread grow with repeated failures, and no two clients following the same
// schedule are likely to retry at the same instant.
//
// The zero value is directly usable — attempt starts at 0 as the zero int
// already gives — but base and max must be set by the caller; there is no
// sensible default for either baked in here (the reconnect loop's choice of
// 500ms/Options.MaxBackoff belongs to loop.go, not to this general-purpose
// type).
//
// backoff is not safe for concurrent use. It doesn't need to be: exactly one
// goroutine (the reconnect loop in loop.go) ever owns one.
type backoff struct {
	base, max time.Duration
	attempt   int
}

// next returns this attempt's delay and advances the internal attempt
// counter, so consecutive calls widen the range without the caller having
// to track attempt itself.
//
// The widening range is computed with an overflow guard: once base<<attempt
// would no longer fit inside a time.Duration's range (or has simply grown
// past max already), the range collapses to [0, max) and STAYS there for
// every later call. Left-shifting a Go integer by a large enough count does
// not panic — it silently produces a small, possibly negative, essentially
// garbage result — so the naive `base * (1 << attempt)` would eventually
// hand out a nonsensical (even zero-width, even negative) range for an
// outage long enough to keep attempt climbing. This guard is what makes
// that impossible: it accepts the shifted value only when it is both
// positive and still below max, and falls back to max otherwise.
func (b *backoff) next() time.Duration {
	limit := b.max
	if b.attempt < maxBackoffShift {
		if scaled := b.base << uint(b.attempt); scaled > 0 && scaled < b.max {
			limit = scaled
		}
	}
	b.attempt++
	return time.Duration(randFloat() * float64(limit))
}

// reset zeroes the attempt counter, so the next call to next() again draws
// from [0, base) instead of continuing to widen from wherever this backoff's
// prior failures left it.
//
// Call this ONLY after a connection attempt completes a full sync
// handshake (its first SyncStep2 applied) — never merely after a
// successful dial or TCP/WS upgrade. A server that accepts a connection and
// then immediately drops it (a load balancer briefly out of healthy
// backends, e.g.) is not "connected" in any sense that should let a client
// hammer it every ~500ms; resetting on dial success alone would turn
// exactly that situation into a hot retry loop instead of the widening
// backoff it exists to provide.
func (b *backoff) reset() {
	b.attempt = 0
}
