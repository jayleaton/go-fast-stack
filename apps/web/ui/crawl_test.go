package ui_test

// The exposure crawl: every route the UI registers is requested in every
// state (anonymous and signed in, Datastar and plain form posts, services
// succeeding and failing with internal errors) and every response is checked
// for what must never reach a browser: the JSON API, secrets, internal error
// text, inline script or style the CSP would have to allow, and missing
// security headers. A new route fails TestCrawlCoversEveryRoute until it is
// added to crawlTargets.

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"go.uber.org/mock/gomock"

	"github.com/jayleaton/ai-patterns-go/apps/web/internal/config"
	"github.com/jayleaton/ai-patterns-go/apps/web/internal/services"
	"github.com/jayleaton/ai-patterns-go/apps/web/ui"
	"github.com/jayleaton/ai-patterns-go/apps/web/ui/mocks"
	"github.com/jayleaton/ai-patterns-go/apps/web/ui/routes"
	"github.com/jayleaton/ai-patterns-go/apps/web/ui/static"
)

// Canaries: values the services hand the UI that must never be rendered.
const (
	canaryToken    = "tok_CANARY_session_7f3a9c"    // the caller's session token
	canaryInternal = "pq: relation CANARY_internal" // an unexpected service error
	canaryPath     = "/srv/app/CANARY_internal.go:42"
)

// leaks are patterns no UI response body may contain.
var leaks = []*regexp.Regexp{
	regexp.MustCompile(`/api/`),            // the JSON API is never referenced
	regexp.MustCompile(`CANARY`),           // secrets and internal error text
	regexp.MustCompile(`\.go:\d+`),         // file paths of stack traces
	regexp.MustCompile(`goroutine \d+ \[`), // stack traces
	regexp.MustCompile(`\*?(services|auth|validate|pgx|pgconn|errors)\.[A-Z]\w+`), // Go type names
	regexp.MustCompile(`"data":|"error":`),                                        // JSON envelopes
	regexp.MustCompile(`(?i)\son[a-z]+=`),                                         // inline event handlers
	regexp.MustCompile(`\sstyle=`),                                                // inline styles (the CSP has no 'unsafe-inline')
}

var (
	scriptTag = regexp.MustCompile(`<script\b[^>]*>`)
	nonceAttr = regexp.MustCompile(`\snonce="([^"]+)"`)
	cspNonce  = regexp.MustCompile(`'nonce-([^']+)'`)
)

// patternRecorder records the patterns ui.Routes registers.
type patternRecorder struct{ patterns []string }

func (p *patternRecorder) Handle(pattern string, _ http.Handler) {
	p.patterns = append(p.patterns, pattern)
}

type crawlTarget struct {
	pattern string   // as registered
	paths   []string // concrete paths to request (query included)
}

var crawlTargets = []crawlTarget{
	{"GET " + routes.Static, []string{static.URL(static.CSSFile), static.URL(static.DatastarFile), static.URL(static.FaviconFile), routes.Static, routes.Static + "nope.js"}},
	{"GET /favicon.ico", []string{"/favicon.ico"}},
	{"GET /{$}", []string{"/"}},
	{"GET " + config.AppRoutes.Why, []string{config.AppRoutes.Why}},
	{"GET " + config.AppRoutes.Login, []string{config.AppRoutes.Login}},
	{"POST " + config.AppRoutes.Login, []string{config.AppRoutes.Login}},
	{"GET " + config.AppRoutes.Signup, []string{config.AppRoutes.Signup}},
	{"POST " + config.AppRoutes.Signup, []string{config.AppRoutes.Signup}},
	{"GET " + config.AppRoutes.ForgotPassword, []string{config.AppRoutes.ForgotPassword}},
	{"POST " + config.AppRoutes.ForgotPassword, []string{config.AppRoutes.ForgotPassword}},
	{"GET " + config.AppRoutes.ResetPassword, []string{config.AppRoutes.ResetPassword, config.AppRoutes.ResetPassword + "?token=reset-tok"}},
	{"POST " + config.AppRoutes.ResetPassword, []string{config.AppRoutes.ResetPassword}},
	{"GET " + config.AppRoutes.VerifyEmail, []string{config.AppRoutes.VerifyEmail + "?email=demo%40example.com"}},
	{"POST " + config.AppRoutes.VerifyEmail, []string{config.AppRoutes.VerifyEmail}},
	{"GET " + config.AppRoutes.VerifyEmailSuccess, []string{
		config.AppRoutes.VerifyEmailSuccess, config.AppRoutes.VerifyEmailSuccess + "?token=verify-tok&callbackURL=%2Fdashboard",
		config.AppRoutes.VerifyEmailSuccess + "?error=INVALID_TOKEN",
	}},
	{"POST " + config.AppRoutes.VerifyEmailSuccess, []string{config.AppRoutes.VerifyEmailSuccess + "?token=verify-tok&callbackURL=%2Fdashboard"}},
	{"GET " + config.AppRoutes.Dashboard, []string{config.AppRoutes.Dashboard}},
	{"GET " + routes.DashboardLive, []string{routes.DashboardLive + "?tab=crawl"}},
	{"GET " + config.AppRoutes.Account, []string{config.AppRoutes.Account, config.AppRoutes.EmailChanged}},
	{"POST " + routes.AccountProfile, []string{routes.AccountProfile}},
	{"POST " + routes.AccountEmail, []string{routes.AccountEmail}},
	{"POST " + routes.AccountPassword, []string{routes.AccountPassword}},
	{"POST " + routes.AccountRevokeSession, []string{routes.AccountRevokeSession}},
	{"POST " + routes.AccountRevokeOthers, []string{routes.AccountRevokeOthers}},
	{"POST " + routes.AccountDelete, []string{routes.AccountDelete}},
	{"POST " + routes.Logout, []string{routes.Logout}},
	{"POST " + routes.Locale, []string{routes.Locale}},
}

// crawlForm carries every field any UI form posts.
var crawlForm = url.Values{
	"email": {"demo@example.com"}, "password": {"password123"}, "confirmPassword": {"password123"},
	"name": {"Demo User"}, "image": {""}, "newEmail": {"new@example.com"},
	"currentPassword": {"password123"}, "newPassword": {"password456"}, "revokeOtherSessions": {"on"},
	"sessionId": {"sess-2"}, "token": {"reset-tok"}, "locale": {"ja"}, "next": {"/dashboard"},
}

func TestCrawlCoversEveryRoute(t *testing.T) {
	rec := &patternRecorder{}
	ui.New(ui.Deps{Auth: mocks.NewMockAuth(gomock.NewController(t)), Users: mocks.NewMockUsers(gomock.NewController(t))}).Routes(rec)
	var crawled []string
	for _, c := range crawlTargets {
		crawled = append(crawled, c.pattern)
	}
	for _, p := range rec.patterns {
		if !slices.Contains(crawled, p) {
			t.Errorf("route %q is not crawled: add it to crawlTargets", p)
		}
	}
	for _, p := range crawled {
		if !slices.Contains(rec.patterns, p) {
			t.Errorf("crawlTargets has %q, which the UI no longer registers", p)
		}
	}
}

func TestCrawlExposesNothing(t *testing.T) {
	for _, failing := range []bool{false, true} {
		for _, signedIn := range []bool{false, true} {
			h := crawlUI(t, failing, signedIn)
			for _, c := range crawlTargets {
				method, _, _ := strings.Cut(c.pattern, " ")
				for _, path := range c.paths {
					for _, ds := range []bool{false, true} {
						name := strings.Join([]string{method, path, state(failing, "failing", "ok"), state(signedIn, "signed-in", "anonymous"), state(ds, "datastar", "plain")}, " ")
						checkResponse(t, name, crawl(h, method, path, ds))
					}
				}
			}
		}
	}
}

func state(b bool, yes, no string) string {
	if b {
		return yes
	}
	return no
}

// crawlUI is the real UI handler over mocks that answer anything: test data,
// or (failing) internal errors carrying canary text.
func crawlUI(t *testing.T, failing, signedIn bool) http.Handler {
	ctrl := gomock.NewController(t)
	a, u := mocks.NewMockAuth(ctrl), mocks.NewMockUsers(ctrl)
	var fail error
	if failing {
		fail = errors.New(canaryInternal + " at " + canaryPath)
	}
	sess := ui.Session{ID: "sess-1", Token: canaryToken, UserID: "u1", ExpiresAt: time.Now().Add(time.Hour)}
	user := ui.User{ID: "u1", Name: "Demo User", Email: "demo@example.com", EmailVerified: true, CreatedAt: time.Now()}
	ip, agent := "192.0.2.1", "Mozilla/5.0 (X11; Linux x86_64)"
	sessions := []services.Session{
		{ID: "sess-1", CreatedAt: time.Now(), ExpiresAt: time.Now().Add(time.Hour), IPAddress: &ip, UserAgent: &agent, Current: true},
		{ID: "sess-2", CreatedAt: time.Now(), ExpiresAt: time.Now().Add(time.Hour), IPAddress: &ip, UserAgent: &agent},
	}

	a.EXPECT().SignUpEmail(gomock.Any(), gomock.Any()).Return(user, sess, fail).AnyTimes()
	a.EXPECT().SignInEmail(gomock.Any(), gomock.Any()).Return(user, sess, fail).AnyTimes()
	a.EXPECT().SignOut(gomock.Any(), gomock.Any()).Return(fail).AnyTimes()
	a.EXPECT().RequestPasswordReset(gomock.Any(), gomock.Any(), gomock.Any()).Return(fail).AnyTimes()
	a.EXPECT().ResetPassword(gomock.Any(), gomock.Any(), gomock.Any()).Return(fail).AnyTimes()
	a.EXPECT().VerifyEmail(gomock.Any(), gomock.Any()).Return(user, sess, fail).AnyTimes()
	a.EXPECT().SendVerificationEmail(gomock.Any(), gomock.Any(), gomock.Any()).Return(fail).AnyTimes()
	a.EXPECT().SetSessionCookie(gomock.Any(), gomock.Any()).AnyTimes()
	a.EXPECT().ClearSessionCookie(gomock.Any()).AnyTimes()

	u.EXPECT().GetProfile(gomock.Any(), gomock.Any(), gomock.Any()).Return(services.User(user), fail).AnyTimes()
	u.EXPECT().UpdateProfile(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Return(services.User(user), fail).AnyTimes()
	u.EXPECT().RequestEmailChange(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Return(fail).AnyTimes()
	u.EXPECT().ChangePassword(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Return(fail).AnyTimes()
	u.EXPECT().ListSessions(gomock.Any(), gomock.Any(), gomock.Any()).Return(sessions, fail).AnyTimes()
	u.EXPECT().RevokeSession(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Return(fail).AnyTimes()
	u.EXPECT().RevokeOtherSessions(gomock.Any(), gomock.Any(), gomock.Any()).Return(fail).AnyTimes()
	u.EXPECT().DeleteAccount(gomock.Any(), gomock.Any(), gomock.Any()).Return(fail).AnyTimes()
	u.EXPECT().DataSummary(gomock.Any(), gomock.Any(), gomock.Any()).Return(services.UserDataSummary{User: services.User(user), Sessions: 2}, fail).AnyTimes()

	return ui.New(ui.Deps{
		Auth: a, Users: u,
		Features:      config.Features{TemplateWelcome: true, EmailVerification: true},
		SecureCookies: true,
		Logger:        slog.New(slog.NewTextHandler(io.Discard, nil)),
		LiveInterval:  time.Hour,
		UserFrom: func(context.Context) (ui.User, ui.Session, bool) {
			return user, sess, signedIn
		},
	}).Handler()
}

// crawl sends what a browser would. Streams are cut after their first write.
func crawl(h http.Handler, method, path string, ds bool) *httptest.ResponseRecorder {
	var body io.Reader
	if method == http.MethodPost {
		body = strings.NewReader(crawlForm.Encode())
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r := httptest.NewRequestWithContext(ctx, method, "https://app.example"+path, body)
	r.Header.Set("Accept", "text/html")
	r.Header.Set("Sec-Fetch-Site", "same-origin")
	if method == http.MethodPost {
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		r.Header.Set("Origin", "https://app.example")
	}
	if ds {
		r.Header.Set("Datastar-Request", "true")
	}
	rec := &firstWrite{ResponseRecorder: httptest.NewRecorder(), cancel: cancel}
	h.ServeHTTP(rec, r)
	return rec.ResponseRecorder
}

// firstWrite ends the request after the first flush, so live streams return.
type firstWrite struct {
	*httptest.ResponseRecorder
	cancel context.CancelFunc
	once   sync.Once
}

func (f *firstWrite) Flush() {
	f.ResponseRecorder.Flush()
	f.once.Do(f.cancel)
}

func checkResponse(t *testing.T, name string, rec *httptest.ResponseRecorder) {
	t.Helper()
	h, body := rec.Header(), rec.Body.String()
	for _, re := range leaks {
		if strings.HasPrefix(h.Get("Content-Type"), "text/javascript") && re.String() != "CANARY" && re.String() != `/api/` {
			continue // the Datastar bundle is pinned (TestDatastarPin); only check it for secrets and API paths
		}
		if m := re.FindString(body); m != "" {
			t.Errorf("%s: body contains %q (%s)", name, m, re)
		}
	}
	for k, vs := range h {
		for _, v := range vs {
			if strings.Contains(v, "CANARY") || strings.Contains(v, "/api/") {
				t.Errorf("%s: header %s leaks %q", name, k, v)
			}
		}
		if strings.HasPrefix(k, "Access-Control-") || k == "Server" || k == "X-Powered-By" || k == "Allow" {
			t.Errorf("%s: header %s is set", name, k)
		}
	}
	for _, c := range rec.Result().Cookies() {
		if !c.HttpOnly || !c.Secure || c.SameSite == http.SameSiteDefaultMode || c.SameSite == http.SameSiteNoneMode {
			t.Errorf("%s: cookie %s is not HttpOnly, Secure and SameSite: %s", name, c.Name, c.String())
		}
	}
	if ct := h.Get("Content-Type"); strings.Contains(ct, "json") {
		t.Errorf("%s: JSON response %q", name, ct)
	}

	csp := h.Get("Content-Security-Policy")
	for _, want := range []string{"default-src 'none'", "script-src 'self' 'nonce-", "style-src 'self';", "frame-ancestors 'none'", "base-uri 'none'", "object-src 'none'", "form-action 'self'"} {
		if !strings.Contains(csp, want) {
			t.Errorf("%s: CSP %q lacks %q", name, csp, want)
		}
	}
	if strings.Contains(csp, "unsafe-") {
		t.Errorf("%s: CSP allows %q", name, csp)
	}
	for k, want := range map[string]string{
		"X-Content-Type-Options":     "nosniff",
		"X-Frame-Options":            "DENY",
		"Referrer-Policy":            "strict-origin",
		"Strict-Transport-Security":  "max-age=63072000; includeSubDomains",
		"Cross-Origin-Opener-Policy": "same-origin",
	} {
		if got := h.Get(k); got != want {
			t.Errorf("%s: %s = %q, want %q", name, k, got, want)
		}
	}
	if !strings.Contains(h.Get("Permissions-Policy"), "camera=()") {
		t.Errorf("%s: Permissions-Policy = %q", name, h.Get("Permissions-Policy"))
	}
	if cc := h.Get("Cache-Control"); cc != "no-store" && !strings.Contains(h.Get("Content-Type"), "css") &&
		!strings.Contains(h.Get("Content-Type"), "javascript") && !strings.Contains(h.Get("Content-Type"), "icon") {
		t.Errorf("%s: Cache-Control = %q, want no-store", name, cc)
	}

	// Every script is this response's own: nonced, and the nonce is the
	// header's (Datastar reads it from <html data-nonce>).
	if m := cspNonce.FindStringSubmatch(csp); m != nil {
		for _, tag := range scriptTag.FindAllString(body, -1) {
			if n := nonceAttr.FindStringSubmatch(tag); n == nil || n[1] != m[1] {
				t.Errorf("%s: script without this response's nonce: %s", name, tag)
			}
		}
		if strings.Contains(body, "<html") && !strings.Contains(body, `data-nonce="`+m[1]+`"`) {
			t.Errorf("%s: <html> lacks data-nonce", name)
		}
	}
}
