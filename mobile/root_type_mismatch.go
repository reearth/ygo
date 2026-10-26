package mobile

import (
	"fmt"
	"strings"
)

// recoverRootTypeMismatch converts CRDT root-type panics to mobile-safe errors.
// Other panics remain programmer errors and keep their normal behavior.
func recoverRootTypeMismatch(err *error) {
	recovered := recover()
	if recovered == nil {
		return
	}

	message, ok := recovered.(string)
	if !ok || !strings.HasPrefix(message, "crdt: root ") || !strings.Contains(message, "; cannot access it as ") {
		panic(recovered)
	}
	if err != nil {
		*err = fmt.Errorf("ygo/mobile: %s", message)
	}
}
