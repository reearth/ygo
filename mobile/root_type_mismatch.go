package mobile

import (
	"errors"
	"fmt"

	"github.com/reearth/ygo/crdt"
)

// recoverRootTypeMismatch converts CRDT root-type panics to mobile-safe errors.
// When err is nil, as in GetText, it swallows the matching panic so the named
// return value is returned as its zero value. Other panics remain programmer
// errors and keep their normal behavior.
func recoverRootTypeMismatch(err *error) {
	recovered := recover()
	if recovered == nil {
		return
	}

	mismatch, ok := recovered.(error)
	if !ok || !errors.Is(mismatch, crdt.ErrRootTypeMismatch) {
		panic(recovered)
	}
	if err != nil {
		*err = fmt.Errorf("ygo/mobile: %w", mismatch)
	}
}
