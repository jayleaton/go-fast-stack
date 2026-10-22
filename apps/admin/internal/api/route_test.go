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

	"github.com/jayleaton/ai-patterns-go/apps/admin/internal/api"
	"github.com/jayleaton/ai-patterns-go/apps/admin/internal/api/apitest"
	"github.com/jayleaton/ai-patterns-go/apps/admin/internal/api/mocks"
	"github.com/jayleaton/ai-patterns-go/apps/admin/internal/config"
	"github.com/jayleaton/ai-patterns-go/apps/admin/internal/services"
	"github.com/jayleaton/ai-patterns-go/apps/admin/internal/validate"
)

var (
	grace  = api.Admin{ID: "a1", Name: "Grace", Email: "grace@example.com", TwoFactorEnabled: true}
	actor  = apitest.Actor(grace)
	r      = config.APIRoutes
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
func newRouter(t *testing.T) (svcs, http.Handler) {
	ctrl := gomock.NewController(t)
	users := mocks.NewMockUserService(ctrl)
	admins := mocks.NewMockAdminService(ctrl)
	audit := mocks.NewMockAuditService(ctrl)
	analytics := mocks.NewMockAnalyticsService(ctrl)
	mux := http.NewServeMux()
	api.NewUsers(users, apitest.CallerFrom).Routes(mux)
	api.NewAdmins(admins, apitest.CallerFrom).Routes(mux)
	api.NewAudit(audit, apitest.CallerFrom).Routes(mux)
	api.NewAnalytics(analytics, apitest.CallerFrom).Routes(mux)
	return svcs{users.EXPECT(), admins.EXPECT(), audit.EXPECT(), analytics.EXPECT()}, mux
}

func ptr[T any](v T) *T { return &v }

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
	expect                   func(m svcs, err error)
	invalidPath, invalidBody string
	field                    string
	errs                     []svcErr
	check                    func(t *testing.T, res apitest.Response)
}

// everyRoute maps the errors any service may return to any request.
var everyRoute = []svcErr{
	{services.ErrForbidden, 403, "Forbidden"},
	{services.ErrTwoFactorRequired, 403, "Two-factor authentication is required"},
	{errors.New("pq: connection refused"), 500, "Internal server error"}, // never leaks
}

// allRoutes merges the per-service tables (users_test.go, admins_test.go...).
func allRoutes(t *testing.T) map[string]route {
	all := map[string]route{}
	for _, m := range []map[string]route{userRoutes, adminRoutes, auditRoutes, analyticsRoutes} {
		for k, v := range m {
			if _, dup := all[k]; dup {
				t.Fatalf("duplicate route name %q", k)
			}
			all[k] = v
		}
	}
	return all
}

func wantSuccess(t *testing.T, res apitest.Response) {
	t.Helper()
	if !apitest.Data[api.Success](t, res).Success {
		t.Errorf("data = %s", res.Data)
	}
}

func wantError(t *testing.T, res apitest.Response, status int, msg string) {
	t.Helper()
	if res.Status != status || *res.Error != msg {
		t.Errorf("got %d %q, want %d %q", res.Status, *res.Error, status, msg)
	}
}

// wantInvalid sends a request the validators reject: it must be a 400 with
// exactly want as details and never reach a service (the mocks have no
// expectations).
func wantInvalid(t *testing.T, method, path, body string, want map[string]string) {
	t.Helper()
	_, h := newRouter(t)
	res := apitest.Do(t, h, apitest.AsAdmin(grace), method, path, body)
	if res.Status != 400 || *res.Error != "Invalid request data" || !maps.Equal(res.Details, want) {
		t.Errorf("%s %.60s %.60q: %d %q %v, want 400 %v", method, path, body, res.Status, *res.Error, res.Details, want)
	}
}

// wantOK sends a valid request after expect registered the exact service
// call it must make.
func wantOK(t *testing.T, method, path, body string, expect func(m svcs)) {
	t.Helper()
	m, h := newRouter(t)
	expect(m)
	if res := apitest.Do(t, h, apitest.AsAdmin(grace), method, path, body); res.Status != 200 {
		t.Errorf("%s %.60s %.60q: status %d (%s)", method, path, body, res.Status, *res.Error)
	}
}

// TestRoutes runs the boundary cases against every endpoint. The mocks fail
// the test on any service call a case does not expect.
func TestRoutes(t *testing.T) {
	routes := allRoutes(t)
	for _, name := range slices.Sorted(maps.Keys(routes)) {
		rt := routes[name]
		t.Run(name+"/no session is 401", func(t *testing.T) {
			_, h := newRouter(t)
			res := apitest.Do(t, h, apitest.Anonymous, rt.method, rt.path, rt.body)
			wantError(t, res, 401, "Unauthorized")
		})
		t.Run(name+"/invalid input is 400", func(t *testing.T) {
			m, h := newRouter(t)
			path, body := rt.path, rt.body
			switch {
			case rt.invalidPath != "":
				path = rt.invalidPath
			case rt.invalidBody != "":
				body = rt.invalidBody
			default: // no input to get wrong: the service rejects the request
				rt.expect(m, services.ErrValidation)
			}
			res := apitest.Do(t, h, apitest.AsAdmin(grace), rt.method, path, body)
			if res.Status != 400 {
				t.Fatalf("status %d, want 400 (%s)", res.Status, *res.Error)
			}
			if rt.field != "" && (*res.Error != "Invalid request data" || res.Details[rt.field] == "") {
				t.Errorf("got %q %v, want details on %q", *res.Error, res.Details, rt.field)
			}
		})
		for _, e := range append(slices.Clone(everyRoute), rt.errs...) {
			t.Run(fmt.Sprintf("%s/%v is %d", name, e.err, e.status), func(t *testing.T) {
				m, h := newRouter(t)
				rt.expect(m, e.err)
				wantError(t, apitest.Do(t, h, apitest.AsAdmin(grace), rt.method, rt.path, rt.body), e.status, e.msg)
			})
		}
		t.Run(name+"/ok is 200", func(t *testing.T) {
			m, h := newRouter(t)
			rt.expect(m, nil)
			res := apitest.Do(t, h, apitest.AsAdmin(grace), rt.method, rt.path, rt.body)
			if res.Status != 200 {
				t.Fatalf("status %d (%s)", res.Status, *res.Error)
			}
			if rt.check == nil {
				t.Fatal("route has no check of its data")
			}
			rt.check(t, res)
		})
	}
}

// TestCallerActor: handlers act as the signed-in admin, on their session,
// carrying their 2FA state (the services' 2FA policy reads it).
func TestCallerActor(t *testing.T) {
	want := services.Actor{AdminID: "a1", Email: "grace@example.com", SessionID: "session-a1", TwoFactorEnabled: true}
	if actor != want {
		t.Fatalf("actor = %+v, want %+v", actor, want)
	}
	if got := apitest.Actor(api.Admin{ID: "a2"}); got.TwoFactorEnabled || got.System {
		t.Fatalf("actor without 2FA = %+v", got)
	}
}

// TestErrorMapping covers every service error kind once; it is the same
// route wrapper for every endpoint.
func TestErrorMapping(t *testing.T) {
	cases := []svcErr{
		{services.ErrUserNotFound, 404, "User not found"},
		{fmt.Errorf("auth: %w", services.ErrAdminExists), 409, "This person is already an admin"},
		{services.ErrInvalidRole, 400, "Unknown role"},
		{services.ErrValidation, 400, "Invalid request"},
		{services.ErrConflict, 409, "Conflict"},
		{services.ErrNotFound, 404, "Not found"},
		{&services.Error{Kind: services.KindRateLimited, Message: "Slow down"}, 429, "Slow down"},
		{&services.Error{Kind: services.KindUnauthorized, Message: "Nope"}, 401, "Nope"},
		{&services.Error{Kind: services.Kind(99), Message: "odd"}, 500, "odd"},
		{&services.Error{Kind: services.KindInternal, Message: "secret detail"}, 500, "Internal server error"},
		{errors.New("pq: connection refused"), 500, "Internal server error"},
	}
	for _, c := range cases {
		m, h := newRouter(t)
		m.users.UnbanUser(anyCtx, actor, "u1").Return(c.err)
		wantError(t, apitest.Do(t, h, apitest.AsAdmin(grace), "POST", config.UserPath("u1", "/unban"), ""), c.status, c.msg)
	}
}

// TestDecodeBody covers validate.Decode's body errors through every route
// that reads a body: none reaches the service (the mocks have no
// expectations) and each names the body.
func TestDecodeBody(t *testing.T) {
	bodies := []struct{ method, path string }{
		{"POST", config.UserPath("u1", "/ban")},
		{"PUT", config.UserPath("u1", "/role")},
		{"POST", r.AdminInvites},
	}
	cases := []struct{ body, msg string }{
		{``, "is empty"},
		{`{"nope":1}`, `unknown field "nope"`},
		{`{} {}`, "must be a single JSON object"},
		{`{}[]`, "must be a single JSON object"},
		{`{`, "must be valid JSON"},
		{`null x`, "must be a single JSON object"},
	}
	for _, b := range bodies {
		for _, c := range cases {
			_, h := newRouter(t)
			res := apitest.Do(t, h, apitest.AsAdmin(grace), b.method, b.path, c.body)
			if res.Status != 400 || *res.Error != "Invalid request data" || res.Details["body"] != c.msg {
				t.Errorf("%s %s %q: %d %q %v, want body %q", b.method, b.path, c.body, res.Status, *res.Error, res.Details, c.msg)
			}
		}
	}
	// Wrong types name the field and the type it must be.
	types := []struct{ method, path, body, msg string }{
		{"POST", config.UserPath("u1", "/ban"), `{"banReason":1}`, "banReason must be string"},
		{"POST", config.UserPath("u1", "/ban"), `{"banReason":"spam","banExpiresIn":"1h"}`, "banExpiresIn must be int64"},
		{"POST", config.UserPath("u1", "/ban"), `{"banReason":"spam","banExpiresIn":1.5}`, "banExpiresIn must be int64"},
		{"PUT", config.UserPath("u1", "/role"), `{"role":["admin"]}`, "role must be string"},
		{"POST", r.AdminInvites, `{"email":true}`, "email must be string"},
		{"POST", r.AdminInvites, `{"email":"a@b.co","name":{}}`, "name must be string"},
	}
	for _, c := range types {
		_, h := newRouter(t)
		res := apitest.Do(t, h, apitest.AsAdmin(grace), c.method, c.path, c.body)
		if res.Status != 400 || res.Details["body"] != c.msg {
			t.Errorf("%s %s %q: %d %v, want body %q", c.method, c.path, c.body, res.Status, res.Details, c.msg)
		}
	}
}

// TestBodyLimit: the route wrapper caps request bodies.
func TestBodyLimit(t *testing.T) {
	_, h := newRouter(t)
	res := apitest.Do(t, h, apitest.AsAdmin(grace), "POST", config.UserPath("u1", "/ban"), `{"banReason":"`+strings.Repeat("x", 2<<20)+`"}`)
	if res.Status != 400 || res.Details["body"] == "" {
		t.Fatalf("oversized body: %d %v", res.Status, res.Details)
	}
}

// TestPublic: the public wrapper needs no session and maps errors the same
// way.
func TestPublic(t *testing.T) {
	ok := api.Public(func(*http.Request) (api.Success, error) { return api.Success{Success: true}, nil })
	res := apitest.Do(t, ok, apitest.Anonymous, "GET", "/", "")
	if res.Status != 200 {
		t.Fatalf("status %d", res.Status)
	}
	wantSuccess(t, res)

	fail := api.Public(func(*http.Request) (api.Success, error) { return api.Success{}, services.ErrUserNotFound })
	wantError(t, apitest.Do(t, fail, apitest.Anonymous, "GET", "/", ""), 404, "User not found")
}

// TestValidationErrorString: a validation error reads the same every time
// (fields sorted), for logs and wrapped errors.
func TestValidationErrorString(t *testing.T) {
	err := &validate.Error{Fields: map[string]string{"offset": "must be 0 or more", "limit": "must be 1-100"}}
	if got, want := err.Error(), "invalid input: limit: must be 1-100, offset: must be 0 or more"; got != want {
		t.Fatalf("Error() = %q, want %q", got, want)
	}
}
