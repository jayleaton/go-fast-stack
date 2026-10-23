package api_test

import (
	"errors"
	"fmt"
	"maps"
	"net/http"
	"slices"
	"strings"
	"testing"

	"go.uber.org/mock/gomock"

	"github.com/jayleaton/go-fast-stack/apps/admin/internal/access"
	"github.com/jayleaton/go-fast-stack/apps/admin/internal/api"
	"github.com/jayleaton/go-fast-stack/apps/admin/internal/api/apitest"
	"github.com/jayleaton/go-fast-stack/apps/admin/internal/api/mocks"
	"github.com/jayleaton/go-fast-stack/apps/admin/internal/config"
	"github.com/jayleaton/go-fast-stack/apps/admin/internal/services"
	"github.com/jayleaton/go-fast-stack/apps/admin/internal/validate"
)

var (
	grace  = access.Admin{ID: "a1", Name: "Grace", Email: "grace@example.com", TwoFactorEnabled: true}
	actor  = apitest.Actor(grace)
	routes = config.APIRoutes
	anyCtx = gomock.Any()
)

// svcs are the recorders of every mocked service port behind the router.
type svcs struct {
	users     *mocks.MockUserServiceMockRecorder
	admins    *mocks.MockAdminServiceMockRecorder
	audit     *mocks.MockAuditServiceMockRecorder
	analytics *mocks.MockAnalyticsServiceMockRecorder
}

// newRouter mounts every admin endpoint on one mux, as the server does.
func newRouter(test *testing.T) (svcs, http.Handler) {
	controller := gomock.NewController(test)
	users := mocks.NewMockUserService(controller)
	admins := mocks.NewMockAdminService(controller)
	audit := mocks.NewMockAuditService(controller)
	analytics := mocks.NewMockAnalyticsService(controller)
	mux := http.NewServeMux()
	api.NewUsers(users, apitest.Gate()).Routes(mux)
	api.NewAdmins(admins, apitest.Gate()).Routes(mux)
	api.NewAudit(audit, apitest.Gate()).Routes(mux)
	api.NewAnalytics(analytics, apitest.Gate()).Routes(mux)
	return svcs{users.EXPECT(), admins.EXPECT(), audit.EXPECT(), analytics.EXPECT()}, mux
}

func ptr[Value any](value Value) *Value { return &value }

// svcErr is a service error and the response it must map to.
type svcErr struct {
	err    error
	status int
	msg    string
}

// route is one endpoint. expect registers the service call a well-formed
// request makes, answering err (and a success value when err is nil).
// invalidPath/invalidBody is a request the validator rejects (field names
// the failing detail); when both are empty the endpoint takes no input and
// the service's own ErrValidation stands in. errs are the service errors
// specific to the endpoint, on top of the ones every endpoint maps.
type route struct {
	method, path, body       string
	expect                   func(deps svcs, err error)
	invalidPath, invalidBody string
	field                    string
	errs                     []svcErr
	check                    func(test *testing.T, res apitest.Response)
}

// everyRoute maps the errors any service may return to any request.
var everyRoute = []svcErr{
	{services.ErrForbidden, 403, "Forbidden"},
	{services.ErrTwoFactorRequired, 403, "Two-factor authentication is required"},
	{errors.New("pq: connection refused"), 500, "Internal server error"}, // never leaks
}

// allRoutes merges the per-service tables (users_test.go, admins_test.go...).
func allRoutes(test *testing.T) map[string]route {
	all := map[string]route{}
	for _, routeTable := range []map[string]route{userRoutes, adminRoutes, auditRoutes, analyticsRoutes} {
		for key, testCase := range routeTable {
			if _, dup := all[key]; dup {
				test.Fatalf("duplicate route name %q", key)
			}
			all[key] = testCase
		}
	}
	return all
}

func wantSuccess(test *testing.T, res apitest.Response) {
	test.Helper()
	if !apitest.Data[api.Success](test, res).Success {
		test.Errorf("data = %s", res.Data)
	}
}

func wantError(test *testing.T, res apitest.Response, status int, msg string) {
	test.Helper()
	if res.Status != status || *res.Error != msg {
		test.Errorf("got %d %q, want %d %q", res.Status, *res.Error, status, msg)
	}
}

// wantInvalid sends a request the validators reject: it must be a 400 with
// exactly want as details and never reach a service (the mocks have no
// expectations).
func wantInvalid(test *testing.T, method, path, body string, want map[string]string) {
	test.Helper()
	_, handler := newRouter(test)
	res := apitest.Do(test, handler, apitest.AsAdmin(grace), method, path, body)
	if res.Status != 400 || *res.Error != "Invalid request data" || !maps.Equal(res.Details, want) {
		test.Errorf("%s %.60s %.60q: %d %q %v, want 400 %v", method, path, body, res.Status, *res.Error, res.Details, want)
	}
}

// wantOK sends a valid request after expect registered the exact service
// call it must make.
func wantOK(test *testing.T, method, path, body string, expect func(deps svcs)) {
	test.Helper()
	deps, handler := newRouter(test)
	expect(deps)
	if res := apitest.Do(test, handler, apitest.AsAdmin(grace), method, path, body); res.Status != 200 {
		test.Errorf("%s %.60s %.60q: status %d (%s)", method, path, body, res.Status, *res.Error)
	}
}

// TestRoutes runs the boundary cases against every endpoint. The mocks fail
// the test on any service call a case does not expect.
func TestRoutes(test *testing.T) {
	test.Parallel()
	routes := allRoutes(test)
	for _, name := range slices.Sorted(maps.Keys(routes)) {
		rt := routes[name]
		test.Run(name+"/no session is 401", func(test *testing.T) {
			test.Parallel()
			_, handler := newRouter(test)
			res := apitest.Do(test, handler, apitest.Anonymous, rt.method, rt.path, rt.body)
			wantError(test, res, 401, "Unauthorized")
		})
		test.Run(name+"/invalid input is 400", func(test *testing.T) {
			test.Parallel()
			deps, handler := newRouter(test)
			path, body := rt.path, rt.body
			switch {
			case rt.invalidPath != "":
				path = rt.invalidPath
			case rt.invalidBody != "":
				body = rt.invalidBody
			default: // no input to get wrong: the service rejects the request
				rt.expect(deps, services.ErrValidation)
			}
			res := apitest.Do(test, handler, apitest.AsAdmin(grace), rt.method, path, body)
			if res.Status != 400 {
				test.Fatalf("status %d, want 400 (%s)", res.Status, *res.Error)
			}
			if rt.field != "" && (*res.Error != "Invalid request data" || res.Details[rt.field] == "") {
				test.Errorf("got %q %v, want details on %q", *res.Error, res.Details, rt.field)
			}
		})
		for _, testCase := range append(slices.Clone(everyRoute), rt.errs...) {
			test.Run(fmt.Sprintf("%s/%v is %d", name, testCase.err, testCase.status), func(test *testing.T) {
				test.Parallel()
				deps, handler := newRouter(test)
				rt.expect(deps, testCase.err)
				wantError(test, apitest.Do(test, handler, apitest.AsAdmin(grace), rt.method, rt.path, rt.body), testCase.status, testCase.msg)
			})
		}
		test.Run(name+"/ok is 200", func(test *testing.T) {
			test.Parallel()
			deps, handler := newRouter(test)
			rt.expect(deps, nil)
			res := apitest.Do(test, handler, apitest.AsAdmin(grace), rt.method, rt.path, rt.body)
			if res.Status != 200 {
				test.Fatalf("status %d (%s)", res.Status, *res.Error)
			}
			if rt.check == nil {
				test.Fatal("route has no check of its data")
			}
			rt.check(test, res)
		})
	}
}

// TestCallerActor: handlers act as the signed-in admin, on their session,
// carrying their 2FA state (the services' 2FA policy reads it).
func TestCallerActor(test *testing.T) {
	test.Parallel()
	want := services.Actor{AdminID: "a1", Email: "grace@example.com", SessionID: "session-a1", TwoFactorEnabled: true}
	if actor != want {
		test.Fatalf("actor = %+v, want %+v", actor, want)
	}
	if got := apitest.Actor(access.Admin{ID: "a2"}); got.TwoFactorEnabled || got.System {
		test.Fatalf("actor without 2FA = %+v", got)
	}
}

// TestErrorMapping covers every service error kind once; it is the same
// route wrapper for every endpoint.
func TestErrorMapping(test *testing.T) {
	test.Parallel()
	cases := []svcErr{
		{services.ErrUserNotFound, 404, "User not found"},
		{fmt.Errorf("auth: %w", services.ErrAdminExists), 409, "This person is already an admin"},
		{services.ErrInvalidRole, 400, "Unknown role"},
		{services.ErrValidation, 400, "Invalid request"},
		{services.ErrConflict, 409, "Conflict"},
		{services.ErrNotFound, 404, "Not found"},
		{&services.Error{Kind: services.KindRateLimited, Message: "Slow down"}, 429, "Slow down"},
		{&services.Error{Kind: services.KindUnauthorized, Message: "Nope"}, 401, "Nope"},
		{services.ErrAuditUnavailable, 503, "The audit log is unavailable, so nothing was changed. Try again later."},
		{&services.Error{Kind: services.Kind(99), Message: "odd"}, 500, "odd"},
		{&services.Error{Kind: services.KindInternal, Message: "secret detail"}, 500, "Internal server error"},
		{errors.New("pq: connection refused"), 500, "Internal server error"},
	}
	for _, testCase := range cases {
		deps, handler := newRouter(test)
		deps.users.UnbanUser(anyCtx, actor, "u1").Return(testCase.err)
		wantError(test, apitest.Do(test, handler, apitest.AsAdmin(grace), "POST", config.UserPath("u1", "/unban"), ""), testCase.status, testCase.msg)
	}
}

// TestDecodeBody covers validate.Decode's body errors through every route
// that reads a body: none reaches the service (the mocks have no
// expectations) and each names the body.
func TestDecodeBody(test *testing.T) {
	test.Parallel()
	bodies := []struct{ method, path string }{
		{"POST", config.UserPath("u1", "/ban")},
		{"PUT", config.UserPath("u1", "/role")},
		{"POST", routes.AdminInvites},
	}
	cases := []struct{ body, msg string }{
		{``, "is empty"},
		{`{"nope":1}`, `unknown field "nope"`},
		{`{} {}`, "must be a single JSON object"},
		{`{}[]`, "must be a single JSON object"},
		{`{`, "must be valid JSON"},
		{`null x`, "must be a single JSON object"},
	}
	for _, body := range bodies {
		for _, testCase := range cases {
			_, handler := newRouter(test)
			res := apitest.Do(test, handler, apitest.AsAdmin(grace), body.method, body.path, testCase.body)
			if res.Status != 400 || *res.Error != "Invalid request data" || res.Details["body"] != testCase.msg {
				test.Errorf("%s %s %q: %d %q %v, want body %q", body.method, body.path, testCase.body, res.Status, *res.Error, res.Details, testCase.msg)
			}
		}
	}
	// Wrong types name the field and the type it must be.
	types := []struct{ method, path, body, msg string }{
		{"POST", config.UserPath("u1", "/ban"), `{"banReason":1}`, "banReason must be a string"},
		{"POST", config.UserPath("u1", "/ban"), `{"banReason":"spam","banExpiresIn":"1h"}`, "banExpiresIn must be an integer"},
		{"POST", config.UserPath("u1", "/ban"), `{"banReason":"spam","banExpiresIn":1.5}`, "banExpiresIn must be an integer"},
		{"PUT", config.UserPath("u1", "/role"), `{"role":["admin"]}`, "role must be a string"},
		{"POST", routes.AdminInvites, `{"email":true}`, "email must be a string"},
		{"POST", routes.AdminInvites, `{"email":"a@b.co","name":{}}`, "name must be a string"},
	}
	for _, testCase := range types {
		_, handler := newRouter(test)
		res := apitest.Do(test, handler, apitest.AsAdmin(grace), testCase.method, testCase.path, testCase.body)
		if res.Status != 400 || res.Details["body"] != testCase.msg {
			test.Errorf("%s %s %q: %d %v, want body %q", testCase.method, testCase.path, testCase.body, res.Status, res.Details, testCase.msg)
		}
	}
}

// TestBodyLimit: the route wrapper caps request bodies.
func TestBodyLimit(test *testing.T) {
	test.Parallel()
	_, handler := newRouter(test)
	res := apitest.Do(test, handler, apitest.AsAdmin(grace), "POST", config.UserPath("u1", "/ban"), `{"banReason":"`+strings.Repeat("x", 2<<20)+`"}`)
	if res.Status != 400 || res.Details["body"] == "" {
		test.Fatalf("oversized body: %d %v", res.Status, res.Details)
	}
}

// TestRoute: the route handler admits each request by its policy before the
// handler runs, and answers a refusal in the envelope without running it. A
// policy declared here (no middleware involved) shows that the extension
// point works on its own.
func TestRoute(test *testing.T) {
	test.Parallel()
	errLookup := errors.New("role lookup: connection refused")
	onlyGrace := func(_ *http.Request, caller access.Caller) error {
		if caller.Admin.Email != grace.Email {
			return fmt.Errorf("%w: not grace", access.ErrDenied)
		}
		return nil
	}
	other := access.Admin{ID: "a2", Email: "bob@example.com"}
	failing := access.NewGate(func(*http.Request) (access.Caller, error) { return access.Caller{}, errLookup })
	cases := []struct {
		name    string
		gate    *access.Gate
		policy  access.Policy
		session apitest.Session
		status  int
		message string // the error; "" for a success
		caller  string // the admin ID the handler sees
	}{
		{"public, signed out", apitest.Gate(), access.Public, apitest.Anonymous, http.StatusOK, "", ""},
		{"public, signed in", apitest.Gate(), access.Public, apitest.AsAdmin(grace), http.StatusOK, "", grace.ID},
		{"signed in, signed out", apitest.Gate(), access.SignedIn, apitest.Anonymous, http.StatusUnauthorized, "Unauthorized", ""},
		{"signed in", apitest.Gate(), access.SignedIn, apitest.AsAdmin(grace), http.StatusOK, "", grace.ID},
		{"custom policy, allowed", apitest.Gate(), onlyGrace, apitest.AsAdmin(grace), http.StatusOK, "", grace.ID},
		{"custom policy, denied", apitest.Gate(), onlyGrace, apitest.AsAdmin(other), http.StatusForbidden, "Forbidden", ""},
		{"resolver fails", failing, access.Public, apitest.AsAdmin(grace), http.StatusInternalServerError, "Internal server error", ""},
	}
	for _, testCase := range cases {
		test.Run(testCase.name, func(test *testing.T) {
			test.Parallel()
			ran := false
			var seen string
			handler := api.Route(testCase.gate, testCase.policy, func(_ *http.Request, caller access.Caller) (api.Success, error) {
				ran, seen = true, caller.Admin.ID
				return api.Success{Success: true}, nil
			})
			res := apitest.Do(test, handler, testCase.session, "GET", "/", "")
			if testCase.message != "" {
				wantError(test, res, testCase.status, testCase.message)
				if ran {
					test.Fatal("handler ran on a refused request")
				}
				return
			}
			if res.Status != testCase.status {
				test.Fatalf("status %d, want %d", res.Status, testCase.status)
			}
			wantSuccess(test, res)
			if seen != testCase.caller {
				test.Fatalf("handler saw caller %q, want %q", seen, testCase.caller)
			}
		})
	}
}

// TestRouteMapsHandlerErrors: an admitted handler's error maps like any
// service error, on a public route too.
func TestRouteMapsHandlerErrors(test *testing.T) {
	test.Parallel()
	handler := api.Route(apitest.Gate(), access.Public, func(*http.Request, access.Caller) (api.Success, error) {
		return api.Success{}, services.ErrUserNotFound
	})
	wantError(test, apitest.Do(test, handler, apitest.Anonymous, "GET", "/", ""), http.StatusNotFound, "User not found")
}

// TestValidationErrorString: a validation error reads the same every time
// (fields sorted), for logs and wrapped errors.
func TestValidationErrorString(test *testing.T) {
	test.Parallel()
	err := &validate.Error{Fields: map[string]string{"offset": "must be 0 or more", "limit": "must be 1-100"}}
	if got, want := err.Error(), "invalid input: limit: must be 1-100, offset: must be 0 or more"; got != want {
		test.Fatalf("Error() = %q, want %q", got, want)
	}
}
