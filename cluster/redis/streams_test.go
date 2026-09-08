package redis

import (
	"testing"
	"time"

	goredis "github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
)

// Transport's zero value must be PubSub so an existing Config keeps working.
func TestUnit_Streams_TransportZeroValueIsPubSub(t *testing.T) {
	require.Equal(t, PubSub, Transport(0))
}

func TestUnit_Streams_ResolveDefaults(t *testing.T) {
	mr := newMiniRedis(t)
	c := newClient(t, mr)

	got, err := resolveStreamCfg(c, Config{Transport: Streams})
	require.NoError(t, err)

	require.Equal(t, "ygo:stream:", got.prefix)
	require.Equal(t, 60*time.Second, got.retention)
	require.Equal(t, int64(4096), got.maxLen)
	require.Equal(t, int64(64), got.awMaxLen)
	require.Equal(t, 10*time.Second, got.awRetention)
	require.Equal(t, 4, got.readers)
	require.Equal(t, 30*time.Second, got.trimInterval)
	require.Equal(t, 5*time.Second, got.readBlock)
}

// An undersized pool starves publishes: every blocking XREAD holds a pool
// connection for its whole ReadBlock. Failing construction is much kinder
// than presenting as mysterious publish latency later.
func TestUnit_Streams_RejectsPoolSmallerThanReaders(t *testing.T) {
	mr := newMiniRedis(t)
	c := goredis.NewClient(&goredis.Options{Addr: mr.Addr(), PoolSize: 2})
	t.Cleanup(func() { _ = c.Close() })

	_, err := resolveStreamCfg(c, Config{Transport: Streams, Readers: 8})
	require.ErrorContains(t, err, "PoolSize")
}

// A sweeper slower than the window it enforces cannot enforce it.
func TestUnit_Streams_RejectsTrimIntervalAboveRetention(t *testing.T) {
	mr := newMiniRedis(t)
	c := newClient(t, mr)

	_, err := resolveStreamCfg(c, Config{
		Transport:       Streams,
		StreamRetention: 10 * time.Second,
		TrimInterval:    30 * time.Second,
	})
	require.ErrorContains(t, err, "TrimInterval")
}

func TestUnit_Streams_RejectsUnknownTransport(t *testing.T) {
	mr := newMiniRedis(t)
	c := newClient(t, mr)

	_, err := resolveStreamCfg(c, Config{Transport: Transport(99)})
	require.ErrorContains(t, err, "Transport")
}

// PubSub mode must not validate stream settings at all — an existing caller
// has never set them and must not start failing construction.
func TestUnit_Streams_PubSubModeSkipsStreamValidation(t *testing.T) {
	mr := newMiniRedis(t)
	c := goredis.NewClient(&goredis.Options{Addr: mr.Addr(), PoolSize: 1})
	t.Cleanup(func() { _ = c.Close() })

	_, err := resolveStreamCfg(c, Config{Readers: 999})
	require.NoError(t, err)
}
