package api_test

import (
	"errors"
	"fmt"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/jayleaton/go-fast-stack/apps/admin/internal/api/apitest"
	"github.com/jayleaton/go-fast-stack/apps/admin/internal/config"
	"github.com/jayleaton/go-fast-stack/apps/admin/internal/services"
	"github.com/jayleaton/go-fast-stack/apps/admin/internal/validate"
)

var (
	ada = services.User{ID: "u2", Name: "Ada", Email: "ada@example.com", Role: "user", CreatedAt: time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)}

	// listDefaults is what GET /users with no query asks the service for.
	listDefaults = validate.ListUsers{SearchField: "email", SortBy: "createdAt", SortDirection: "desc", Limit: validate.DefaultLimit}

	notFound   = svcErr{services.ErrUserNotFound, 404, "User not found"}
	notBanned  = svcErr{services.ErrUserNotBanned, 400, "The user is not banned"}
	badRole    = svcErr{services.ErrInvalidRole, 400, "Unknown role"}
	sameRole   = svcErr{services.ErrRoleUnchanged, 400, "The user already has this role"}
	noSession  = svcErr{services.ErrSessionNotFound, 404, "Session not found"}
	reasonRule = "must be 1-500 characters"
	expiryRule = "must be 0 (permanent) or 60 seconds to 10 years"
	roleRule   = "must be a role name"
)

// unavailable is every mutation refused because its audit intent could not
// be written (wrapped with the cause, as services.mutate returns it).
var unavailable = svcErr{
	fmt.Errorf("%w: audit intent: %w", services.ErrAuditUnavailable, errors.New("pq: connection refused")),
	503, "The audit log is unavailable, so nothing was changed. Try again later.",
}

var userRoutes = map[string]route{
	"GET users": {
		method: "GET", path: routes.Users, invalidPath: routes.Users + "?limit=1000", field: "limit",
		expect: func(svcs svcs, err error) {
			svcs.users.ListUsers(anyCtx, actor, listDefaults).Return(services.UserPage{Users: []services.User{ada}, Total: 1, Limit: 25}, err)
		},
		check: func(test *testing.T, res apitest.Response) {
			got := apitest.Data[services.UserPage](test, res)
			if len(got.Users) != 1 || got.Users[0].Email != ada.Email || got.Total != 1 || got.Limit != 25 {
				test.Errorf("data = %s", res.Data)
			}
		},
	},
	"GET user": {
		method: "GET", path: config.UserPath("user 2", ""), errs: []svcErr{notFound},
		expect: func(svcs svcs, err error) { // the path id arrives unescaped
			svcs.users.GetUser(anyCtx, actor, "user 2").Return(services.UserDetail{User: ada, Sessions: []services.Session{{ID: "s1"}, {ID: "s2"}}}, err)
		},
		check: func(test *testing.T, res apitest.Response) {
			got := apitest.Data[services.UserDetail](test, res)
			if got.User.ID != "u2" || len(got.Sessions) != 2 || got.Sessions[1].ID != "s2" || strings.Contains(string(res.Data), "token") {
				test.Errorf("data = %s", res.Data)
			}
		},
	},
	"POST user ban": {
		method: "POST", path: config.UserPath("u2", "/ban"), body: `{"banReason":"  spam  ","banExpiresIn":3600}`,
		invalidBody: `{"banReason":"   "}`, field: "banReason", errs: []svcErr{notFound, unavailable},
		expect: func(svcs svcs, err error) {
			svcs.users.BanUser(anyCtx, actor, "u2", validate.BanUser{BanReason: "spam", BanExpiresIn: ptr(int64(3600))}).Return(err)
		},
		check: wantSuccess,
	},
	"POST user unban": {
		method: "POST", path: config.UserPath("u2", "/unban"), errs: []svcErr{notFound, notBanned, unavailable},
		expect: func(svcs svcs, err error) {
			svcs.users.UnbanUser(anyCtx, actor, "u2").Return(err)
		},
		check: wantSuccess,
	},
	"PUT user role": {
		method: "PUT", path: config.UserPath("u2", "/role"), body: `{"role":" admin "}`,
		invalidBody: `{"role":"Admin"}`, field: "role", errs: []svcErr{notFound, badRole, sameRole, unavailable},
		expect: func(svcs svcs, err error) {
			svcs.users.SetRole(anyCtx, actor, "u2", validate.SetUserRole{Role: "admin"}).Return(err)
		},
		check: wantSuccess,
	},
	"DELETE user sessions": {
		method: "DELETE", path: config.UserPath("u2", "/sessions"), errs: []svcErr{notFound, unavailable},
		expect: func(svcs svcs, err error) {
			svcs.users.RevokeSessions(anyCtx, actor, "u2").Return(err)
		},
		check: wantSuccess,
	},
	"DELETE user session": {
		method: "DELETE", path: config.UserSessionPath("u2", "s2"), errs: []svcErr{notFound, noSession, unavailable},
		expect: func(svcs svcs, err error) {
			svcs.users.RevokeSession(anyCtx, actor, "u2", "s2").Return(err)
		},
		check: wantSuccess,
	},
}

// TestListUsersQuery: every query parameter is decoded, trimmed where it is
// free text, and defaulted, and reaches the service as given.
func TestListUsersQuery(test *testing.T) {
	test.Parallel()
	cases := []struct {
		values url.Values
		want   validate.ListUsers
	}{
		{url.Values{}, listDefaults},
		{url.Values{"limit": {"0"}, "offset": {"0"}}, listDefaults},
		{
			url.Values{
				"search": {"  ada  "}, "searchField": {"name"}, "role": {" admin "}, "banned": {"true"},
				"sortBy": {"email"}, "sortDirection": {"asc"}, "limit": {"100"}, "offset": {"50"},
			},
			validate.ListUsers{Search: "ada", SearchField: "name", Role: "admin", Banned: ptr(true), SortBy: "email", SortDirection: "asc", Limit: 100, Offset: 50},
		},
		{
			url.Values{"banned": {"false"}, "sortBy": {"name"}, "limit": {"1"}, "role": {"super_user-2"}},
			validate.ListUsers{SearchField: "email", Role: "super_user-2", Banned: ptr(false), SortBy: "name", SortDirection: "desc", Limit: 1},
		},
		{
			url.Values{"banned": {"0"}, "sortDirection": {"desc"}, "searchField": {"email"}, "sortBy": {"createdAt"}},
			validate.ListUsers{SearchField: "email", Banned: ptr(false), SortBy: "createdAt", SortDirection: "desc", Limit: 25},
		},
		{ // the bounds are inclusive; length counts characters, not bytes
			url.Values{"search": {strings.Repeat("é", 255)}, "role": {strings.Repeat("r", 32)}},
			validate.ListUsers{Search: strings.Repeat("é", 255), SearchField: "email", Role: strings.Repeat("r", 32), SortBy: "createdAt", SortDirection: "desc", Limit: 25},
		},
	}
	for _, testCase := range cases {
		wantOK(test, "GET", routes.Users+"?"+testCase.values.Encode(), "", func(svcs svcs) {
			svcs.users.ListUsers(anyCtx, actor, testCase.want)
		})
	}
}

// TestListUsersInvalid covers every ListUsers rule: bad queries never reach
// the service and name each failing parameter.
func TestListUsersInvalid(test *testing.T) {
	test.Parallel()
	limit := map[string]string{"limit": "must be 1-100"}
	whole := func(key string) map[string]string { return map[string]string{key: "must be a whole number"} }
	cases := []struct {
		query string
		want  map[string]string
	}{
		{"limit=1000", limit},
		{"limit=101", limit},
		{"limit=-1", limit},
		{"limit=abc", whole("limit")},
		{"offset=1.5", whole("offset")},
		{"offset=-1", map[string]string{"offset": "must be 0 or more"}},
		{"banned=maybe", map[string]string{"banned": "must be true or false"}},
		{"banned=maybe&limit=x", map[string]string{"banned": "must be true or false", "limit": "must be a whole number"}},
		{"search=" + strings.Repeat("x", 256), map[string]string{"search": "must be at most 255 characters"}},
		{"searchField=id", map[string]string{"searchField": "must be email or name"}},
		{"role=Admin", map[string]string{"role": roleRule}},
		{"role=a+b", map[string]string{"role": roleRule}},
		{"role=adm!n", map[string]string{"role": roleRule}},
		{"role=" + strings.Repeat("r", 33), map[string]string{"role": roleRule}},
		{"sortBy=updatedAt", map[string]string{"sortBy": "must be createdAt, email or name"}},
		{"sortDirection=up", map[string]string{"sortDirection": "must be asc or desc"}},
		{"sortBy=x&sortDirection=y&offset=-2", map[string]string{"sortBy": "must be createdAt, email or name", "sortDirection": "must be asc or desc", "offset": "must be 0 or more"}},
	}
	for _, testCase := range cases {
		wantInvalid(test, "GET", routes.Users+"?"+testCase.query, "", testCase.want)
	}
}

// TestBanUserBody covers every BanUser rule and its boundaries.
func TestBanUserBody(test *testing.T) {
	test.Parallel()
	path := config.UserPath("u2", "/ban")
	for _, body := range []string{
		`{}`,
		`{"banReason":""}`,
		`{"banReason":" \t "}`,
		`{"banReason":"` + strings.Repeat("é", 501) + `"}`,
	} {
		wantInvalid(test, "POST", path, body, map[string]string{"banReason": reasonRule})
	}
	for _, size := range []int64{-1, 1, 59, validate.MaxBanSeconds + 1} {
		wantInvalid(test, "POST", path, fmt.Sprintf(`{"banReason":"spam","banExpiresIn":%d}`, size), map[string]string{"banExpiresIn": expiryRule})
	}
	wantInvalid(test, "POST", path, `{"banExpiresIn":5}`, map[string]string{"banReason": reasonRule, "banExpiresIn": expiryRule})

	valid := []struct {
		body string
		want validate.BanUser
	}{
		{`{"banReason":"spam"}`, validate.BanUser{BanReason: "spam"}},
		{`{"banReason":"spam","banExpiresIn":null}`, validate.BanUser{BanReason: "spam"}},
		{`{"banReason":"spam","banExpiresIn":0}`, validate.BanUser{BanReason: "spam", BanExpiresIn: ptr(int64(0))}},
		{`{"banReason":"spam","banExpiresIn":60}`, validate.BanUser{BanReason: "spam", BanExpiresIn: ptr(int64(60))}},
		{fmt.Sprintf(`{"banReason":"spam","banExpiresIn":%d}`, validate.MaxBanSeconds), validate.BanUser{BanReason: "spam", BanExpiresIn: ptr(int64(validate.MaxBanSeconds))}},
		{`{"banReason":"` + strings.Repeat("é", 500) + `"}`, validate.BanUser{BanReason: strings.Repeat("é", 500)}},
	}
	for _, testCase := range valid {
		wantOK(test, "POST", path, testCase.body, func(svcs svcs) {
			svcs.users.BanUser(anyCtx, actor, "u2", testCase.want)
		})
	}
}

// TestSetUserRoleBody covers the role shape (which roles exist is the
// service's rule, so any well-formed name reaches it).
func TestSetUserRoleBody(test *testing.T) {
	test.Parallel()
	path := config.UserPath("u2", "/role")
	for _, body := range []string{
		`{}`,
		`{"role":"  "}`,
		`{"role":"Admin"}`,
		`{"role":"a b"}`,
		`{"role":"adm!n"}`,
		`{"role":"rôle"}`,
		`{"role":"` + strings.Repeat("r", 33) + `"}`,
	} {
		wantInvalid(test, "PUT", path, body, map[string]string{"role": roleRule})
	}
	for body, want := range map[string]string{
		`{"role":" admin "}`:                         "admin",
		`{"role":"super_user-2"}`:                    "super_user-2",
		`{"role":"` + strings.Repeat("r", 32) + `"}`: strings.Repeat("r", 32),
	} {
		wantOK(test, "PUT", path, body, func(svcs svcs) {
			svcs.users.SetRole(anyCtx, actor, "u2", validate.SetUserRole{Role: want})
		})
	}
}
