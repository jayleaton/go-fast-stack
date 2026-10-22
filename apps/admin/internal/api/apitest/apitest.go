// Package apitest drives admin JSON route handlers in tests: the real router
// and route wrapper, a generated mock of the service port (api/mocks), and a
// mock session per request.
//
//	svc := mocks.NewMockUserService(gomock.NewController(t))
//	mux := http.NewServeMux()
//	api.NewUsers(svc, apitest.CallerFrom).Routes(mux)
//	res := apitest.Do(t, mux, apitest.AsAdmin(grace), "GET", config.APIRoutes.Users, "")
package apitest

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jayleaton/go-fast-stack/apps/admin/internal/api"
	"github.com/jayleaton/go-fast-stack/apps/admin/internal/services"
)

// Session decides who a test request is signed in as.
type Session func(*http.Request) *http.Request

// Anonymous sends the request without a session.
func Anonymous(r *http.Request) *http.Request { return r }

type callerKey struct{}

// AsAdmin signs the request in as a, on the session Caller(a).Session.
func AsAdmin(a api.Admin) Session {
	return func(r *http.Request) *http.Request {
		return r.WithContext(context.WithValue(r.Context(), callerKey{}, Caller(a)))
	}
}

// Caller is the caller a request signed in AsAdmin(a) carries.
func Caller(a api.Admin) api.Caller {
	return api.Caller{Admin: a, Session: api.Session{ID: "session-" + a.ID, ExpiresAt: time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)}}
}

// Actor is the services.Actor handlers pass for a request signed in AsAdmin(a).
func Actor(a api.Admin) services.Actor { return Caller(a).Actor() }

// CallerFrom is the api.CallerFrom of a router under test.
func CallerFrom(ctx context.Context) (api.Caller, bool) {
	c, ok := ctx.Value(callerKey{}).(api.Caller)
	return c, ok
}

// Response is a decoded envelope.
type Response struct {
	Status  int
	Data    json.RawMessage // "null" on errors
	Error   *string
	Details map[string]string
}

// Do serves one request through h. It fails the test unless the response is
// JSON and carries exactly one of data and error.
func Do(t testing.TB, h http.Handler, as Session, method, path, body string) Response {
	t.Helper()
	req := as(httptest.NewRequestWithContext(t.Context(), method, path, strings.NewReader(body)))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Fatalf("%s %s: content-type %q, body %s", method, path, ct, rec.Body)
	}
	var env struct {
		Data    json.RawMessage   `json:"data"`
		Error   *string           `json:"error"`
		Details map[string]string `json:"details"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("%s %s: body %q: %v", method, path, rec.Body, err)
	}
	hasData := len(env.Data) > 0 && string(env.Data) != "null"
	if hasData == (env.Error != nil) {
		t.Fatalf("%s %s: envelope must carry exactly one of data and error: %s", method, path, rec.Body)
	}
	return Response{Status: rec.Code, Data: env.Data, Error: env.Error, Details: env.Details}
}

// Data decodes the data of a success response into T.
func Data[T any](t testing.TB, r Response) T {
	t.Helper()
	var v T
	if err := json.Unmarshal(r.Data, &v); err != nil {
		t.Fatalf("data %s: %v", r.Data, err)
	}
	return v
}
