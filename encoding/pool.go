package encoding

import "sync"

// encoderPool reuses Encoder instances across wire-framing call sites.
// Every send / broadcast path in provider/websocket and every internal
// encodeV1Locked call previously allocated a fresh Encoder plus its 64-byte
// initial buffer, then grew that buffer as fields were appended. The pool
// keeps the underlying buffer warm so subsequent calls can append without
// allocating until the previous high-water mark is exceeded.
//
// Pooled Encoders are NOT safe to share with consumers that outlive the
// Get/Put scope — call sites that hand the encoded bytes to another
// goroutine (e.g. via a write channel) MUST copy the bytes out before
// PutEncoder, since the underlying buffer is reused by the next Get.
// EncodeBytes wraps this pattern.
var encoderPool = sync.Pool{
	New: func() any { return NewEncoder() },
}

// GetEncoder returns a Reset Encoder from the pool. The encoder MUST be
// returned to the pool via PutEncoder once its bytes are no longer needed.
// The byte slice returned by the encoder's Bytes() method aliases the
// pooled buffer and becomes invalid after PutEncoder — copy out anything
// that needs to outlive the Put.
func GetEncoder() *Encoder {
	e := encoderPool.Get().(*Encoder)
	e.Reset()
	return e
}

// PutEncoder returns e to the pool for reuse. Callers must not access e
// or any slice obtained from e.Bytes() after this call returns.
func PutEncoder(e *Encoder) {
	if e == nil {
		return
	}
	encoderPool.Put(e)
}

// EncodeBytes runs fn against a pooled Encoder and returns an
// independently-allocated copy of the resulting bytes. The encoder is
// returned to the pool before EncodeBytes returns; the returned slice is
// safe to retain across goroutines and outlive the call. This is the
// preferred wrapper for wire-framing call sites that hand bytes to a
// write channel or other long-lived consumer.
//
// PutEncoder is deferred so a panic from fn still returns the encoder to
// the pool (GetEncoder calls Reset on the way out, so any partially-
// written bytes from the panicking call are discarded before the encoder
// is reused).
func EncodeBytes(fn func(*Encoder)) (out []byte) {
	e := GetEncoder()
	defer PutEncoder(e)
	fn(e)
	src := e.Bytes()
	out = make([]byte, len(src))
	copy(out, src)
	return out
}
