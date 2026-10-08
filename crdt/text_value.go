package crdt

import (
	"bytes"
	"encoding"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"reflect"
	"strconv"
)

// maxTextValueDepth is ReadAny's nesting limit: a deeper value would encode,
// but no ygo peer could read it back from V2.
const maxTextValueDepth = 100

var (
	// errSharedTypeValue rejects a Y type as a text value: ygo does not insert
	// it as ContentType, and as a plain value V1 would write it as {}.
	errSharedTypeValue = errors.New("a shared type cannot be embedded in YText")
	errDocValue        = errors.New("a Doc cannot be embedded in YText")
	errTextValueDepth  = fmt.Errorf("nested deeper than %d levels", maxTextValueDepth)

	sharedTypeT = reflect.TypeOf((*sharedType)(nil)).Elem()
	docPtrT     = reflect.TypeOf((*Doc)(nil))
)

// textValue returns v in the lib0-Any form both V1 and V2 encode, converting
// as Attributes documents; a non-string-keyed map also takes its json.Marshal
// form. It keeps a NaN or ±Inf, since a Yjs peer can send one, and errors for
// a shared type, Doc, func, chan or complex.
func textValue(v any) (any, error) { return textValueAt(v, 0) }

func textValueAt(v any, depth int) (any, error) {
	if depth > maxTextValueDepth {
		return nil, errTextValueDepth
	}
	switch t := v.(type) {
	case nil, bool, string, int64, float64, float32:
		return v, nil
	case int:
		return int64(t), nil
	case sharedType:
		return nil, errSharedTypeValue
	case *Doc:
		return nil, errDocValue
	case json.Number:
		if i, err := t.Int64(); err == nil {
			return i, nil
		}
		f, err := strconv.ParseFloat(string(t), 64)
		if err != nil && !errors.Is(err, strconv.ErrRange) {
			return nil, fmt.Errorf("json.Number %q is not a number", string(t))
		}
		return f, nil // ±Inf past float64's range, as JSON.parse reads it
	case []any:
		out := make([]any, len(t))
		for i, e := range t {
			var err error
			if out[i], err = textValueAt(e, depth+1); err != nil {
				return nil, err
			}
		}
		return out, nil
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, e := range t {
			var err error
			if out[k], err = textValueAt(e, depth+1); err != nil {
				return nil, err
			}
		}
		return out, nil
	case json.Marshaler, encoding.TextMarshaler:
		return marshalTextValue(v, depth)
	}
	rv := reflect.ValueOf(v)
	switch rv.Kind() {
	case reflect.Bool:
		return rv.Bool(), nil
	case reflect.String:
		return rv.String(), nil
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return rv.Int(), nil
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
		if u := rv.Uint(); u <= math.MaxInt64 {
			return int64(u), nil
		}
		return float64(rv.Uint()), nil // WriteAny's form above int64
	case reflect.Float32:
		return float32(rv.Float()), nil
	case reflect.Float64:
		return rv.Float(), nil
	case reflect.Pointer, reflect.Interface:
		if rv.IsNil() {
			return nil, nil
		}
		return textValueAt(rv.Elem().Interface(), depth+1)
	case reflect.Slice:
		if rv.IsNil() {
			return nil, nil
		}
		if rv.Type().Elem().Kind() == reflect.Uint8 {
			return rv.Bytes(), nil // lib0 binary; V1 writes it as base64 JSON text
		}
		fallthrough
	case reflect.Array:
		out := make([]any, rv.Len())
		for i := range out {
			var err error
			if out[i], err = textValueAt(rv.Index(i).Interface(), depth+1); err != nil {
				return nil, err
			}
		}
		return out, nil
	case reflect.Map:
		if rv.Type().Key().Kind() != reflect.String {
			return marshalTextValue(v, depth)
		}
		if rv.IsNil() {
			return nil, nil
		}
		out := make(map[string]any, rv.Len())
		for it := rv.MapRange(); it.Next(); {
			var err error
			if out[it.Key().String()], err = textValueAt(it.Value().Interface(), depth+1); err != nil {
				return nil, err
			}
		}
		return out, nil
	case reflect.Struct:
		return marshalTextValue(v, depth)
	}
	return nil, fmt.Errorf("unsupported type %T", v)
}

// marshalTextValue is textValue for a value only json.Marshal knows how to
// write; a shared type inside it would marshal as {}, so it is rejected.
func marshalTextValue(v any, depth int) (any, error) {
	if err := findSharedValue(reflect.ValueOf(v), depth); err != nil {
		return nil, err
	}
	b, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	var out any
	if err := dec.Decode(&out); err != nil {
		return nil, err
	}
	return textValueAt(out, depth)
}

// findSharedValue reports a shared type or Doc anywhere in rv, exported or
// not, since json.Marshal also writes an embedded unexported struct's fields.
func findSharedValue(rv reflect.Value, depth int) error {
	if depth > maxTextValueDepth || !rv.IsValid() {
		return nil // json.Marshal or textValueAt rejects what is left
	}
	if rv.Type() == docPtrT {
		return errDocValue
	}
	if rv.Type().Implements(sharedTypeT) {
		return errSharedTypeValue
	}
	switch rv.Kind() {
	case reflect.Pointer, reflect.Interface:
		return findSharedValue(rv.Elem(), depth+1)
	case reflect.Slice, reflect.Array:
		if k := rv.Type().Elem().Kind(); k <= reflect.Complex128 || k == reflect.String {
			return nil // scalar elements
		}
		for i := range rv.Len() {
			if err := findSharedValue(rv.Index(i), depth+1); err != nil {
				return err
			}
		}
	case reflect.Map:
		for it := rv.MapRange(); it.Next(); {
			if err := findSharedValue(it.Value(), depth+1); err != nil {
				return err
			}
		}
	case reflect.Struct:
		for i := range rv.NumField() {
			if err := findSharedValue(rv.Field(i), depth+1); err != nil {
				return err
			}
		}
	}
	return nil
}

// checkTextValue returns textValue(v), panicking if v has no lib0-Any form
// rather than letting V1 write it as null or every later V2 encode panic.
func checkTextValue(op, what string, v any) any {
	out, err := textValue(v)
	if err != nil {
		panic(fmt.Sprintf("crdt: %s: %s: not JSON-encodable: %v", op, what, err))
	}
	return out
}

// checkTextAttrs is checkTextValue for every attribute value, returning a
// new map; nil and empty attrs are returned as is.
func checkTextAttrs(op string, attrs Attributes) Attributes {
	if len(attrs) == 0 {
		return attrs
	}
	out := make(Attributes, len(attrs))
	for k, v := range attrs {
		out[k] = checkTextValue(op, fmt.Sprintf("attribute %q", k), v)
	}
	return out
}
