package client

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// withFixedRand replaces the package-level randFloat indirection with a
// function that always returns v, for the duration of the calling test, and
// restores the real generator on cleanup. This is what makes the backoff
// tests deterministic assertions on exact durations rather than statistical
// claims about a real PRNG (see backoff.go's randFloat doc).
//
// # Correctness here rests on two facts, not one (#228)
//
// First, t.Cleanup's LIFO ordering — which IS documented ("Cleanup functions
// will be called in last added, first called order", testing.T.Cleanup) —
// is what makes NESTED calls in one test (e.g. two withFixedRand calls in
// the same test body, as TestBackoff_NextSaturatesAtMax does) restore
// correctly: each call captures whatever randFloat currently is as `orig`
// before overwriting it, so the LAST call's cleanup must run FIRST to hand
// back its immediate predecessor's value, chaining back to the real
// generator only once every call's cleanup has run in reverse order. An
// earlier version of this comment called that ordering "undocumented" — it
// isn't; Go's own doc states it plainly.
//
// Second, and this is the part actually worth calling out: randFloat is
// PACKAGE-LEVEL, mutable, shared state, and this helper's set/restore has no
// locking of its own. That is safe today only because nothing in this test
// package calls t.Parallel — every test that uses this helper runs to
// completion (cleanup included) before the next one starts. Adding
// t.Parallel to any test in this file, now or later, would let two tests'
// set/restore pairs interleave on the same var: a genuine data race, not
// merely a flaky assertion.
func withFixedRand(t *testing.T, v float64) {
	t.Helper()
	orig := randFloat
	randFloat = func() float64 { return v }
	t.Cleanup(func() { randFloat = orig })
}

// TestBackoff_NextIsFullJitterAtEachAttempt pins randFloat to a known value
// and checks next()'s result against the Full Jitter formula by hand —
// time.Duration(randFloat() * float64(min(max, base*2^attempt))) — at three
// consecutive attempts, proving both that the range widens (0, 1, 2 -> base,
// 2*base, 4*base) and that attempt auto-advances across calls without the
// caller tracking it.
func TestBackoff_NextIsFullJitterAtEachAttempt(t *testing.T) {
	withFixedRand(t, 0.5)
	b := backoff{base: 100 * time.Millisecond, max: 10 * time.Second}

	got0 := b.next()
	want0 := time.Duration(0.5 * float64(100*time.Millisecond))
	require.Equal(t, want0, got0, "attempt 0: range should be [0, base)")

	got1 := b.next()
	want1 := time.Duration(0.5 * float64(200*time.Millisecond))
	require.Equal(t, want1, got1, "attempt 1: range should be [0, 2*base)")

	got2 := b.next()
	want2 := time.Duration(0.5 * float64(400*time.Millisecond))
	require.Equal(t, want2, got2, "attempt 2: range should be [0, 4*base)")
}

// TestBackoff_NextSaturatesAtMax proves the range stops widening once
// base*2^attempt would exceed max: it collapses to [0, max) and STAYS there
// on every later call, rather than continuing to grow (or, worse, wrapping
// via overflow into something smaller than max).
func TestBackoff_NextSaturatesAtMax(t *testing.T) {
	withFixedRand(t, 0.5)
	b := backoff{base: 100 * time.Millisecond, max: 300 * time.Millisecond}

	require.Equal(t, 50*time.Millisecond, b.next(), "attempt 0: [0, 100ms)")
	require.Equal(t, 100*time.Millisecond, b.next(), "attempt 1: [0, 200ms)")
	// attempt 2 would want [0, 400ms), but max is 300ms.
	require.Equal(t, 150*time.Millisecond, b.next(), "attempt 2: capped to [0, 300ms)")
	require.Equal(t, 150*time.Millisecond, b.next(), "attempt 3: stays capped to [0, 300ms)")
}

// TestBackoff_ResetRestartsFromBase is the property the reconnect loop
// depends on for the whole "don't hot-loop against a server that accepts
// then drops" guarantee: reset must make the very next call behave exactly
// like a fresh backoff's first call, not merely shrink the range.
func TestBackoff_ResetRestartsFromBase(t *testing.T) {
	withFixedRand(t, 0.5)
	b := backoff{base: 100 * time.Millisecond, max: 10 * time.Second}
	b.next()
	b.next()
	b.next() // widen the range a few times

	b.reset()

	got := b.next()
	want := time.Duration(0.5 * float64(100*time.Millisecond))
	require.Equal(t, want, got, "reset must restart the range from base")
}

// TestBackoff_JitterVariesWithRandSource proves next()'s output is actually
// driven by randFloat (i.e. by whatever the injected source returns) rather
// than being some fixed function of attempt alone — a low injected value
// must produce a shorter delay than a high one drawn at the same attempt.
func TestBackoff_JitterVariesWithRandSource(t *testing.T) {
	base := backoff{base: time.Second, max: 10 * time.Second}

	withFixedRand(t, 0.1)
	low := base
	lowDelay := low.next()

	withFixedRand(t, 0.9)
	high := base
	highDelay := high.next()

	require.Less(t, lowDelay, highDelay)
}

// TestBackoff_NextStaysWithinHalfOpenRange checks the boundary end of Full
// Jitter's contract — "[0, limit)", half-open — holds even for a randFloat
// return value very close to 1 and across several widening and saturated
// attempts: the result must never reach, let alone exceed, max.
func TestBackoff_NextStaysWithinHalfOpenRange(t *testing.T) {
	withFixedRand(t, 0.999999)
	b := backoff{base: 500 * time.Millisecond, max: 2 * time.Second}

	for i := 0; i < 10; i++ {
		got := b.next()
		require.GreaterOrEqual(t, got, time.Duration(0))
		require.Less(t, got, 2*time.Second)
	}
}

// TestBackoff_ZeroRandProducesZeroDelay is the other boundary: randFloat
// returning exactly 0 (a legitimate math/rand.Float64 result) must yield a
// zero delay, not some minimum floor — Full Jitter's range is genuinely
// [0, limit), inclusive of 0.
func TestBackoff_ZeroRandProducesZeroDelay(t *testing.T) {
	withFixedRand(t, 0)
	b := backoff{base: 500 * time.Millisecond, max: 30 * time.Second}
	require.Equal(t, time.Duration(0), b.next())
}
