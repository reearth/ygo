// Package websocket - internal unit tests for unexported helpers.
package websocket

import (
	"io"
	"log/slog"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestServer_MaxMessageBytes_ConfigurableDefault(t *testing.T) {
	// Default: zero in config means use the maxWSMessageBytes constant.
	s := &Server{}
	assert.Equal(t, int64(64<<20), s.maxMessageBytes(), "default is 64 MiB")

	// Configured: positive value overrides default.
	s2 := &Server{MaxMessageBytes: 1 << 20}
	assert.Equal(t, int64(1<<20), s2.maxMessageBytes(), "configured value used")
}

func TestServer_Logger_FallsBackToDefault(t *testing.T) {
	s := &Server{}
	assert.NotNil(t, s.log(), "log() returns non-nil even with no Logger configured")

	custom := slog.New(slog.NewTextHandler(io.Discard, nil))
	s2 := &Server{Logger: custom}
	assert.Same(t, custom, s2.log(), "configured logger is returned")
}
