package mobile

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

// jsonEqual reports whether two JSON byte slices encode the same value, ignoring
// object key order (compares via json.Unmarshal into any on both sides).
func jsonEqual(t *testing.T, got, want []byte) bool {
	t.Helper()
	var g, w any
	if err := json.Unmarshal(got, &g); err != nil {
		t.Fatalf("unmarshal got %q: %v", got, err)
	}
	if err := json.Unmarshal(want, &w); err != nil {
		t.Fatalf("unmarshal want %q: %v", want, err)
	}
	return reflect.DeepEqual(g, w)
}

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

func TestAwareness_ClientID(t *testing.T) {
	a, err := NewAwareness(99)
	if err != nil {
		t.Fatalf("NewAwareness: %v", err)
	}
	if got := a.ClientID(); got != 99 {
		t.Fatalf("ClientID = %d, want 99", got)
	}
}

func TestAwareness_LocalStateJSON_Lifecycle(t *testing.T) {
	a, err := NewAwareness(1)
	if err != nil {
		t.Fatalf("NewAwareness: %v", err)
	}

	// Freshly constructed: no local presence yet -> JSON null.
	js, err := a.LocalStateJSON()
	if err != nil {
		t.Fatalf("LocalStateJSON (fresh): %v", err)
	}
	if string(js) != "null" {
		t.Fatalf("LocalStateJSON (fresh) = %q, want null", js)
	}

	// After SetLocalState: a JSON object equal to the state we set.
	if err := a.SetLocalState([]byte(`{"user":"alice"}`)); err != nil {
		t.Fatalf("SetLocalState: %v", err)
	}
	js, err = a.LocalStateJSON()
	if err != nil {
		t.Fatalf("LocalStateJSON (set): %v", err)
	}
	if !jsonEqual(t, js, []byte(`{"user":"alice"}`)) {
		t.Fatalf("LocalStateJSON (set) = %q, want {\"user\":\"alice\"}", js)
	}

	// After ClearLocalState: presence removed -> JSON null again.
	a.ClearLocalState()
	js, err = a.LocalStateJSON()
	if err != nil {
		t.Fatalf("LocalStateJSON (cleared): %v", err)
	}
	if string(js) != "null" {
		t.Fatalf("LocalStateJSON (cleared) = %q, want null", js)
	}
}

func TestAwareness_LocalStateJSON_PresentButEmpty(t *testing.T) {
	a, err := NewAwareness(1)
	if err != nil {
		t.Fatalf("NewAwareness: %v", err)
	}
	// A present-but-empty state must serialize as {}, NOT null — this is the
	// present/absent distinction consumers rely on.
	if err := a.SetLocalState([]byte(`{}`)); err != nil {
		t.Fatalf("SetLocalState({}): %v", err)
	}
	js, err := a.LocalStateJSON()
	if err != nil {
		t.Fatalf("LocalStateJSON: %v", err)
	}
	if string(js) != "{}" {
		t.Fatalf("LocalStateJSON (present-but-empty) = %q, want {}", js)
	}
}

func TestAwareness_StatesJSON_Shape(t *testing.T) {
	a, err := NewAwareness(5)
	if err != nil {
		t.Fatalf("NewAwareness: %v", err)
	}
	// No states yet -> empty JSON object {}, not null.
	js, err := a.StatesJSON()
	if err != nil {
		t.Fatalf("StatesJSON (empty): %v", err)
	}
	if string(js) != "{}" {
		t.Fatalf("StatesJSON (empty) = %q, want {}", js)
	}

	// After setting a local state, the map is keyed by client ID.
	if err := a.SetLocalState([]byte(`{"user":"bob"}`)); err != nil {
		t.Fatalf("SetLocalState: %v", err)
	}
	js, err = a.StatesJSON()
	if err != nil {
		t.Fatalf("StatesJSON (set): %v", err)
	}
	if !strings.Contains(string(js), `"5"`) {
		t.Fatalf("StatesJSON = %q, want client id key \"5\"", js)
	}
}

func TestAwareness_PostClose_Contract(t *testing.T) {
	a, err := NewAwareness(3)
	if err != nil {
		t.Fatalf("NewAwareness: %v", err)
	}
	a.Close()

	// LocalStateJSON: (nil, ErrClosed)
	if js, err := a.LocalStateJSON(); js != nil || err != ErrClosed {
		t.Fatalf("LocalStateJSON after Close = (%v, %v), want (nil, ErrClosed)", js, err)
	}
	// StatesJSON: (nil, ErrClosed)
	if js, err := a.StatesJSON(); js != nil || err != ErrClosed {
		t.Fatalf("StatesJSON after Close = (%v, %v), want (nil, ErrClosed)", js, err)
	}
	// EncodeAll: nil
	if b := a.EncodeAll(); b != nil {
		t.Fatalf("EncodeAll after Close = %v, want nil", b)
	}
	// SetLocalState: ErrClosed
	if err := a.SetLocalState([]byte(`{"x":1}`)); err != ErrClosed {
		t.Fatalf("SetLocalState after Close = %v, want ErrClosed", err)
	}
	// ClearLocalState: no panic, no-op.
	a.ClearLocalState()
	// ClientID: 0
	if id := a.ClientID(); id != 0 {
		t.Fatalf("ClientID after Close = %d, want 0", id)
	}
}
