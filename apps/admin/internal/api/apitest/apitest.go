// Package apitest drives admin JSON route handlers in tests: the real router
// and route wrapper, a generated mock of the service port (api/mocks), and a
// mock session per request.
//
//	svc := mocks.NewMockUserService(gomock.NewController(t))
//	mux := http.NewServeMux()
//	api.NewUsers(svc, apitest.Gate()).Routes(mux)
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

	"github.com/jayleaton/go-fast-stack/apps/admin/internal/access"
	"github.com/jayleaton/go-fast-stack/apps/admin/internal/services"
)

// Session decides who a test request is signed in as.
type Session func(*http.Request) *http.Request

// Anonymous sends the request without a session.
func Anonymous(request *http.Request) *http.Request { return request }

type callerKey struct{}

// AsAdmin signs the request in as admin, on the session Caller(admin).Session.
func AsAdmin(admin access.Admin) Session {
	return func(request *http.Request) *http.Request {
		return request.WithContext(context.WithValue(request.Context(), callerKey{}, Caller(admin)))
	}
}

// sessionExpiry is when every test session expires: far enough ahead that no
// test sees it lapse.
var sessionExpiry = time.Date(2030, time.January, 1, 0, 0, 0, 0, time.UTC)

// Caller is the caller a request signed in AsAdmin(admin) carries.
func Caller(admin access.Admin) access.Caller {
	return access.Caller{Admin: admin, Session: access.Session{ID: "session-" + admin.ID, ExpiresAt: sessionExpiry}}
}

// Actor is the services.Actor handlers pass for a request signed in AsAdmin(admin).
func Actor(admin access.Admin) services.Actor { return Caller(admin).Actor() }

// Gate is the access gate of a router under test: it admits the session the
// request was signed in with (AsAdmin, Anonymous).
func Gate() *access.Gate {
	return access.NewGate(access.FromContext(func(ctx context.Context) (access.Caller, bool) {
		caller, ok := ctx.Value(callerKey{}).(access.Caller)
		return caller, ok
	}))
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
func Do(test testing.TB, handler http.Handler, as Session, method, path, body string) Response {
	test.Helper()
	req := as(httptest.NewRequestWithContext(test.Context(), method, path, strings.NewReader(body)))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		test.Fatalf("%s %s: content-type %q, body %s", method, path, ct, rec.Body)
	}
	var env struct {
		Data    json.RawMessage   `json:"data"`
		Error   *string           `json:"error"`
		Details map[string]string `json:"details"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		test.Fatalf("%s %s: body %q: %v", method, path, rec.Body, err)
	}
	hasData := len(env.Data) > 0 && string(env.Data) != "null"
	if hasData == (env.Error != nil) {
		test.Fatalf("%s %s: envelope must carry exactly one of data and error: %s", method, path, rec.Body)
	}
	return Response{Status: rec.Code, Data: env.Data, Error: env.Error, Details: env.Details}
}

// Data decodes the data of a success response into Value.
func Data[Value any](test testing.TB, response Response) Value {
	test.Helper()
	var value Value
	if err := json.Unmarshal(response.Data, &value); err != nil {
		test.Fatalf("data %s: %v", response.Data, err)
	}
	return value
}
