package api_test

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"go.uber.org/mock/gomock"

	"github.com/jayleaton/go-fast-stack/apps/web/internal/api"
	"github.com/jayleaton/go-fast-stack/apps/web/internal/api/apitest"
	"github.com/jayleaton/go-fast-stack/apps/web/internal/api/mocks"
	"github.com/jayleaton/go-fast-stack/apps/web/internal/config"
	"github.com/jayleaton/go-fast-stack/apps/web/internal/services"
	"github.com/jayleaton/go-fast-stack/apps/web/internal/validate"
)

var (
	ada       = services.User{ID: "u1", Name: "Ada", Email: "ada@example.com"}
	actor     = apitest.Actor(ada)
	apiRoutes = config.APIRoutes
	anyCtx    = gomock.Any()
)

func newRouter(test *testing.T) (*mocks.MockUserService, http.Handler) {
	svc := mocks.NewMockUserService(gomock.NewController(test))
	mux := http.NewServeMux()
	api.NewUsers(svc, apitest.Gate()).Routes(mux)
	return svc, mux
}

func ptr[Value any](value Value) *Value { return &value }

// route is one endpoint. expect registers the service call a well-formed
// request makes, answering err (and a success value when err is nil);
// invalid is a body the validator rejects, or "" for endpoints without one.
type route struct {
	method, path, body string
	expect             func(recorder *mocks.MockUserServiceMockRecorder, err error)
	invalid            string
	check              func(test *testing.T, res apitest.Response)
}

var routes = map[string]route{
	"GET me": {
		method: "GET", path: apiRoutes.Me,
		expect: func(recorder *mocks.MockUserServiceMockRecorder, err error) {
			recorder.GetProfile(anyCtx, actor, "u1").Return(ada, err)
		},
		check: func(test *testing.T, res apitest.Response) {
			got := apitest.Data[api.CurrentUser](test, res)
			if got.User.Email != ada.Email || got.Session.ID != actor.SessionID {
				test.Errorf("data = %+v", got)
			}
		},
	},
	"PATCH me": {
		method: "PATCH", path: apiRoutes.Me, body: `{"name":"  Ada L  "}`, invalid: `{"name":""}`,
		expect: func(recorder *mocks.MockUserServiceMockRecorder, err error) {
			recorder.UpdateProfile(anyCtx, actor, "u1", validate.UpdateUserSettings{Name: ptr("Ada L")}).Return(services.User{ID: "u1", Name: "Ada L"}, err)
		},
		check: func(test *testing.T, res apitest.Response) {
			if got := apitest.Data[api.UserUpdated](test, res); !got.Success || got.User.Name != "Ada L" {
				test.Errorf("data = %+v", got)
			}
		},
	},
	"PATCH user by id": {
		method: "PATCH", path: config.UserPath("u2"), body: `{"image":null}`, invalid: `{"image":"javascript:alert(1)"}`,
		expect: func(recorder *mocks.MockUserServiceMockRecorder, err error) {
			recorder.UpdateProfile(anyCtx, actor, "u2", validate.UpdateUserSettings{Image: validate.Nullable[string]{Set: true}}).Return(services.User{ID: "u2"}, err)
		},
	},
	"DELETE me": {
		method: "DELETE", path: apiRoutes.Me,
		expect: func(recorder *mocks.MockUserServiceMockRecorder, err error) {
			recorder.DeleteAccount(anyCtx, actor, "u1").Return(err)
		},
		check: wantSuccess,
	},
	"GET me data": {
		method: "GET", path: apiRoutes.MeData,
		expect: func(recorder *mocks.MockUserServiceMockRecorder, err error) {
			recorder.DataSummary(anyCtx, actor, "u1").Return(services.UserDataSummary{User: ada, Sessions: 2}, err)
		},
		check: func(test *testing.T, res apitest.Response) {
			if got := apitest.Data[services.UserDataSummary](test, res); got.Sessions != 2 || got.User.ID != "u1" {
				test.Errorf("data = %+v", got)
			}
		},
	},
	"POST me email": {
		method: "POST", path: apiRoutes.MeEmail, body: `{"newEmail":" Ada@New.Example ","callbackURL":"/dashboard"}`, invalid: `{"newEmail":"not an email"}`,
		expect: func(recorder *mocks.MockUserServiceMockRecorder, err error) {
			recorder.RequestEmailChange(anyCtx, actor, "u1", validate.UpdateUserEmail{NewEmail: "ada@new.example", CallbackURL: "/dashboard"}).Return(err)
		},
		check: wantSuccess,
	},
	"POST me password": {
		method: "POST", path: apiRoutes.MePassword, body: `{"currentPassword":"password123","newPassword":"password456","revokeOtherSessions":true}`, invalid: `{"newPassword":"password456"}`,
		expect: func(recorder *mocks.MockUserServiceMockRecorder, err error) {
			recorder.ChangePassword(anyCtx, actor, "u1", validate.UpdateUserPassword{CurrentPassword: "password123", NewPassword: "password456", RevokeOtherSessions: true}).Return(err)
		},
		check: wantSuccess,
	},
	"GET me sessions": {
		method: "GET", path: apiRoutes.MeSessions,
		expect: func(recorder *mocks.MockUserServiceMockRecorder, err error) {
			recorder.ListSessions(anyCtx, actor, "u1").Return([]services.Session{{ID: actor.SessionID, Current: true}, {ID: "s2"}}, err)
		},
		check: func(test *testing.T, res apitest.Response) {
			if got := apitest.Data[[]services.Session](test, res); len(got) != 2 || !got[0].Current || strings.Contains(string(res.Data), "token") {
				test.Errorf("data = %s", res.Data)
			}
		},
	},
	"DELETE me session": {
		method: "DELETE", path: config.MeSessionPath("s2"),
		expect: func(recorder *mocks.MockUserServiceMockRecorder, err error) {
			recorder.RevokeSession(anyCtx, actor, "u1", "s2").Return(err)
		},
		check: wantSuccess,
	},
	"DELETE me other sessions": {
		method: "DELETE", path: apiRoutes.MeOtherSessions,
		expect: func(recorder *mocks.MockUserServiceMockRecorder, err error) {
			recorder.RevokeOtherSessions(anyCtx, actor, "u1").Return(err)
		},
		check: wantSuccess,
	},
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

// TestUserRoutes runs the four boundary cases against every endpoint. The
// mock fails the test on any service call a case does not expect.
func TestUserRoutes(test *testing.T) {
	test.Parallel()
	for name, rt := range routes {
		test.Run(name+"/no session is 401", func(test *testing.T) {
			test.Parallel()
			_, handler := newRouter(test)
			wantError(test, apitest.Do(test, handler, apitest.Anonymous, rt.method, rt.path, rt.body), 401, "Unauthorized")
		})
		test.Run(name+"/other user is 403", func(test *testing.T) {
			test.Parallel()
			svc, handler := newRouter(test)
			rt.expect(svc.EXPECT(), services.ErrForbidden)
			wantError(test, apitest.Do(test, handler, apitest.AsUser(ada), rt.method, rt.path, rt.body), 403, "Forbidden")
		})
		test.Run(name+"/invalid input is 400", func(test *testing.T) {
			test.Parallel()
			svc, handler := newRouter(test)
			body := rt.invalid
			if body == "" { // no body to get wrong: the service rejects the request
				body = rt.body
				rt.expect(svc.EXPECT(), services.ErrValidation)
			}
			res := apitest.Do(test, handler, apitest.AsUser(ada), rt.method, rt.path, body)
			if res.Status != 400 {
				test.Errorf("status %d, want 400 (%s)", res.Status, *res.Error)
			}
		})
		test.Run(name+"/ok is 200", func(test *testing.T) {
			test.Parallel()
			svc, handler := newRouter(test)
			rt.expect(svc.EXPECT(), nil)
			res := apitest.Do(test, handler, apitest.AsUser(ada), rt.method, rt.path, rt.body)
			if res.Status != 200 {
				test.Fatalf("status %d (%s)", res.Status, *res.Error)
			}
			if rt.check != nil {
				rt.check(test, res)
			}
		})
	}
}

// TestErrorMapping covers every service error kind once; it is the same
// route wrapper for every endpoint.
func TestErrorMapping(test *testing.T) {
	test.Parallel()
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
	for _, testCase := range cases {
		svc, handler := newRouter(test)
		svc.EXPECT().DeleteAccount(anyCtx, actor, "u1").Return(testCase.err)
		wantError(test, apitest.Do(test, handler, apitest.AsUser(ada), "DELETE", apiRoutes.Me, ""), testCase.status, testCase.msg)
	}
}

// TestDecode covers validate.Decode and the validators through the routes
// that use them: invalid bodies never reach the service (the mock has no
// expectations) and name the failing field.
func TestDecode(test *testing.T) {
	test.Parallel()
	cases := []struct {
		method, path, body, field string
	}{
		{"PATCH", apiRoutes.Me, `{"name":"` + strings.Repeat("x", 256) + `"}`, "name"},
		{"PATCH", apiRoutes.Me, `{"image":"ftp://x/a.png"}`, "image"},
		{"PATCH", apiRoutes.Me, `{"email":"a@b.c"}`, "body"}, // email changes go through POST /me/email
		{"PATCH", apiRoutes.Me, `{"name":1}`, "body"},
		{"PATCH", apiRoutes.Me, ``, "body"},
		{"PATCH", apiRoutes.Me, `{} {}`, "body"},
		{"PATCH", apiRoutes.Me, `{`, "body"},
		{"POST", apiRoutes.MeEmail, `{"newEmail":"Ada <ada@example.com>"}`, "newEmail"},
		{"POST", apiRoutes.MeEmail, `{"newEmail":"ada@example.com","callbackURL":"/` + strings.Repeat("x", 2048) + `"}`, "callbackURL"},
		{"POST", apiRoutes.MePassword, `{"currentPassword":"password123"}`, "newPassword"},
	}
	for _, testCase := range cases {
		_, handler := newRouter(test)
		res := apitest.Do(test, handler, apitest.AsUser(ada), testCase.method, testCase.path, testCase.body)
		if res.Status != 400 || *res.Error != "Invalid request data" || res.Details[testCase.field] == "" {
			test.Errorf("%s %s %.40q: %d %v %v, want 400 on %q", testCase.method, testCase.path, testCase.body, res.Status, *res.Error, res.Details, testCase.field)
		}
	}
}

// TestDecodeNamesJSONTypes: a wrong type is described in JSON terms, never
// with the Go type it decodes into.
func TestDecodeNamesJSONTypes(test *testing.T) {
	test.Parallel()
	for body, want := range map[string]string{
		`{"name":1}`:      "name must be a string",
		`{"name":["x"]}`:  "name must be a string",
		`{"image":false}`: "image must be a string",
	} {
		_, handler := newRouter(test)
		res := apitest.Do(test, handler, apitest.AsUser(ada), "PATCH", apiRoutes.Me, body)
		if res.Status != 400 || res.Details["body"] != want {
			test.Errorf("%s: %d %v, want body %q", body, res.Status, res.Details, want)
		}
	}
}

// TestEmailCallbackDefault: without a callbackURL the link lands on the
// account page.
func TestEmailCallbackDefault(test *testing.T) {
	test.Parallel()
	svc, handler := newRouter(test)
	svc.EXPECT().RequestEmailChange(anyCtx, actor, "u1", validate.UpdateUserEmail{NewEmail: "ada@new.example", CallbackURL: config.AppRoutes.EmailChanged})
	if res := apitest.Do(test, handler, apitest.AsUser(ada), "POST", apiRoutes.MeEmail, `{"newEmail":"ada@new.example"}`); res.Status != 200 {
		test.Fatalf("status %d (%s)", res.Status, *res.Error)
	}
}

// TestBodyLimit: the route wrapper caps request bodies.
func TestBodyLimit(test *testing.T) {
	test.Parallel()
	_, handler := newRouter(test)
	res := apitest.Do(test, handler, apitest.AsUser(ada), "PATCH", apiRoutes.Me, `{"name":"`+strings.Repeat("x", 2<<20)+`"}`)
	if res.Status != 400 || res.Details["body"] == "" {
		test.Fatalf("oversized body: %d %v", res.Status, res.Details)
	}
}
