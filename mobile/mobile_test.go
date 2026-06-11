package mobile

import (
	"reflect"
	"testing"
)

func TestCheckClientID(t *testing.T) {
	cases := []struct {
		name string
		id   int64
		ok   bool
	}{
		{"zero", 0, true},
		{"one", 1, true},
		{"max safe integer (2^53 - 1)", (1 << 53) - 1, true},
		{"negative", -1, false},
		{"two to the 53 (one past max safe)", 1 << 53, false},
		{"above max safe integer", (1 << 53) + 1, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := checkClientID(c.id)
			if (err == nil) != c.ok {
				t.Fatalf("checkClientID(%d): err=%v, want ok=%v", c.id, err, c.ok)
			}
		})
	}
}

// isMobileSafe reports whether t is a type gomobile bind can carry across the
// language boundary in this package's API.
func isMobileSafe(t reflect.Type) bool {
	switch t.Kind() {
	case reflect.String, reflect.Int64, reflect.Bool:
		return true
	case reflect.Slice:
		return t.Elem().Kind() == reflect.Uint8 // []byte
	case reflect.Pointer:
		return t == reflect.TypeOf((*Doc)(nil)) || t == reflect.TypeOf((*Awareness)(nil))
	case reflect.Interface:
		return t == reflect.TypeOf((*error)(nil)).Elem() // error
	default:
		return false
	}
}

func checkFuncSafe(t *testing.T, label string, fn reflect.Type, skipReceiver bool) {
	t.Helper()
	start := 0
	if skipReceiver {
		start = 1 // In(0) is the receiver
	}
	for i := start; i < fn.NumIn(); i++ {
		if !isMobileSafe(fn.In(i)) {
			t.Errorf("%s: param %d type %s is not gomobile-safe", label, i, fn.In(i))
		}
	}
	for i := 0; i < fn.NumOut(); i++ {
		if !isMobileSafe(fn.Out(i)) {
			t.Errorf("%s: return %d type %s is not gomobile-safe", label, i, fn.Out(i))
		}
	}
}

func TestExportedAPIIsMobileSafe(t *testing.T) {
	// Exported methods of the bound types.
	for _, ptr := range []any{(*Doc)(nil), (*Awareness)(nil)} {
		rt := reflect.TypeOf(ptr)
		for i := 0; i < rt.NumMethod(); i++ {
			mth := rt.Method(i)
			checkFuncSafe(t, rt.Elem().Name()+"."+mth.Name, mth.Func.Type(), true)
		}
	}
	// Exported package constructors.
	//
	// NOTE: keep this list in sync with every exported package-level func in
	// mobile/. The method side is reflection-driven and self-maintaining, but new
	// top-level constructors/funcs must be added here or they escape the safety
	// check.
	ctors := map[string]any{
		"NewDoc":             NewDoc,
		"NewDocWithClientID": NewDocWithClientID,
		"NewAwareness":       NewAwareness,
	}
	for name, fn := range ctors {
		checkFuncSafe(t, name, reflect.TypeOf(fn), false)
	}
}
