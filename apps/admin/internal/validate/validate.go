// Package validate holds every admin input schema, named by CRUD verb
// (BanUser, CreateAdminInvite...). Transports decode bodies with Decode and
// queries with DecodeQuery, then pass the validated value to a service; never
// validate inline. It mirrors apps/web/internal/validate (Go internal rules
// keep the apps from sharing it).
package validate

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
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

func (err *Error) Error() string {
	keys := make([]string, 0, len(err.Fields))
	for key := range err.Fields {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	parts := make([]string, len(keys))
	for index, key := range keys {
		parts[index] = key + ": " + err.Fields[key]
	}
	return "invalid input: " + strings.Join(parts, ", ")
}

// Fields collects field errors; Err returns nil when there are none.
type Fields map[string]string

// Add records msg for field when bad is true.
func (fields Fields) Add(bad bool, field, msg string) {
	if bad {
		fields[field] = msg
	}
}

// Err returns an *Error, or nil when no field failed.
func (fields Fields) Err() error {
	if len(fields) == 0 {
		return nil
	}
	return &Error{Fields: fields}
}

// Decode reads one JSON object into T, rejecting unknown fields and trailing
// data, then runs (*T).Validate: validate.Decode[validate.BanUser](r.Body).
func Decode[Value any, PT interface {
	*Value
	Validator
}](reader io.Reader) (Value, error) {
	var value Value
	return value, DecodeInto(reader, PT(&value))
}

// DecodeInto is Decode for a target only known as a Validator (the API's
// generic endpoints): one JSON object into target, then target.Validate().
func DecodeInto(reader io.Reader, target Validator) error {
	decoder := json.NewDecoder(reader)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return &Error{Fields: map[string]string{"body": jsonMessage(err)}}
	}
	if decoder.More() {
		return &Error{Fields: map[string]string{"body": "must be a single JSON object"}}
	}
	return target.Validate()
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
func jsonType(kind reflect.Type) string {
	for kind.Kind() == reflect.Pointer {
		kind = kind.Elem()
	}
	switch kind.Kind() {
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

// QueryDecoder is implemented by input types read from a query string (GET
// lists): DecodeQuery fills the fields, leaving defaults to Validate.
type QueryDecoder interface {
	DecodeQuery(values url.Values) error
}

// DecodeQuery reads T from a query string, then runs (*T).Validate:
// validate.DecodeQuery[validate.ListUsers](r.URL.Query()).
func DecodeQuery[Value any, PT interface {
	*Value
	QueryDecoder
	Validator
}](values url.Values) (Value, error) {
	var value Value
	return value, DecodeQueryInto(values, PT(&value))
}

// QueryInput is an input read from a query string and then validated.
type QueryInput interface {
	QueryDecoder
	Validator
}

// DecodeQueryInto is DecodeQuery for a target only known as a QueryInput.
func DecodeQueryInto(query url.Values, target QueryInput) error {
	if err := target.DecodeQuery(query); err != nil {
		return err
	}
	return target.Validate()
}
