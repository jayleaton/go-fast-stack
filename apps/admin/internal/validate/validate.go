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
// data, then runs (*T).Validate: validate.Decode[validate.BanUser](r.Body).
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
		return fmt.Sprintf("%s must be %s", typeErr.Field, typeErr.Type)
	case strings.HasPrefix(err.Error(), "json: unknown field"):
		return strings.TrimPrefix(err.Error(), "json: ")
	default:
		return "must be valid JSON"
	}
}

// QueryDecoder is implemented by input types read from a query string (GET
// lists): DecodeQuery fills the fields, leaving defaults to Validate.
type QueryDecoder interface {
	DecodeQuery(q url.Values) error
}

// DecodeQuery reads T from a query string, then runs (*T).Validate:
// validate.DecodeQuery[validate.ListUsers](r.URL.Query()).
func DecodeQuery[T any, PT interface {
	*T
	QueryDecoder
	Validator
}](q url.Values) (T, error) {
	var v T
	if err := PT(&v).DecodeQuery(q); err != nil {
		return v, err
	}
	return v, PT(&v).Validate()
}
