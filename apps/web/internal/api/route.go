package api

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strings"

	"github.com/jayleaton/go-fast-stack/apps/web/internal/access"
	"github.com/jayleaton/go-fast-stack/apps/web/internal/validate"
)

// maxBody caps JSON request bodies at 1 MiB.
const maxBody = 1 << 20

// Handler is a JSON endpoint: it gets the caller the gate admitted (the zero
// Caller on a public route without a session) and returns the data for the
// envelope or an error to map.
type Handler[Result any] func(request *http.Request, caller access.Caller) (Result, error)

// Route is the one route handler of the JSON API. The gate resolves the
// caller and enforces policy (signed out: 401, denied: 403, both in the
// envelope); then the body is capped, handler runs, and its result or error
// becomes the envelope. Endpoint.Serve is Route for an Endpoint.
func Route[Result any](gate *access.Gate, policy access.Policy, handler Handler[Result]) http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		caller, request, err := gate.Admit(request, policy)
		switch {
		case err == nil:
		case errors.Is(err, access.ErrSignedOut):
			WriteError(writer, http.StatusUnauthorized, "Unauthorized", nil)
			return
		case errors.Is(err, access.ErrDenied):
			slog.InfoContext(request.Context(), "access denied", "method", request.Method, "path", request.URL.Path, "err", err)
			WriteError(writer, http.StatusForbidden, "Forbidden", nil)
			return
		default:
			writeErr(writer, request, err)
			return
		}
		request.Body = http.MaxBytesReader(writer, request.Body, maxBody)
		data, err := handler(request, caller)
		if err != nil {
			writeErr(writer, request, err)
			return
		}
		WriteData(writer, http.StatusOK, data)
	})
}

// NoInput is the Input of an endpoint that takes no body.
type NoInput struct{}

// Validate accepts the empty input.
func (*NoInput) Validate() error { return nil }

// Endpoint is one JSON API operation, declared once: where it is served,
// who may call it (its access policy) and what it does with a validated
// input. Both ways in run the same path through it:
//
//   - Serve, over HTTP (the mobile app, servers): Route decodes the JSON body
//     into Input with validate.DecodeInto and writes the envelope.
//   - Call, in-process (the UI, through apiclient.Client): the same gate and
//     policy, the same Input.Validate, the same Handle; errors come back as
//     values (access.ErrSignedOut, access.ErrDenied, *validate.Error, service
//     errors) instead of an envelope.
//
// Input must be a validate.Validator through its pointer (every validate
// type is); NoInput for none. Path parameters ({id}) are read with
// request.PathValue in both cases.
type Endpoint[Input any, Result any] struct {
	Method, Path string
	Policy       access.Policy
	Handle       func(request *http.Request, caller access.Caller, input Input) (Result, error)
}

// Pattern is the ServeMux pattern of the endpoint.
func (endpoint Endpoint[Input, Result]) Pattern() string {
	return endpoint.Method + " " + endpoint.Path
}

// Serve is the endpoint's HTTP handler.
func (endpoint Endpoint[Input, Result]) Serve(gate *access.Gate) http.Handler {
	return Route(gate, endpoint.Policy, func(request *http.Request, caller access.Caller) (Result, error) {
		var input Input
		if _, none := any(input).(NoInput); !none {
			if err := validate.DecodeInto(request.Body, validator(&input)); err != nil {
				var zero Result
				return zero, err
			}
		}
		return endpoint.Handle(request, caller, input)
	})
}

// PathValue fills one {name} wildcard of an endpoint's path for Call.
type PathValue struct{ Name, Value string }

// Call runs the endpoint in-process on behalf of the request ctx belongs to
// (auth.Middleware has put its session there): Admit with the endpoint's
// policy, then input.Validate, then Handle. This is what the UI's
// api.Client does for every read and write, so the UI reaches data only
// through the same gate and validation as an HTTP request.
func (endpoint Endpoint[Input, Result]) Call(ctx context.Context, gate *access.Gate, input Input, values ...PathValue) (Result, error) {
	var zero Result
	path := endpoint.Path
	for _, value := range values {
		path = strings.Replace(path, "{"+value.Name+"}", value.Value, 1)
	}
	request, err := http.NewRequestWithContext(ctx, endpoint.Method, path, http.NoBody)
	if err != nil {
		return zero, err
	}
	for _, value := range values {
		request.SetPathValue(value.Name, value.Value)
	}
	caller, request, err := gate.Admit(request, endpoint.Policy)
	if err != nil {
		return zero, err
	}
	if err := validator(&input).Validate(); err != nil {
		return zero, err
	}
	return endpoint.Handle(request, caller, input)
}

// validator is input as a validate.Validator. Every Input type implements
// it on its pointer; one that does not is a programming error caught by the
// first request in any test.
func validator[Input any](input *Input) validate.Validator {
	if target, ok := any(input).(validate.Validator); ok {
		return target
	}
	panic("api: endpoint input type does not implement validate.Validator")
}
