package api_test

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"go.uber.org/mock/gomock"

	"github.com/jayleaton/ai-patterns-go/apps/web/internal/api"
	"github.com/jayleaton/ai-patterns-go/apps/web/internal/api/apitest"
	"github.com/jayleaton/ai-patterns-go/apps/web/internal/api/mocks"
	"github.com/jayleaton/ai-patterns-go/apps/web/internal/config"
	"github.com/jayleaton/ai-patterns-go/apps/web/internal/services"
	"github.com/jayleaton/ai-patterns-go/apps/web/internal/validate"
)

var (
	ada    = services.User{ID: "u1", Name: "Ada", Email: "ada@example.com"}
	actor  = apitest.Actor(ada)
	r      = config.APIRoutes
	anyCtx = gomock.Any()
)

func newRouter(t *testing.T) (*mocks.MockUserService, http.Handler) {
	svc := mocks.NewMockUserService(gomock.NewController(t))
	mux := http.NewServeMux()
	api.NewUsers(svc, apitest.CallerFrom).Routes(mux)
	return svc, mux
}

func ptr[T any](v T) *T { return &v }

// route is one endpoint. expect registers the service call a well-formed
// request makes, answering err (and a success value when err is nil);
// invalid is a body the validator rejects, or "" for endpoints without one.
type route struct {
	method, path, body string
	expect             func(m *mocks.MockUserServiceMockRecorder, err error)
	invalid            string
	check              func(t *testing.T, res apitest.Response)
}

var routes = map[string]route{
	"GET me": {
		method: "GET", path: r.Me,
		expect: func(m *mocks.MockUserServiceMockRecorder, err error) {
			m.GetProfile(anyCtx, actor, "u1").Return(ada, err)
		},
		check: func(t *testing.T, res apitest.Response) {
			got := apitest.Data[api.CurrentUser](t, res)
			if got.User.Email != ada.Email || got.Session.ID != actor.SessionID {
				t.Errorf("data = %+v", got)
			}
		},
	},
	"PATCH me": {
		method: "PATCH", path: r.Me, body: `{"name":"  Ada L  "}`, invalid: `{"name":""}`,
		expect: func(m *mocks.MockUserServiceMockRecorder, err error) {
			m.UpdateProfile(anyCtx, actor, "u1", validate.UpdateUserSettings{Name: ptr("Ada L")}).Return(services.User{ID: "u1", Name: "Ada L"}, err)
		},
		check: func(t *testing.T, res apitest.Response) {
			if got := apitest.Data[api.UserUpdated](t, res); !got.Success || got.User.Name != "Ada L" {
				t.Errorf("data = %+v", got)
			}
		},
	},
	"PATCH user by id": {
		method: "PATCH", path: config.UserPath("u2"), body: `{"image":null}`, invalid: `{"image":"javascript:alert(1)"}`,
		expect: func(m *mocks.MockUserServiceMockRecorder, err error) {
			m.UpdateProfile(anyCtx, actor, "u2", validate.UpdateUserSettings{Image: validate.Nullable[string]{Set: true}}).Return(services.User{ID: "u2"}, err)
		},
	},
	"DELETE me": {
		method: "DELETE", path: r.Me,
		expect: func(m *mocks.MockUserServiceMockRecorder, err error) {
			m.DeleteAccount(anyCtx, actor, "u1").Return(err)
		},
		check: wantSuccess,
	},
	"GET me data": {
		method: "GET", path: r.MeData,
		expect: func(m *mocks.MockUserServiceMockRecorder, err error) {
			m.DataSummary(anyCtx, actor, "u1").Return(services.UserDataSummary{User: ada, Sessions: 2}, err)
		},
		check: func(t *testing.T, res apitest.Response) {
			if got := apitest.Data[services.UserDataSummary](t, res); got.Sessions != 2 || got.User.ID != "u1" {
				t.Errorf("data = %+v", got)
			}
		},
	},
	"POST me email": {
		method: "POST", path: r.MeEmail, body: `{"newEmail":" Ada@New.Example ","callbackURL":"/dashboard"}`, invalid: `{"newEmail":"not an email"}`,
		expect: func(m *mocks.MockUserServiceMockRecorder, err error) {
			m.RequestEmailChange(anyCtx, actor, "u1", validate.UpdateUserEmail{NewEmail: "ada@new.example", CallbackURL: "/dashboard"}).Return(err)
		},
		check: wantSuccess,
	},
	"POST me password": {
		method: "POST", path: r.MePassword, body: `{"currentPassword":"password123","newPassword":"password456","revokeOtherSessions":true}`, invalid: `{"newPassword":"password456"}`,
		expect: func(m *mocks.MockUserServiceMockRecorder, err error) {
			m.ChangePassword(anyCtx, actor, "u1", validate.UpdateUserPassword{CurrentPassword: "password123", NewPassword: "password456", RevokeOtherSessions: true}).Return(err)
		},
		check: wantSuccess,
	},
	"GET me sessions": {
		method: "GET", path: r.MeSessions,
		expect: func(m *mocks.MockUserServiceMockRecorder, err error) {
			m.ListSessions(anyCtx, actor, "u1").Return([]services.Session{{ID: actor.SessionID, Current: true}, {ID: "s2"}}, err)
		},
		check: func(t *testing.T, res apitest.Response) {
			if got := apitest.Data[[]services.Session](t, res); len(got) != 2 || !got[0].Current || strings.Contains(string(res.Data), "token") {
				t.Errorf("data = %s", res.Data)
			}
		},
	},
	"DELETE me session": {
		method: "DELETE", path: config.MeSessionPath("s2"),
		expect: func(m *mocks.MockUserServiceMockRecorder, err error) {
			m.RevokeSession(anyCtx, actor, "u1", "s2").Return(err)
		},
		check: wantSuccess,
	},
	"DELETE me other sessions": {
		method: "DELETE", path: r.MeOtherSessions,
		expect: func(m *mocks.MockUserServiceMockRecorder, err error) {
			m.RevokeOtherSessions(anyCtx, actor, "u1").Return(err)
		},
		check: wantSuccess,
	},
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

// TestUserRoutes runs the four boundary cases against every endpoint. The
// mock fails the test on any service call a case does not expect.
func TestUserRoutes(t *testing.T) {
	for name, rt := range routes {
		t.Run(name+"/no session is 401", func(t *testing.T) {
			_, h := newRouter(t)
			wantError(t, apitest.Do(t, h, apitest.Anonymous, rt.method, rt.path, rt.body), 401, "Unauthorized")
		})
		t.Run(name+"/other user is 403", func(t *testing.T) {
			svc, h := newRouter(t)
			rt.expect(svc.EXPECT(), services.ErrForbidden)
			wantError(t, apitest.Do(t, h, apitest.AsUser(ada), rt.method, rt.path, rt.body), 403, "Forbidden")
		})
		t.Run(name+"/invalid input is 400", func(t *testing.T) {
			svc, h := newRouter(t)
			body := rt.invalid
			if body == "" { // no body to get wrong: the service rejects the request
				body = rt.body
				rt.expect(svc.EXPECT(), services.ErrValidation)
			}
			res := apitest.Do(t, h, apitest.AsUser(ada), rt.method, rt.path, body)
			if res.Status != 400 {
				t.Errorf("status %d, want 400 (%s)", res.Status, *res.Error)
			}
		})
		t.Run(name+"/ok is 200", func(t *testing.T) {
			svc, h := newRouter(t)
			rt.expect(svc.EXPECT(), nil)
			res := apitest.Do(t, h, apitest.AsUser(ada), rt.method, rt.path, rt.body)
			if res.Status != 200 {
				t.Fatalf("status %d (%s)", res.Status, *res.Error)
			}
			if rt.check != nil {
				rt.check(t, res)
			}
		})
	}
}

// TestErrorMapping covers every service error kind once; it is the same
// route wrapper for every endpoint.
func TestErrorMapping(t *testing.T) {
	cases := []struct {
		err    error
		status int
		msg    string
	}{
		{services.ErrUserNotFound, 404, "User not found"},
		{fmt.Errorf("auth: %w", services.ErrEmailTaken), 409, "Email is already in use"},
		{services.ErrInvalidPassword, 400, "Current password is incorrect"},
		{services.ErrRateLimited, 429, "Too many requests. Please try again later."},
		{&services.Error{Kind: services.KindUnauthorized, Message: "Nope"}, 401, "Nope"},
		{&services.Error{Kind: services.KindInternal, Message: "secret detail"}, 500, "Internal server error"},
		{errors.New("pq: connection refused"), 500, "Internal server error"}, // never leaks
	}
	for _, c := range cases {
		svc, h := newRouter(t)
		svc.EXPECT().DeleteAccount(anyCtx, actor, "u1").Return(c.err)
		wantError(t, apitest.Do(t, h, apitest.AsUser(ada), "DELETE", r.Me, ""), c.status, c.msg)
	}
}

// TestDecode covers validate.Decode and the validators through the routes
// that use them: invalid bodies never reach the service (the mock has no
// expectations) and name the failing field.
func TestDecode(t *testing.T) {
	cases := []struct {
		method, path, body, field string
	}{
		{"PATCH", r.Me, `{"name":"` + strings.Repeat("x", 256) + `"}`, "name"},
		{"PATCH", r.Me, `{"image":"ftp://x/a.png"}`, "image"},
		{"PATCH", r.Me, `{"email":"a@b.c"}`, "body"}, // email changes go through POST /me/email
		{"PATCH", r.Me, `{"name":1}`, "body"},
		{"PATCH", r.Me, ``, "body"},
		{"PATCH", r.Me, `{} {}`, "body"},
		{"PATCH", r.Me, `{`, "body"},
		{"POST", r.MeEmail, `{"newEmail":"Ada <ada@example.com>"}`, "newEmail"},
		{"POST", r.MeEmail, `{"newEmail":"ada@example.com","callbackURL":"/` + strings.Repeat("x", 2048) + `"}`, "callbackURL"},
		{"POST", r.MePassword, `{"currentPassword":"password123"}`, "newPassword"},
	}
	for _, c := range cases {
		_, h := newRouter(t)
		res := apitest.Do(t, h, apitest.AsUser(ada), c.method, c.path, c.body)
		if res.Status != 400 || *res.Error != "Invalid request data" || res.Details[c.field] == "" {
			t.Errorf("%s %s %.40q: %d %v %v, want 400 on %q", c.method, c.path, c.body, res.Status, *res.Error, res.Details, c.field)
		}
	}
}

// TestEmailCallbackDefault: without a callbackURL the link lands on the
// account page.
func TestEmailCallbackDefault(t *testing.T) {
	svc, h := newRouter(t)
	svc.EXPECT().RequestEmailChange(anyCtx, actor, "u1", validate.UpdateUserEmail{NewEmail: "ada@new.example", CallbackURL: config.AppRoutes.EmailChanged})
	if res := apitest.Do(t, h, apitest.AsUser(ada), "POST", r.MeEmail, `{"newEmail":"ada@new.example"}`); res.Status != 200 {
		t.Fatalf("status %d (%s)", res.Status, *res.Error)
	}
}

// TestBodyLimit: the route wrapper caps request bodies.
func TestBodyLimit(t *testing.T) {
	_, h := newRouter(t)
	res := apitest.Do(t, h, apitest.AsUser(ada), "PATCH", r.Me, `{"name":"`+strings.Repeat("x", 2<<20)+`"}`)
	if res.Status != 400 || res.Details["body"] == "" {
		t.Fatalf("oversized body: %d %v", res.Status, res.Details)
	}
}
