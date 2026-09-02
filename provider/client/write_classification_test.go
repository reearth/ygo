package client

import (
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestUnit_EveryWriteSiteClassifiesAuthRejection is a source-level guard, and
// it exists because this bug has now shipped twice by the same mechanism.
//
// A write failure returns before either read-path rejection detector runs, so
// EVERY site that writes to the connection and hands its error back to
// runReconnectLoop has to route through classifyWriteErr, or a rejected token
// gets retried. #238 covered two of the sites and left the SyncStep2 reply
// uncovered, which kept TestClient_Auth_WrongTokenIsTerminal flaking; the
// awareness reply and flushLane's own write were uncovered too.
//
// Behavioural tests can only cover the sites someone thought to write a test
// for — that is exactly how the gap survived. Counting the sites catches a NEW
// one on the day it is added, which is the failure mode that actually recurs.
//
// If this fails because you added a write: route its error through
// s.classifyWriteErr(...) and the count matches again. If your write genuinely
// cannot be in the rejection window (it runs after the handshake, or its error
// is discarded rather than returned), say so here and adjust the expectation
// deliberately rather than by reflex.
func TestUnit_EveryWriteSiteClassifiesAuthRejection(t *testing.T) {
	src, err := os.ReadFile("loop.go")
	require.NoError(t, err)
	text := string(src)

	// Only ERROR-CHECKED call sites: those are the ones whose failure reaches
	// runReconnectLoop and so must be classified. This deliberately excludes
	// write's own internal delegation to writeWithDeadline (a plain
	// `return s.writeWithDeadline(...)`, which forwards an error rather than
	// handling one) — counting that would demand a classification that has
	// nowhere to go.
	writeCalls := regexp.MustCompile(`if err := s\.write(WithDeadline)?\(`).FindAllString(text, -1)
	classified := strings.Count(text, "s.classifyWriteErr(")

	require.NotEmpty(t, writeCalls, "regex found no write sites at all — it has gone stale")
	require.Equal(t, len(writeCalls), classified,
		"every connection write whose error reaches runReconnectLoop must be wrapped in "+
			"s.classifyWriteErr, or a rejected auth token can be retried (found %d write "+
			"sites but %d classified)", len(writeCalls), classified)
}
