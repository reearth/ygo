package awareness_test

import (
	"fmt"

	"github.com/reearth/ygo/awareness"
)

// ExampleAwareness_SetLocalState shows broadcasting presence state
// (cursor position, user color, etc.) to peers.
func ExampleAwareness_SetLocalState() {
	a := awareness.New(42)
	a.SetLocalState(map[string]any{
		"name": "Alice",
		"cursor": map[string]any{
			"line": 10,
			"col":  3,
		},
	})

	state := a.GetLocalState()
	fmt.Println("name:", state["name"])
	// Output: name: Alice
}
