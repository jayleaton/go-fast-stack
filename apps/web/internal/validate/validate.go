// Package validate holds every input schema, named by CRUD verb
// (UpdateUserSettings, CreateEvent...). Transports decode with Decode and
// pass the validated value to a service; never validate inline.
package validate

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"reflect"
	"sort"
	"strings"
)

// Validator is implemented by every input type.
type Validator interface {
	Validate() error
}

// Error is a validation failure. Fields maps field name to message.
type Error struct {
	Fields map[string]string
}

func (e *Error) Error() string {
	keys := make([]string, 0, len(e.Fields))
	for k := range e.Fields {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, len(keys))
	for i, k := range keys {
		parts[i] = k + ": " + e.Fields[k]
	}
	return "invalid input: " + strings.Join(parts, ", ")
}

// Fields collects field errors; Err returns nil when there are none.
type Fields map[string]string

// Add records msg for field when bad is true.
func (f Fields) Add(bad bool, field, msg string) {
	if bad {
		f[field] = msg
	}
}

// Err returns an *Error, or nil when no field failed.
func (f Fields) Err() error {
	if len(f) == 0 {
		return nil
	}
	return &Error{Fields: f}
}

// Decode reads one JSON object into T, rejecting unknown fields and trailing
// data, then runs (*T).Validate: validate.Decode[validate.UpdateUserSettings](r.Body).
func Decode[T any, PT interface {
	*T
	Validator
}](r io.Reader) (T, error) {
	var v T
	dec := json.NewDecoder(r)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&v); err != nil {
		return v, &Error{Fields: map[string]string{"body": jsonMessage(err)}}
	}
	if dec.More() {
		return v, &Error{Fields: map[string]string{"body": "must be a single JSON object"}}
	}
	return v, PT(&v).Validate()
}

func jsonMessage(err error) string {
	var typeErr *json.UnmarshalTypeError
	switch {
	case errors.Is(err, io.EOF):
		return "is empty"
	case errors.As(err, &typeErr):
		return fmt.Sprintf("%s must be %s", typeErr.Field, jsonType(typeErr.Type))
	case strings.HasPrefix(err.Error(), "json: unknown field"):
		return strings.TrimPrefix(err.Error(), "json: ")
	default:
		return "must be valid JSON"
	}
}

// jsonType names t the way a JSON client sees it, never as a Go type.
func jsonType(t reflect.Type) string {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	switch t.Kind() {
	case reflect.String:
		return "a string"
	case reflect.Bool:
		return "a boolean"
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return "an integer"
	case reflect.Float32, reflect.Float64:
		return "a number"
	case reflect.Slice, reflect.Array:
		return "an array"
	default:
		return "an object"
	}
}

// Nullable distinguishes an absent field (Set false) from an explicit null
// (Set true, Value nil), so PATCH bodies can clear a value.
type Nullable[T any] struct {
	Set   bool
	Value *T
}

// UnmarshalJSON marks the field as present.
func (n *Nullable[T]) UnmarshalJSON(b []byte) error {
	n.Set = true
	if bytes.Equal(b, []byte("null")) {
		n.Value = nil
		return nil
	}
	return json.Unmarshal(b, &n.Value)
}
