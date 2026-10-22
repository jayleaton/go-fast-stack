package ui_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"go.uber.org/mock/gomock"

	"github.com/jayleaton/ai-patterns-go/apps/admin/internal/config"
	"github.com/jayleaton/ai-patterns-go/apps/admin/internal/services"
	"github.com/jayleaton/ai-patterns-go/apps/admin/internal/validate"
	"github.com/jayleaton/ai-patterns-go/apps/admin/ui"
	"github.com/jayleaton/ai-patterns-go/apps/admin/ui/mocks"
	"github.com/jayleaton/ai-patterns-go/apps/admin/ui/routes"
)

var (
	root    = ui.Admin{ID: "a1", Name: "Root", Email: "root@example.com", TwoFactorEnabled: true}
	rootSes = ui.Session{ID: "s1", Token: "tok-1"}
	actor   = services.Actor{AdminID: "a1", Email: "root@example.com", SessionID: "s1", TwoFactorEnabled: true}
	newSes  = ui.Session{ID: "s2", Token: "tok-2"}
	errBoom = errors.New("boom")
	// errAudit is a mutation refused because its audit intent could not be
	// written, wrapped with the cause as services.mutate returns it.
	errAudit = fmt.Errorf("%w: audit intent: %w", services.ErrAuditUnavailable, errBoom)
	// errApplied: the change was applied but its outcome was not recorded
	// (an internal error the admin sees as "something went wrong").
	errApplied = fmt.Errorf("audit outcome of entry 7 NOT recorded; the change WAS applied: %w", errBoom)
)

// Translated alerts for errAudit.
const (
	auditDownEN = "The audit log is unavailable, so nothing was changed."
	auditDownJA = "監査ログを利用できないため、変更は行われませんでした。"
)

func ptr[T any](v T) *T { return &v }

// mock session: Deps.UserFrom reads the admin a request was signed in as.
type sessionKey struct{}

func userFrom(ctx context.Context) (ui.Admin, ui.Session, bool) {
	a, ok := ctx.Value(sessionKey{}).(ui.Admin)
	return a, rootSes, ok
}

type env struct {
	t         *testing.T
	auth      *mocks.MockAuth
	users     *mocks.MockUsers
	admins    *mocks.MockAdmins
	audit     *mocks.MockAudit
	analytics *mocks.MockAnalytics
	h         http.Handler
}

func newEnv(t *testing.T, mod ...func(*ui.Deps)) *env {
	t.Helper()
	ctrl := gomock.NewController(t)
	e := &env{
		t:         t,
		auth:      mocks.NewMockAuth(ctrl),
		users:     mocks.NewMockUsers(ctrl),
		admins:    mocks.NewMockAdmins(ctrl),
		audit:     mocks.NewMockAudit(ctrl),
		analytics: mocks.NewMockAnalytics(ctrl),
	}
	d := ui.Deps{
		Auth: e.auth, Users: e.users, Admins: e.admins, Audit: e.audit, Analytics: e.analytics,
		UserFrom: userFrom,
		AppName:  "Acme Admin",
		Logger:   slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	for _, m := range mod {
		m(&d)
	}
	e.h = ui.New(d).Handler()
	return e
}

type reqOpt func(*http.Request)

// datastar marks the request as a Datastar action (SSE response).
func datastar(r *http.Request) { r.Header.Set("Datastar-Request", "true") }

func lang(l string) reqOpt { return func(r *http.Request) { r.Header.Set("Accept-Language", l) } }

func as(a ui.Admin) reqOpt {
	return func(r *http.Request) { *r = *r.WithContext(context.WithValue(r.Context(), sessionKey{}, a)) }
}

var signedIn = as(root)

func (e *env) do(method, target string, form url.Values, opts ...reqOpt) *httptest.ResponseRecorder {
	e.t.Helper()
	var body io.Reader
	if form != nil {
		body = strings.NewReader(form.Encode())
	}
	r := httptest.NewRequest(method, target, body)
	if form != nil {
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	for _, o := range opts {
		o(r)
	}
	rec := httptest.NewRecorder()
	e.h.ServeHTTP(rec, r)
	return rec
}

// assertSSE checks a Datastar response carries element patches containing want.
func assertSSE(t *testing.T, rec *httptest.ResponseRecorder, want ...string) {
	t.Helper()
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("content-type %q, want SSE; body:\n%s", ct, rec.Body)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "event: datastar-patch-elements") {
		t.Fatalf("no patch-elements event:\n%s", body)
	}
	for _, w := range want {
		if !strings.Contains(body, w) {
			t.Errorf("SSE body lacks %q:\n%s", w, body)
		}
	}
}

func assertRedirectScript(t *testing.T, rec *httptest.ResponseRecorder, to string) {
	t.Helper()
	assertSSE(t, rec, `window.location.href = "`+to+`"`)
}

func assertSeeOther(t *testing.T, rec *httptest.ResponseRecorder, to string) {
	t.Helper()
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != to {
		t.Fatalf("got %d %q, want 303 %q; body:\n%s", rec.Code, rec.Header().Get("Location"), to, rec.Body)
	}
}

func assertPage(t *testing.T, rec *httptest.ResponseRecorder, status int, want ...string) {
	t.Helper()
	if rec.Code != status {
		t.Fatalf("status %d, want %d; body:\n%s", rec.Code, status, rec.Body)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Fatalf("content-type %q", ct)
	}
	for _, w := range want {
		if !strings.Contains(rec.Body.String(), w) {
			t.Errorf("page lacks %q", w)
		}
	}
}

// --- fixtures ---

var created = time.Date(2026, 1, 2, 3, 4, 0, 0, time.UTC)

func appUser(id, email string) services.User {
	return services.User{ID: id, Name: "Ada", Email: email, Role: "user", CreatedAt: created, UpdatedAt: created, EmailVerified: true}
}

func bannedDetail() services.UserDetail {
	u := appUser("u1", "ada@example.com")
	u.Banned, u.BanReason, u.BanExpires = true, ptr("spamming"), ptr(created.Add(24*time.Hour))
	return services.UserDetail{User: u, Sessions: []services.Session{
		{ID: "us1", CreatedAt: created, ExpiresAt: created.Add(time.Hour), IPAddress: ptr("10.0.0.7"), UserAgent: ptr("Firefox/140")},
	}}
}

// expectPages makes every page's service call succeed.
func (e *env) expectPages() {
	e.users.EXPECT().ListUsers(gomock.Any(), actor, gomock.Any()).Return(services.UserPage{Users: []services.User{appUser("u1", "ada@example.com")}, Total: 1, Limit: 25}, nil).AnyTimes()
	e.users.EXPECT().GetUser(gomock.Any(), actor, "u1").Return(bannedDetail(), nil).AnyTimes()
	e.admins.EXPECT().ListAdmins(gomock.Any(), actor).Return([]services.Admin{{ID: "a1", Email: "root@example.com", HasPassword: true, TwoFactorEnabled: true}, {ID: "a2", Email: "new@example.com"}}, nil).AnyTimes()
	e.audit.EXPECT().List(gomock.Any(), actor, gomock.Any()).Return(services.AuditPage{Entries: []services.AuditEntry{{ID: 1, Action: services.ActionUserBan, ActorEmail: "root@example.com", TargetType: services.TargetUser, TargetID: "u1", TargetLabel: "ada@example.com", CreatedAt: created}}, Total: 1, Limit: 25}, nil).AnyTimes()
	e.analytics.EXPECT().Overview(gomock.Any(), actor).Return(services.Overview{TotalUsers: 42, GeneratedAt: created}, nil).AnyTimes()
}

// dashboardPages are the signed-in pages with their nav section heading.
var dashboardPages = []struct {
	path       string
	en, ja     string
	navCurrent string
}{
	{config.AppRoutes.Users, "Users", "ユーザー", config.AppRoutes.Users},
	{config.UserPage("u1"), "spamming", "利用停止中", config.AppRoutes.Users},
	{config.AppRoutes.Admins, "Invite pending", "招待中", config.AppRoutes.Admins},
	{config.AppRoutes.Audit, "Banned user", "ユーザーを利用停止", config.AppRoutes.Audit},
	{config.AppRoutes.Analytics, "Total users", "総ユーザー数", config.AppRoutes.Analytics},
}

func TestPagesSignedOutRedirectToLogin(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	for _, p := range dashboardPages {
		assertSeeOther(t, e.do("GET", p.path, nil), config.AppRoutes.Login)
	}
	assertSeeOther(t, e.do("GET", config.AppRoutes.Home, nil), config.AppRoutes.Login)
}

func TestPagesSignedIn(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.expectPages()
	for _, l := range []string{"en", "ja"} {
		for _, p := range dashboardPages {
			rec := e.do("GET", p.path, nil, signedIn, lang(l))
			want := p.en
			if l == "ja" {
				want = p.ja
			}
			assertPage(t, rec, http.StatusOK, want, "root@example.com", "Acme Admin", `<html lang="`+l+`"`,
				`href="`+p.navCurrent+`" aria-current="page"`)
			if got := rec.Header().Get("Content-Language"); got != l {
				t.Errorf("%s %s: Content-Language %q", l, p.path, got)
			}
			if rec.Header().Get("Cache-Control") != "no-store" {
				t.Errorf("%s: signed-in page cacheable", p.path)
			}
		}
	}
	assertSeeOther(t, e.do("GET", config.AppRoutes.Home, nil, signedIn), config.AppRoutes.Users)
	assertSeeOther(t, e.do("GET", config.AppRoutes.Login, nil, signedIn), config.AppRoutes.Users)
}

func TestPublicPages(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	for _, l := range []string{"en", "ja"} {
		for _, p := range []string{config.AppRoutes.Login, config.AppRoutes.SetPassword + "?token=t", config.AppRoutes.SetPassword, config.AppRoutes.TwoFactor} {
			rec := e.do("GET", p, nil, lang(l))
			assertPage(t, rec, http.StatusOK, `<html lang="`+l+`"`, "<title>")
			if strings.Contains(rec.Body.String(), `aria-current="page"`) {
				t.Errorf("%s: nav shown signed out", p)
			}
		}
	}
	page := e.do("GET", config.AppRoutes.Login, nil).Body.String()
	if strings.Contains(strings.ToLower(page), "sign up") {
		t.Error("login page offers sign-up")
	}
	if rec := e.do("GET", "/favicon.ico", nil); rec.Code != http.StatusMovedPermanently {
		t.Errorf("favicon: %d", rec.Code)
	}
	if rec := e.do("GET", "/static/favicon.ico", nil); rec.Code != http.StatusOK {
		t.Errorf("static: %d", rec.Code)
	}
}

func TestPagesTwoFactorRequired(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.users.EXPECT().ListUsers(gomock.Any(), gomock.Any(), gomock.Any()).Return(services.UserPage{}, services.ErrTwoFactorRequired)
	e.users.EXPECT().GetUser(gomock.Any(), gomock.Any(), "u1").Return(services.UserDetail{}, services.ErrTwoFactorRequired)
	e.admins.EXPECT().ListAdmins(gomock.Any(), gomock.Any()).Return(nil, services.ErrTwoFactorRequired)
	e.audit.EXPECT().List(gomock.Any(), gomock.Any(), gomock.Any()).Return(services.AuditPage{}, services.ErrTwoFactorRequired)
	e.analytics.EXPECT().Overview(gomock.Any(), gomock.Any()).Return(services.Overview{}, services.ErrTwoFactorRequired)
	for _, p := range dashboardPages {
		assertSeeOther(t, e.do("GET", p.path, nil, signedIn), config.AppRoutes.TwoFactorSetup)
	}
}

func TestPageServiceErrors(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.users.EXPECT().ListUsers(gomock.Any(), actor, gomock.Any()).Return(services.UserPage{}, services.ErrForbidden)
	assertPage(t, e.do("GET", config.AppRoutes.Users, nil, signedIn, lang("ja")), http.StatusForbidden, "この操作は許可されていません。")
	// Invalid queries never reach the service.
	assertPage(t, e.do("GET", config.AppRoutes.Users+"?limit=abc", nil, signedIn), http.StatusBadRequest, "Please check your input")
	assertPage(t, e.do("GET", config.AppRoutes.Audit+"?offset=-1", nil, signedIn), http.StatusBadRequest, "Please check your input")

	e.users.EXPECT().GetUser(gomock.Any(), actor, "nope").Return(services.UserDetail{}, services.ErrUserNotFound)
	assertPage(t, e.do("GET", config.UserPage("nope"), nil, signedIn), http.StatusNotFound, "User not found")
	e.admins.EXPECT().ListAdmins(gomock.Any(), actor).Return(nil, errBoom)
	assertPage(t, e.do("GET", config.AppRoutes.Admins, nil, signedIn), http.StatusInternalServerError, "Something went wrong")
	e.audit.EXPECT().List(gomock.Any(), actor, gomock.Any()).Return(services.AuditPage{}, &services.Error{Kind: services.KindConflict, Message: "x"})
	assertPage(t, e.do("GET", config.AppRoutes.Audit, nil, signedIn), http.StatusConflict, "Something went wrong")
	e.analytics.EXPECT().Overview(gomock.Any(), actor).Return(services.Overview{}, errBoom)
	assertPage(t, e.do("GET", config.AppRoutes.Analytics, nil, signedIn, lang("ja")), http.StatusInternalServerError, "問題が発生しました")
}

func TestUsersListQuery(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	users := make([]services.User, 25)
	for i := range users {
		users[i] = appUser("u", "user@example.com")
	}
	e.users.EXPECT().ListUsers(gomock.Any(), actor, validate.ListUsers{
		Search: "ada", SearchField: "name", Role: "admin", Banned: ptr(true),
		SortBy: "email", SortDirection: "asc", Limit: 25, Offset: 25,
	}).Return(services.UserPage{Users: users, Total: 60, Limit: 25, Offset: 25}, nil)
	rec := e.do("GET", config.AppRoutes.Users+"?search=ada&searchField=name&banned=true&role=admin&sortBy=email&sortDirection=asc&offset=25", nil, signedIn)
	assertPage(t, rec, http.StatusOK,
		"Showing 26–50 of 60",
		// prev drops the offset, next moves on by the page, both keep the filters
		`href="/users?banned=true&amp;role=admin&amp;search=ada&amp;searchField=name&amp;sortBy=email&amp;sortDirection=asc"`,
		`href="/users?banned=true&amp;offset=50&amp;role=admin&amp;search=ada&amp;searchField=name&amp;sortBy=email&amp;sortDirection=asc"`,
		// the active column flips direction; others start fresh
		`sortBy=email&amp;sortDirection=desc"`, `sortBy=createdAt&amp;sortDirection=desc"`, "Email ↑",
		`value="ada"`, `<option value="name" selected>`, `<option value="true" selected>`,
	)

	e.users.EXPECT().ListUsers(gomock.Any(), actor, validate.ListUsers{SearchField: "email", SortBy: "createdAt", SortDirection: "desc", Limit: 25}).
		Return(services.UserPage{Users: []services.User{}, Limit: 25}, nil)
	assertPage(t, e.do("GET", config.AppRoutes.Users, nil, signedIn, lang("ja")), http.StatusOK, "条件に一致するユーザーはいません。", "0件中 0〜0件を表示")
}

func TestAuditListQuery(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.audit.EXPECT().List(gomock.Any(), actor, validate.ListAuditLog{Action: services.ActionUserSetRole, TargetID: "u1", Limit: 25}).Return(services.AuditPage{
		Entries: []services.AuditEntry{
			{Action: services.ActionUserSetRole, ActorEmail: "root@example.com", TargetType: services.TargetUser, TargetID: "u1", TargetLabel: "ada@example.com", Details: map[string]string{"to": "admin", "from": "user"}, CreatedAt: created},
			{Action: "custom.thing", TargetType: services.TargetAdmin, TargetID: "a9", CreatedAt: created},
		},
		Total: 30, Limit: 25,
	}, nil)
	rec := e.do("GET", config.AppRoutes.Audit+"?action=user.set_role&targetId=u1", nil, signedIn)
	assertPage(t, rec, http.StatusOK, "Changed role", "custom.thing", `href="/users/u1"`, ">ada@example.com</a>", "a9",
		"from: user</li><li>to: admin", `<option value="user.set_role" selected>`,
		`name="targetId" value="u1"`, "Showing 1–2 of 30", `href="/audit?action=user.set_role&amp;offset=2&amp;targetId=u1"`)
}

func TestAnalyticsPage(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.analytics.EXPECT().Overview(gomock.Any(), actor).Return(services.Overview{TotalUsers: 1234, VerifiedUsers: 1000, BannedUsers: 7, NewUsers7d: 55, ActiveSessions: 321, GeneratedAt: created}, nil)
	assertPage(t, e.do("GET", config.AppRoutes.Analytics, nil, signedIn), http.StatusOK, ">1234<", ">1000<", ">7<", ">55<", ">321<", "Jan 2, 2026 03:04 UTC", "placeholder")
}

// --- sign-in flows ---

func TestLogin(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	form := url.Values{"email": {" root@example.com "}, "password": {"pw"}}

	e.auth.EXPECT().SignIn(gomock.Any(), "root@example.com", "pw").Return(ui.Session{}, ui.ErrInvalidCredentials).Times(2)
	assertSSE(t, e.do("POST", config.AppRoutes.Login, form, datastar, lang("ja")), `id="form-error"`, "無効な認証情報")
	assertPage(t, e.do("POST", config.AppRoutes.Login, form), http.StatusUnprocessableEntity, "Invalid credentials", `value="root@example.com"`)

	e.auth.EXPECT().SignIn(gomock.Any(), "root@example.com", "pw").Return(newSes, nil).Times(2)
	e.auth.EXPECT().SetSessionCookie(gomock.Any(), newSes).Times(2)
	assertRedirectScript(t, e.do("POST", config.AppRoutes.Login, form, datastar), config.AppRoutes.Users)
	assertSeeOther(t, e.do("POST", config.AppRoutes.Login, form), config.AppRoutes.Users)

	e.auth.EXPECT().SignIn(gomock.Any(), "root@example.com", "pw").Return(ui.Session{}, ui.ErrTooManyRequests)
	assertSSE(t, e.do("POST", config.AppRoutes.Login, form, datastar), "Too many attempts")
	e.auth.EXPECT().SignIn(gomock.Any(), "root@example.com", "pw").Return(ui.Session{}, errBoom)
	assertSSE(t, e.do("POST", config.AppRoutes.Login, form, datastar), "Something went wrong")
}

func TestSignInChallenges(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	form := url.Values{"email": {"root@example.com"}, "password": {"pw"}}
	second := &ui.TwoFactorChallenge{Token: "c1", Methods: []string{"totp"}}
	setup := &ui.TwoFactorChallenge{Token: "c2", SetupRequired: true}

	e.auth.EXPECT().SignIn(gomock.Any(), "root@example.com", "pw").Return(ui.Session{}, second)
	e.auth.EXPECT().SetTwoFactorCookie(gomock.Any(), second)
	assertRedirectScript(t, e.do("POST", config.AppRoutes.Login, form, datastar), config.AppRoutes.TwoFactor)

	e.auth.EXPECT().SignIn(gomock.Any(), "root@example.com", "pw").Return(ui.Session{}, setup)
	e.auth.EXPECT().SetTwoFactorCookie(gomock.Any(), setup)
	assertSeeOther(t, e.do("POST", config.AppRoutes.Login, form), config.AppRoutes.TwoFactorSetup)

	pw := url.Values{"token": {"inv"}, "password": {"password123"}, "confirmPassword": {"password123"}}
	e.auth.EXPECT().CompleteSetPassword(gomock.Any(), "inv", "password123").Return(ui.Session{}, setup)
	e.auth.EXPECT().SetTwoFactorCookie(gomock.Any(), setup)
	assertRedirectScript(t, e.do("POST", config.AppRoutes.SetPassword, pw, datastar), config.AppRoutes.TwoFactorSetup)

	e.auth.EXPECT().CompleteSetPassword(gomock.Any(), "inv", "password123").Return(ui.Session{}, second)
	e.auth.EXPECT().SetTwoFactorCookie(gomock.Any(), second)
	assertSeeOther(t, e.do("POST", config.AppRoutes.SetPassword, pw), config.AppRoutes.TwoFactor)
}

func TestSetPassword(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	pw := func(token, p, c string) url.Values {
		return url.Values{"token": {token}, "password": {p}, "confirmPassword": {c}}
	}
	// Checked before the auth call.
	assertSSE(t, e.do("POST", config.AppRoutes.SetPassword, pw("", "password123", "password123"), datastar), `id="form-error"`, "This link is invalid")
	assertSSE(t, e.do("POST", config.AppRoutes.SetPassword, pw("t", "password123", "other"), datastar, lang("ja")), "パスワードが一致しません")
	assertPage(t, e.do("POST", config.AppRoutes.SetPassword, pw("t", "password123", "other")), http.StatusUnprocessableEntity, "Passwords don&#39;t match", `name="token" value="t"`)

	e.auth.EXPECT().CompleteSetPassword(gomock.Any(), "t", "password123").Return(ui.Session{}, ui.ErrInvalidToken)
	assertSSE(t, e.do("POST", config.AppRoutes.SetPassword, pw("t", "password123", "password123"), datastar, lang("ja")), "このリンクは無効か")
	e.auth.EXPECT().CompleteSetPassword(gomock.Any(), "t", "short").Return(ui.Session{}, ui.ErrPasswordPolicy)
	assertSSE(t, e.do("POST", config.AppRoutes.SetPassword, pw("t", "short", "short"), datastar), "at least 8 characters")

	e.auth.EXPECT().CompleteSetPassword(gomock.Any(), "t", "password123").Return(newSes, nil).Times(2)
	e.auth.EXPECT().SetSessionCookie(gomock.Any(), newSes).Times(2)
	assertRedirectScript(t, e.do("POST", config.AppRoutes.SetPassword, pw("t", "password123", "password123"), datastar), config.AppRoutes.Users)
	assertSeeOther(t, e.do("POST", config.AppRoutes.SetPassword, pw("t", "password123", "password123")), config.AppRoutes.Users)
}

func TestTwoFactor(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	page := e.do("GET", config.AppRoutes.TwoFactor, nil).Body.String()
	for _, w := range []string{`autocomplete="one-time-code"`, `inputmode="numeric"`, `name="method" value="backup_code"`, `name="trustDevice"`, "data-indicator:_loading"} {
		if !strings.Contains(page, w) {
			t.Errorf("2FA page lacks %q", w)
		}
	}

	e.auth.EXPECT().VerifyTwoFactor(gomock.Any(), ui.VerifyTwoFactorInput{Code: "123456", Method: ui.MethodTOTP, TrustDevice: true}).Return(newSes, "trust-1", nil)
	e.auth.EXPECT().SetSessionCookie(gomock.Any(), newSes)
	e.auth.EXPECT().SetTrustDeviceCookie(gomock.Any(), "trust-1")
	assertRedirectScript(t, e.do("POST", config.AppRoutes.TwoFactor, url.Values{"code": {" 123456 "}, "trustDevice": {"true"}}, datastar), config.AppRoutes.Users)

	// No trust token: no trust cookie (the mock fails on an unexpected call).
	e.auth.EXPECT().VerifyTwoFactor(gomock.Any(), ui.VerifyTwoFactorInput{Code: "abcd-efgh", Method: ui.MethodBackupCode}).Return(newSes, "", nil)
	e.auth.EXPECT().SetSessionCookie(gomock.Any(), newSes)
	assertSeeOther(t, e.do("POST", config.AppRoutes.TwoFactor, url.Values{"code": {"abcd-efgh"}, "method": {"backup_code"}}), config.AppRoutes.Users)

	e.auth.EXPECT().VerifyTwoFactor(gomock.Any(), gomock.Any()).Return(ui.Session{}, "", ui.ErrInvalidCode)
	assertSSE(t, e.do("POST", config.AppRoutes.TwoFactor, url.Values{"code": {"1"}}, datastar, lang("ja")), `id="form-error"`, "コードが正しくありません")
	e.auth.EXPECT().VerifyTwoFactor(gomock.Any(), gomock.Any()).Return(ui.Session{}, "", ui.ErrChallengeExpired)
	assertSSE(t, e.do("POST", config.AppRoutes.TwoFactor, url.Values{"code": {"1"}}, datastar), "sign-in attempt expired", `href="/login"`)
	e.auth.EXPECT().VerifyTwoFactor(gomock.Any(), gomock.Any()).Return(ui.Session{}, "", ui.ErrInvalidCode)
	assertPage(t, e.do("POST", config.AppRoutes.TwoFactor, url.Values{"code": {"1"}, "method": {"backup_code"}}), http.StatusUnprocessableEntity, "That code is not valid", `value="backup_code" checked`)
}

// qrSVG and qrFinder pin the inline QR code: the SVG element, and the top
// row of the top-left finder pattern (7 dark modules after the 4-module quiet
// zone) at the start of its one path.
const (
	qrSVG    = `data-testid="totp-qr"`
	qrFinder = `<path d="M4 4h7v1h-7z`
)

var totp = ui.TOTPSetup{Secret: "JBSWY3DPEHPK3PXP", URI: "otpauth://totp/Acme:root?secret=JBSWY3DPEHPK3PXP", BackupCodes: []string{"aaaa-1111", "bbbb-2222"}}

func TestTwoFactorSetupSignedOut(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.auth.EXPECT().StartTwoFactorSetup(gomock.Any()).Return(totp, nil)
	assertPage(t, e.do("GET", config.AppRoutes.TwoFactorSetup, nil), http.StatusOK,
		"JBSW Y3DP EHPK 3PXP", `href="otpauth://totp/Acme:root?secret=JBSWY3DPEHPK3PXP"`, "aaaa-1111", "bbbb-2222", "shown only once", `name="code"`,
		qrSVG, `aria-label="QR code for your authenticator app"`, qrFinder)

	e.auth.EXPECT().StartTwoFactorSetup(gomock.Any()).Return(ui.TOTPSetup{}, ui.ErrChallengeExpired)
	assertPage(t, e.do("GET", config.AppRoutes.TwoFactorSetup, nil, lang("ja")), http.StatusOK, "サインインの有効期限が切れました", `href="/login"`)
	e.auth.EXPECT().StartTwoFactorSetup(gomock.Any()).Return(ui.TOTPSetup{}, errBoom)
	assertPage(t, e.do("GET", config.AppRoutes.TwoFactorSetup, nil), http.StatusInternalServerError, "Something went wrong", `href="/login"`)

	// The code finishes enrolment and signs in.
	code := url.Values{"code": {"654321"}, "secret": {totp.Secret}, "uri": {totp.URI}}
	e.auth.EXPECT().VerifyTwoFactor(gomock.Any(), ui.VerifyTwoFactorInput{Code: "654321", Method: ui.MethodTOTP}).Return(newSes, "", nil).Times(2)
	e.auth.EXPECT().SetSessionCookie(gomock.Any(), newSes).Times(2)
	assertRedirectScript(t, e.do("POST", config.AppRoutes.TwoFactorSetup, code, datastar), config.AppRoutes.Users)
	assertSeeOther(t, e.do("POST", config.AppRoutes.TwoFactorSetup, code), config.AppRoutes.Users)

	e.auth.EXPECT().VerifyTwoFactor(gomock.Any(), gomock.Any()).Return(ui.Session{}, "", ui.ErrInvalidCode)
	assertSSE(t, e.do("POST", config.AppRoutes.TwoFactorSetup, code, datastar), `id="form-error"`, "That code is not valid")
	// Without JS the page comes back with the secret (from the form), not the backup codes.
	e.auth.EXPECT().VerifyTwoFactor(gomock.Any(), gomock.Any()).Return(ui.Session{}, "", ui.ErrInvalidCode)
	rec := e.do("POST", config.AppRoutes.TwoFactorSetup, code)
	assertPage(t, rec, http.StatusUnprocessableEntity, "That code is not valid", "JBSW Y3DP EHPK 3PXP", qrSVG, qrFinder)
	if strings.Contains(rec.Body.String(), "aaaa-1111") {
		t.Error("backup codes shown twice")
	}
	e.auth.EXPECT().VerifyTwoFactor(gomock.Any(), gomock.Any()).Return(ui.Session{}, "", ui.ErrChallengeExpired)
	assertSSE(t, e.do("POST", config.AppRoutes.TwoFactorSetup, code, datastar), `id="two-factor-setup"`, "sign-in attempt expired", `href="/login"`)

	// A secret sent back that is not an otpauth URI is never linked.
	e.auth.EXPECT().VerifyTwoFactor(gomock.Any(), gomock.Any()).Return(ui.Session{}, "", ui.ErrInvalidCode)
	rec = e.do("POST", config.AppRoutes.TwoFactorSetup, url.Values{"code": {"1"}, "secret": {"S"}, "uri": {"javascript:alert(1)"}})
	if strings.Contains(rec.Body.String(), `href="javascript`) {
		t.Error("untrusted uri linked")
	}
	if strings.Contains(rec.Body.String(), qrSVG) {
		t.Error("untrusted uri encoded as a QR code")
	}
	assertPage(t, rec, http.StatusUnprocessableEntity, `data-testid="totp-secret"`)
}

func TestTwoFactorSetupSignedIn(t *testing.T) {
	t.Parallel()
	e := newEnv(t, func(d *ui.Deps) { d.RequireTwoFactor = true })
	noTwoFA := ui.Admin{ID: "a1", Email: "root@example.com"}
	noTwoFAActor := as(noTwoFA)

	assertPage(t, e.do("GET", config.AppRoutes.TwoFactorSetup, nil, noTwoFAActor), http.StatusOK, "requires two-factor authentication", `action="/two-factor/setup/enable"`)
	assertPage(t, e.do("GET", config.AppRoutes.TwoFactorSetup, nil, signedIn, lang("ja")), http.StatusOK, "このアカウントでは二要素認証が有効です。")

	e.auth.EXPECT().EnableTwoFactor(gomock.Any(), "a1", "wrong").Return(ui.TOTPSetup{}, ui.ErrInvalidPassword)
	assertSSE(t, e.do("POST", routes.EnableTwoFactor, url.Values{"password": {"wrong"}}, datastar, noTwoFAActor), `id="form-error"`, "Incorrect password")
	e.auth.EXPECT().EnableTwoFactor(gomock.Any(), "a1", "pw").Return(totp, nil)
	assertSSE(t, e.do("POST", routes.EnableTwoFactor, url.Values{"password": {"pw"}}, datastar, noTwoFAActor), `id="two-factor-setup"`, "JBSW Y3DP EHPK 3PXP", "aaaa-1111", qrSVG, qrFinder)
	e.auth.EXPECT().EnableTwoFactor(gomock.Any(), "a1", "pw").Return(totp, nil)
	assertPage(t, e.do("POST", routes.EnableTwoFactor, url.Values{"password": {"pw"}}, noTwoFAActor, lang("ja")), http.StatusOK, "aaaa-1111", qrSVG, qrFinder, `aria-label="認証アプリ用のQRコード"`)
	assertRedirectScript(t, e.do("POST", routes.EnableTwoFactor, url.Values{"password": {"pw"}}, datastar), config.AppRoutes.Login)

	code := url.Values{"code": {"111222"}, "secret": {totp.Secret}, "uri": {totp.URI}}
	e.auth.EXPECT().ConfirmTwoFactor(gomock.Any(), "a1", "111222").Return(ui.ErrInvalidCode)
	assertSSE(t, e.do("POST", config.AppRoutes.TwoFactorSetup, code, datastar, noTwoFAActor), `id="form-error"`, "That code is not valid")
	e.auth.EXPECT().ConfirmTwoFactor(gomock.Any(), "a1", "111222").Return(nil).Times(2)
	assertSSE(t, e.do("POST", config.AppRoutes.TwoFactorSetup, code, datastar, noTwoFAActor), `id="two-factor-setup"`, "Two-factor authentication is now on.", `href="/users"`)
	assertPage(t, e.do("POST", config.AppRoutes.TwoFactorSetup, code, noTwoFAActor, lang("ja")), http.StatusOK, "二要素認証が有効になりました。")
}

func TestLogout(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.auth.EXPECT().SignOut(gomock.Any(), "tok-1").Return(nil)
	e.auth.EXPECT().ClearSessionCookie(gomock.Any())
	assertSeeOther(t, e.do("POST", routes.Logout, nil, signedIn), config.AppRoutes.Login)

	e.auth.EXPECT().SignOut(gomock.Any(), "tok-1").Return(errBoom) // still signed out locally
	e.auth.EXPECT().ClearSessionCookie(gomock.Any())
	assertRedirectScript(t, e.do("POST", routes.Logout, nil, signedIn, datastar), config.AppRoutes.Login)

	e.auth.EXPECT().ClearSessionCookie(gomock.Any())
	assertSeeOther(t, e.do("POST", routes.Logout, nil), config.AppRoutes.Login)
}

// --- user actions ---

type userAction struct {
	name   string
	path   string
	form   url.Values
	expect func(e *env) *gomock.Call // the service call of the happy path
	done   string
}

func userActions() []userAction {
	return []userAction{
		{"ban", routes.User(routes.UserBan, "u1"), url.Values{"banReason": {"  spam  "}, "banExpiresIn": {"604800"}}, func(e *env) *gomock.Call {
			return e.users.EXPECT().BanUser(gomock.Any(), actor, "u1", validate.BanUser{BanReason: "spam", BanExpiresIn: ptr(int64(604800))})
		}, "User banned and signed out everywhere."},
		{"ban permanent", routes.User(routes.UserBan, "u1"), url.Values{"banReason": {"spam"}, "banExpiresIn": {"0"}}, func(e *env) *gomock.Call {
			return e.users.EXPECT().BanUser(gomock.Any(), actor, "u1", validate.BanUser{BanReason: "spam"})
		}, "User banned"},
		{"unban", routes.User(routes.UserUnban, "u1"), url.Values{}, func(e *env) *gomock.Call {
			return e.users.EXPECT().UnbanUser(gomock.Any(), actor, "u1")
		}, "Ban lifted."},
		{"role", routes.User(routes.UserRole, "u1"), url.Values{"role": {" admin "}}, func(e *env) *gomock.Call {
			return e.users.EXPECT().SetRole(gomock.Any(), actor, "u1", validate.SetUserRole{Role: "admin"})
		}, "Role updated."},
		{"revoke all", routes.User(routes.UserSessions, "u1"), url.Values{}, func(e *env) *gomock.Call {
			return e.users.EXPECT().RevokeSessions(gomock.Any(), actor, "u1")
		}, "All sessions revoked."},
		{"revoke one", routes.Session("u1", "us1"), url.Values{}, func(e *env) *gomock.Call {
			return e.users.EXPECT().RevokeSession(gomock.Any(), actor, "u1", "us1")
		}, "Session revoked."},
	}
}

func TestUserActions(t *testing.T) {
	t.Parallel()
	for _, a := range userActions() {
		t.Run(a.name, func(t *testing.T) {
			t.Parallel()
			e := newEnv(t)
			// no session
			assertRedirectScript(t, e.do("POST", a.path, a.form, datastar), config.AppRoutes.Login)
			assertSeeOther(t, e.do("POST", a.path, a.form), config.AppRoutes.Login)

			// happy path: refreshed fragments + status (Datastar), 303 (plain)
			a.expect(e).Return(nil).Times(2)
			e.users.EXPECT().GetUser(gomock.Any(), actor, "u1").Return(bannedDetail(), nil)
			assertSSE(t, e.do("POST", a.path, a.form, datastar, signedIn),
				`id="user-profile"`, `id="user-ban"`, `id="user-sessions"`, `id="user-status"`, a.done, "spamming", "10.0.0.7")
			assertSeeOther(t, e.do("POST", a.path, a.form, signedIn), config.UserPage("u1"))

			// service error: translated alert (Datastar), full page (plain)
			a.expect(e).Return(services.ErrUserNotFound)
			assertSSE(t, e.do("POST", a.path, a.form, datastar, signedIn, lang("ja")), `id="user-status"`, "ユーザーが見つかりません")
			a.expect(e).Return(errBoom)
			e.users.EXPECT().GetUser(gomock.Any(), actor, "u1").Return(bannedDetail(), nil)
			assertPage(t, e.do("POST", a.path, a.form, signedIn), http.StatusUnprocessableEntity, "Something went wrong", `id="user-profile"`)

			// applied, outcome not recorded: the generic message, no internals
			a.expect(e).Return(errApplied)
			rec := e.do("POST", a.path, a.form, datastar, signedIn)
			assertSSE(t, rec, `id="user-status"`, "Something went wrong")
			if strings.Contains(rec.Body.String(), "WAS applied") {
				t.Error("internal error text reached the page")
			}

			// audit log down: refused, nothing changed (Datastar and plain)
			a.expect(e).Return(errAudit).Times(3)
			assertSSE(t, e.do("POST", a.path, a.form, datastar, signedIn), `id="user-status"`, `role="alert"`, auditDownEN)
			assertSSE(t, e.do("POST", a.path, a.form, datastar, signedIn, lang("ja")), `id="user-status"`, auditDownJA)
			e.users.EXPECT().GetUser(gomock.Any(), actor, "u1").Return(bannedDetail(), nil)
			assertPage(t, e.do("POST", a.path, a.form, signedIn), http.StatusUnprocessableEntity, auditDownEN, `id="user-profile"`)

			// 2FA required: off to enrolment
			a.expect(e).Return(services.ErrTwoFactorRequired)
			assertRedirectScript(t, e.do("POST", a.path, a.form, datastar, signedIn), config.AppRoutes.TwoFactorSetup)

			// reload after success failed: the status still shows
			a.expect(e).Return(nil)
			e.users.EXPECT().GetUser(gomock.Any(), actor, "u1").Return(services.UserDetail{}, errBoom)
			rec = e.do("POST", a.path, a.form, datastar, signedIn)
			assertSSE(t, rec, `id="user-status"`, a.done)
			if strings.Contains(rec.Body.String(), `id="user-profile"`) {
				t.Error("patched a profile it could not load")
			}
		})
	}
}

func TestUserActionsInvalidInput(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	cases := []struct {
		path string
		form url.Values
	}{
		{routes.User(routes.UserBan, "u1"), url.Values{"banReason": {"   "}, "banExpiresIn": {"3600"}}},
		{routes.User(routes.UserBan, "u1"), url.Values{"banReason": {"spam"}, "banExpiresIn": {"soon"}}},
		{routes.User(routes.UserBan, "u1"), url.Values{"banReason": {"spam"}, "banExpiresIn": {"5"}}},
		{routes.User(routes.UserRole, "u1"), url.Values{"role": {"Not A Role"}}},
	}
	for _, c := range cases {
		// No service call is expected: the mock fails the test on one.
		assertSSE(t, e.do("POST", c.path, c.form, datastar, signedIn, lang("ja")), `id="user-status"`, "入力内容を確認して")
	}
	// Service-side rules come back translated too.
	e.users.EXPECT().SetRole(gomock.Any(), actor, "u1", validate.SetUserRole{Role: "user"}).Return(services.ErrRoleUnchanged)
	assertSSE(t, e.do("POST", routes.User(routes.UserRole, "u1"), url.Values{"role": {"user"}}, datastar, signedIn), "already has this role")
	e.users.EXPECT().UnbanUser(gomock.Any(), actor, "u1").Return(services.ErrUserNotBanned)
	assertSSE(t, e.do("POST", routes.User(routes.UserUnban, "u1"), nil, datastar, signedIn), "The user is not banned.")
	e.users.EXPECT().RevokeSession(gomock.Any(), actor, "u1", "gone").Return(services.ErrSessionNotFound)
	assertSSE(t, e.do("POST", routes.Session("u1", "gone"), nil, datastar, signedIn), "That session no longer exists.")
	e.users.EXPECT().SetRole(gomock.Any(), actor, "u1", validate.SetUserRole{Role: "owner"}).Return(services.ErrInvalidRole)
	assertSSE(t, e.do("POST", routes.User(routes.UserRole, "u1"), url.Values{"role": {"owner"}}, datastar, signedIn), "Unknown role")
	e.users.EXPECT().UnbanUser(gomock.Any(), actor, "u1").Return(&services.Error{Kind: services.KindInvalid, Message: "x"})
	assertSSE(t, e.do("POST", routes.User(routes.UserUnban, "u1"), nil, datastar, signedIn), "Please check your input")
}

// --- admins ---

func TestInviteAdmin(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	form := url.Values{"email": {" New@Example.com "}, "name": {"Nia"}}
	assertRedirectScript(t, e.do("POST", routes.AdminInvite, form, datastar), config.AppRoutes.Login)
	assertSeeOther(t, e.do("POST", routes.AdminInvite, form), config.AppRoutes.Login)

	// invalid input never reaches the service
	assertSSE(t, e.do("POST", routes.AdminInvite, url.Values{"email": {"not-an-email"}}, datastar, signedIn, lang("ja")), `id="invite-card"`, "入力内容を確認して", `value="not-an-email"`)

	in := validate.CreateAdminInvite{Email: "new@example.com", Name: "Nia"}
	e.admins.EXPECT().InviteAdmin(gomock.Any(), actor, in).Return(services.Admin{ID: "a2", Email: "new@example.com"}, nil).Times(2)
	e.admins.EXPECT().ListAdmins(gomock.Any(), actor).Return([]services.Admin{{ID: "a2", Email: "new@example.com", Name: "Nia"}}, nil)
	assertSSE(t, e.do("POST", routes.AdminInvite, form, datastar, signedIn, lang("ja")), `id="admins-table"`, `id="invite-card"`, "new@example.com に招待を送信しました", "招待中")
	assertSeeOther(t, e.do("POST", routes.AdminInvite, form, signedIn), config.AppRoutes.Admins)

	e.admins.EXPECT().InviteAdmin(gomock.Any(), actor, in).Return(services.Admin{}, services.ErrAdminExists)
	assertSSE(t, e.do("POST", routes.AdminInvite, form, datastar, signedIn), `id="invite-error"`, "already an admin")
	e.admins.EXPECT().InviteAdmin(gomock.Any(), actor, in).Return(services.Admin{}, errBoom)
	e.admins.EXPECT().ListAdmins(gomock.Any(), actor).Return(nil, errBoom)
	assertPage(t, e.do("POST", routes.AdminInvite, form, signedIn), http.StatusUnprocessableEntity, "Something went wrong", `id="admins-table"`)
	// audit log down: refused with the form kept (Datastar and plain)
	e.admins.EXPECT().InviteAdmin(gomock.Any(), actor, in).Return(services.Admin{}, errAudit).Times(3)
	assertSSE(t, e.do("POST", routes.AdminInvite, form, datastar, signedIn), `id="invite-error"`, auditDownEN, `value="Nia"`)
	assertSSE(t, e.do("POST", routes.AdminInvite, form, datastar, signedIn, lang("ja")), `id="invite-error"`, auditDownJA)
	e.admins.EXPECT().ListAdmins(gomock.Any(), actor).Return([]services.Admin{{ID: "a1", Email: "root@example.com", HasPassword: true}}, nil)
	assertPage(t, e.do("POST", routes.AdminInvite, form, signedIn, lang("ja")), http.StatusUnprocessableEntity, auditDownJA, `id="admins-table"`)
	e.admins.EXPECT().InviteAdmin(gomock.Any(), actor, in).Return(services.Admin{}, services.ErrTwoFactorRequired)
	assertSeeOther(t, e.do("POST", routes.AdminInvite, form, signedIn), config.AppRoutes.TwoFactorSetup)

	// reload after success failed: the confirmation still shows
	e.admins.EXPECT().InviteAdmin(gomock.Any(), actor, in).Return(services.Admin{ID: "a2", Email: "new@example.com"}, nil)
	e.admins.EXPECT().ListAdmins(gomock.Any(), actor).Return(nil, errBoom)
	rec := e.do("POST", routes.AdminInvite, form, datastar, signedIn)
	assertSSE(t, rec, "Invite sent to new@example.com")
	if strings.Contains(rec.Body.String(), `id="admins-table"`) {
		t.Error("patched a table it could not load")
	}
}

func TestAdminsPageRowActions(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.admins.EXPECT().ListAdmins(gomock.Any(), actor).Return([]services.Admin{
		{ID: "a1", Email: "root@example.com", HasPassword: true, TwoFactorEnabled: true},
		{ID: "a2", Email: "active@example.com", HasPassword: true},
		{ID: "a3", Email: "pending@example.com"},
		{ID: "a4", Email: "off@example.com", HasPassword: true, Disabled: true},
	}, nil).Times(2)
	rec := e.do("GET", config.AppRoutes.Admins, nil, signedIn)
	assertPage(t, rec, http.StatusOK, `id="admins-status"`, "Invite pending", "Disabled", ">You<",
		`action="/admins/a2/disable"`, `action="/admins/a2/remove"`,
		`action="/admins/a3/disable"`, `action="/admins/a3/remove"`,
		`action="/admins/a4/enable"`, `action="/admins/a4/remove"`,
		// Remove asks first in a dialog with JS; the plain form posts as is.
		`<dialog id="remove-a2-dialog"`, "Remove active@example.com? They lose access to the dashboard right away. This cannot be undone.",
		`data-on:submit="@post(&#39;/admins/a2/remove&#39;, {contentType: &#39;form&#39;})"`,
		"data-indicator:_disabling", "data-indicator:_enabling", "data-indicator:_removing")
	body := rec.Body.String()
	for _, absent := range []string{"/admins/a1/", `action="/admins/a2/enable"`, `action="/admins/a3/enable"`, `action="/admins/a4/disable"`} {
		if strings.Contains(body, absent) {
			t.Errorf("admins page has %q", absent)
		}
	}
	assertPage(t, e.do("GET", config.AppRoutes.Admins, nil, signedIn, lang("ja")), http.StatusOK,
		"無効化済み", "招待中", ">あなた<", ">無効化<", ">有効化<", ">削除<", "active@example.com を削除しますか？")
}

type adminAction struct {
	name   string
	path   func(id string) string
	expect func(e *env, id string) *gomock.Call // the service call
	done   string
}

func adminActions() []adminAction {
	return []adminAction{
		{"disable", func(id string) string { return routes.Admin(routes.AdminDisable, id) }, func(e *env, id string) *gomock.Call {
			return e.admins.EXPECT().DisableAdmin(gomock.Any(), actor, id)
		}, "Admin disabled and signed out everywhere."},
		{"enable", func(id string) string { return routes.Admin(routes.AdminEnable, id) }, func(e *env, id string) *gomock.Call {
			return e.admins.EXPECT().EnableAdmin(gomock.Any(), actor, id)
		}, "Admin enabled. They can sign in again."},
		{"remove", func(id string) string { return routes.Admin(routes.AdminRemove, id) }, func(e *env, id string) *gomock.Call {
			return e.admins.EXPECT().RemoveAdmin(gomock.Any(), actor, id)
		}, "Admin removed."},
	}
}

func TestAdminActions(t *testing.T) {
	t.Parallel()
	refreshed := []services.Admin{
		{ID: "a1", Email: "root@example.com", HasPassword: true},
		{ID: "a2", Email: "other@example.com", HasPassword: true, Disabled: true},
	}
	for _, a := range adminActions() {
		t.Run(a.name, func(t *testing.T) {
			t.Parallel()
			e := newEnv(t)
			path := a.path("a2")
			// no session
			assertRedirectScript(t, e.do("POST", path, url.Values{}, datastar), config.AppRoutes.Login)
			assertSeeOther(t, e.do("POST", path, url.Values{}), config.AppRoutes.Login)

			// happy path: refreshed table + status (Datastar), 303 (plain)
			a.expect(e, "a2").Return(nil).Times(2)
			e.admins.EXPECT().ListAdmins(gomock.Any(), actor).Return(refreshed, nil)
			rec := e.do("POST", path, url.Values{}, datastar, signedIn)
			assertSSE(t, rec, `id="admins-table"`, `id="admins-status"`, `role="status"`, a.done, "other@example.com", `action="/admins/a2/enable"`)
			if strings.Contains(rec.Body.String(), "/admins/a1/") {
				t.Error("refreshed table offers actions on the signed-in admin")
			}
			assertSeeOther(t, e.do("POST", path, url.Values{}, signedIn), config.AppRoutes.Admins)

			// every service error is translated
			for _, c := range []struct {
				err    error
				en, ja string
			}{
				{services.ErrAdminNotFound, "Admin not found.", "管理者が見つかりません。"},
				{services.ErrAdminSelf, "You cannot disable or remove yourself.", "自分自身を無効化または削除することはできません。"},
				{services.ErrLastAdmin, "The last active admin cannot be disabled or removed.", "最後の有効な管理者は無効化または削除できません。"},
				{services.ErrAdminDisabled, "This admin is already disabled.", "この管理者はすでに無効化されています。"},
				{services.ErrAdminNotDisabled, "This admin is not disabled.", "この管理者は無効化されていません。"},
				{services.ErrForbidden, "You are not allowed to do that.", "この操作は許可されていません。"},
				{errAudit, auditDownEN, auditDownJA},
			} {
				a.expect(e, "a2").Return(c.err).Times(2)
				assertSSE(t, e.do("POST", path, url.Values{}, datastar, signedIn), `id="admins-status"`, `role="alert"`, c.en)
				assertSSE(t, e.do("POST", path, url.Values{}, datastar, signedIn, lang("ja")), c.ja)
			}

			// the signed-in admin's own row: the service refuses
			a.expect(e, "a1").Return(services.ErrAdminSelf)
			assertSSE(t, e.do("POST", a.path("a1"), url.Values{}, datastar, signedIn), "You cannot disable or remove yourself.")

			// plain post error: the full page with the alert and the table
			a.expect(e, "a2").Return(errBoom)
			e.admins.EXPECT().ListAdmins(gomock.Any(), actor).Return(refreshed, nil)
			assertPage(t, e.do("POST", path, url.Values{}, signedIn), http.StatusUnprocessableEntity, "Something went wrong", `id="admins-table"`, "other@example.com")
			a.expect(e, "a2").Return(services.ErrLastAdmin)
			e.admins.EXPECT().ListAdmins(gomock.Any(), actor).Return(nil, errBoom)
			assertPage(t, e.do("POST", path, url.Values{}, signedIn, lang("ja")), http.StatusUnprocessableEntity, "最後の有効な管理者")
			a.expect(e, "a2").Return(errAudit)
			e.admins.EXPECT().ListAdmins(gomock.Any(), actor).Return(refreshed, nil)
			assertPage(t, e.do("POST", path, url.Values{}, signedIn), http.StatusUnprocessableEntity, auditDownEN, `id="admins-table"`)

			// 2FA required: off to enrolment
			a.expect(e, "a2").Return(services.ErrTwoFactorRequired).Times(2)
			assertRedirectScript(t, e.do("POST", path, url.Values{}, datastar, signedIn), config.AppRoutes.TwoFactorSetup)
			assertSeeOther(t, e.do("POST", path, url.Values{}, signedIn), config.AppRoutes.TwoFactorSetup)

			// reload after success failed: the status still shows
			a.expect(e, "a2").Return(nil)
			e.admins.EXPECT().ListAdmins(gomock.Any(), actor).Return(nil, errBoom)
			rec = e.do("POST", path, url.Values{}, datastar, signedIn)
			assertSSE(t, rec, `id="admins-status"`, a.done)
			if strings.Contains(rec.Body.String(), `id="admins-table"`) {
				t.Error("patched a table it could not load")
			}
		})
	}
}

func TestAdminActionsRejectCrossOrigin(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	for _, a := range adminActions() {
		rec := e.do("POST", a.path("a2"), url.Values{}, signedIn, func(r *http.Request) { r.Header.Set("Sec-Fetch-Site", "cross-site") })
		if rec.Code != http.StatusForbidden {
			t.Errorf("%s: cross-site post %d", a.name, rec.Code)
		}
	}
}

func TestAuditAdminActions(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	entry := func(action string, d map[string]string) services.AuditEntry {
		return services.AuditEntry{Action: action, ActorEmail: "root@example.com", TargetType: services.TargetAdmin, TargetID: "a2", TargetLabel: "other@example.com", Details: d, CreatedAt: created}
	}
	page := services.AuditPage{Entries: []services.AuditEntry{
		entry(services.ActionAdminDisable, nil),
		entry(services.ActionAdminEnable, nil),
		entry(services.ActionAdminRemove, nil),
		entry(services.ActionAdminSignIn, map[string]string{"method": services.SignInPassword}),
		entry(services.ActionAdminSignIn, map[string]string{"method": services.SignInTwoFactor}),
		entry(services.ActionAdminSignIn, map[string]string{"method": services.SignInSetPassword}),
		entry(services.ActionAdminSignIn, map[string]string{"method": "passkey"}),
		entry(services.ActionAdminSignOut, nil),
		entry(services.ActionAdminTwoFactorFailed, map[string]string{"reason": "INVALID_CODE"}),
		entry(services.ActionAdminTwoFactorFailed, map[string]string{"reason": "SOMETHING_NEW"}),
		entry("admin.something_new", map[string]string{"k": "v"}),
	}, Total: 11, Limit: 25}
	e.audit.EXPECT().List(gomock.Any(), actor, gomock.Any()).Return(page, nil).Times(2)
	assertPage(t, e.do("GET", config.AppRoutes.Audit, nil, signedIn), http.StatusOK,
		">Disabled admin<", ">Enabled admin<", ">Removed admin<", ">Signed in<", ">Signed out<", ">Failed two-factor code<",
		"Method: password<", "Method: password + two-factor<", "Method: set-password link<", "Method: passkey<",
		"Reason: wrong code<", "Reason: SOMETHING_NEW<", ">admin.something_new<", "k: v<",
		`<option value="admin.disable">Disabled admin</option>`, `<option value="admin.sign_in">Signed in</option>`,
		`<option value="admin.two_factor_failed">Failed two-factor code</option>`)
	assertPage(t, e.do("GET", config.AppRoutes.Audit, nil, signedIn, lang("ja")), http.StatusOK,
		">管理者を無効化<", ">管理者を有効化<", ">管理者を削除<", ">サインイン<", ">サインアウト<", ">二要素認証コードの誤り<",
		"方法: パスワード<", "方法: パスワード + 二要素認証<", "方法: パスワード設定リンク<", "理由: コードの誤り<",
		`<option value="admin.sign_out">サインアウト</option>`)

	// The filter passes the new actions through.
	e.audit.EXPECT().List(gomock.Any(), actor, validate.ListAuditLog{Action: services.ActionAdminSignIn, Limit: 25}).Return(services.AuditPage{Limit: 25}, nil)
	assertPage(t, e.do("GET", config.AppRoutes.Audit+"?action=admin.sign_in", nil, signedIn), http.StatusOK, `<option value="admin.sign_in" selected>`)
}

// TestAuditStatuses: every entry shows its outcome. Pending is "outcome
// unknown" with what that means; known failure reasons and the reconciler's
// reasons are translated, anything else shows as recorded.
func TestAuditStatuses(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	entry := func(status, reason string, reconciled bool) services.AuditEntry {
		return services.AuditEntry{Action: services.ActionUserBan, ActorEmail: "root@example.com", TargetType: services.TargetUser, TargetID: "u1", TargetLabel: "ada@example.com", CreatedAt: created, Status: status, Reason: reason, Reconciled: reconciled}
	}
	page := services.AuditPage{Entries: []services.AuditEntry{
		entry(services.StatusSucceeded, "", false),
		entry(services.StatusFailed, services.ErrUserNotBanned.Message, false),
		entry(services.StatusFailed, "Internal error", false),
		entry(services.StatusPending, "", false),
		entry(services.StatusUnknown, "", false),
		entry(services.StatusSucceeded, "Reconciled: the current state shows the change", true),
		entry(services.StatusFailed, "Reconciled: the current state does not show the change", true),
		entry(services.StatusUnknown, "Reconciled: the outcome cannot be told from the current state", true),
		entry("archived", "", false),
	}, Total: 9, Limit: 25}
	e.audit.EXPECT().List(gomock.Any(), actor, gomock.Any()).Return(page, nil).Times(2)

	rec := e.do("GET", config.AppRoutes.Audit, nil, signedIn)
	assertPage(t, rec, http.StatusOK, ">Status<",
		// status badges (a dot, then the label)
		" Done</span>", " Failed</span>", " Outcome unknown</span>", " Unknown</span>", " archived</span>",
		">The user is not banned.<", ">Internal error (details are in the server log)<",
		"The change may or may not have been applied.",
		">reconciled<", `title="Resolved later from the current state`,
		">The current state shows the change.<", ">The current state does not show the change.<",
		">The outcome cannot be told from the current state.<")
	if strings.Contains(rec.Body.String(), "Reconciled:") {
		t.Error("a reconciler reason showed untranslated")
	}
	if n := strings.Count(rec.Body.String(), ">reconciled<"); n != 3 {
		t.Errorf("%d reconciled markers, want 3", n)
	}

	rec = e.do("GET", config.AppRoutes.Audit, nil, signedIn, lang("ja"))
	assertPage(t, rec, http.StatusOK, ">結果<",
		" 完了</span>", " 失敗</span>", " 結果不明</span>", " 判定不能</span>",
		">このユーザーは利用停止されていません。<",
		"変更が適用されたかどうかは不明です。", ">照合済み<",
		">現在の状態に変更が反映されています。<", ">現在の状態に変更は反映されていません。<",
		">現在の状態からは結果を判断できません。<")
	for _, en := range []string{"Reconciled:", " Done</span>", "Outcome unknown", "The user is not banned"} {
		if strings.Contains(rec.Body.String(), en) {
			t.Errorf("ja page shows %q", en)
		}
	}
}

// --- shell ---

func TestLocaleSwitch(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	rec := e.do("POST", routes.Locale, url.Values{"locale": {"ja"}, "next": {"/users?x=1"}})
	assertSeeOther(t, rec, "/users?x=1")
	var lc *http.Cookie
	for _, c := range rec.Result().Cookies() {
		if c.Name == "locale" {
			lc = c
		}
	}
	if lc == nil || lc.Value != "ja" {
		t.Fatalf("locale cookie: %+v", lc)
	}
	page := e.do("GET", config.AppRoutes.Login, nil, func(r *http.Request) { r.AddCookie(lc) }, lang("en"))
	if !strings.Contains(page.Body.String(), "ダッシュボードにサインイン") {
		t.Error("cookie locale not applied over Accept-Language")
	}
	for _, next := range []string{"//evil.example", "https://evil.example", `/\evil.example`, "/\t/evil.example", "/\n/evil.example"} {
		rec := e.do("POST", routes.Locale, url.Values{"locale": {"xx"}, "next": {next}})
		assertSeeOther(t, rec, config.AppRoutes.Home)
	}
}

func TestActionsRejectCrossOrigin(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	rec := e.do("POST", config.AppRoutes.Login, url.Values{"email": {"root@example.com"}, "password": {"pw"}}, func(r *http.Request) {
		r.Header.Set("Sec-Fetch-Site", "cross-site")
	})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("cross-site post: %d", rec.Code)
	}
}

func TestNewRequiresDeps(t *testing.T) {
	t.Parallel()
	defer func() {
		if recover() == nil {
			t.Fatal("New without UserFrom did not panic")
		}
	}()
	e := newEnv(t)
	ui.New(ui.Deps{Auth: e.auth, Users: e.users, Admins: e.admins, Audit: e.audit, Analytics: e.analytics})
}

func TestDefaultAppName(t *testing.T) {
	t.Parallel()
	e := newEnv(t, func(d *ui.Deps) { d.AppName = ""; d.Logger = nil })
	assertPage(t, e.do("GET", config.AppRoutes.Login, nil), http.StatusOK, "— Admin</title>")
}
