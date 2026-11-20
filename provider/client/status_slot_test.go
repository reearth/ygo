//go:build go1.24

package client

import (
	"runtime"
	"testing"
	"weak"
)

// The test in this file needs the weak package, which Go 1.24 added; the
// module still builds with Go 1.23, which leaves it out.

// subscribeHeld subscribes to c's status a callback holding a large array and
// returns the unsubscribe and a weak pointer to the array; no frame of the
// test holds the callback.
func subscribeHeld(c *Client) (func(), weak.Pointer[[1 << 20]byte]) {
	big := new([1 << 20]byte)
	return c.OnStatus(func(Status) { big[0]++ }), weak.Make(big)
}

// An OnStatus unsubscribe clears the slot it vacates, so the callback it
// removes is reachable from nothing.
func TestUnit_OnStatus_UnsubscribeClearsTheVacatedSlot(t *testing.T) {
	c := &Client{}
	c.OnStatus(func(Status) {})
	unsubscribe, freed := subscribeHeld(c)
	unsubscribe()
	runtime.GC()
	if freed.Value() != nil {
		t.Error("the unsubscribed status callback is still reachable from the client's list")
	}
	runtime.KeepAlive(c)
}
