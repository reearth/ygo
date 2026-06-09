package mobile

import (
	"strings"
	"testing"
)

func TestAwareness_EncodeApplyStates(t *testing.T) {
	a, err := NewAwareness(1)
	if err != nil {
		t.Fatalf("NewAwareness: %v", err)
	}
	if err := a.SetLocalState([]byte(`{"name":"alice"}`)); err != nil {
		t.Fatalf("SetLocalState: %v", err)
	}

	b, _ := NewAwareness(2)
	if err := b.ApplyUpdate(a.EncodeAll()); err != nil {
		t.Fatalf("ApplyUpdate: %v", err)
	}
	states, err := b.StatesJSON()
	if err != nil {
		t.Fatalf("StatesJSON: %v", err)
	}
	if !strings.Contains(string(states), `"alice"`) || !strings.Contains(string(states), `"1"`) {
		t.Fatalf("StatesJSON missing peer 1/alice: %s", states)
	}
}

func TestAwareness_Validation_Clear_Close(t *testing.T) {
	if _, err := NewAwareness(-1); err == nil {
		t.Fatal("NewAwareness(-1): expected error")
	}
	a, _ := NewAwareness(7)
	if err := a.SetLocalState(nil); err == nil {
		t.Fatal("SetLocalState(nil): expected error (use ClearLocalState)")
	}
	if err := a.SetLocalState([]byte(`{}`)); err != nil {
		t.Fatalf("SetLocalState({}): %v", err) // present-but-empty is valid
	}
	if err := a.SetLocalState([]byte(`not json`)); err == nil {
		t.Fatal("SetLocalState(invalid): expected error")
	}
	a.ClearLocalState()
	a.Close()
	a.Close() // idempotent
	if err := a.ApplyUpdate([]byte{1}); err != ErrClosed {
		t.Fatalf("ApplyUpdate after Close = %v, want ErrClosed", err)
	}
}
