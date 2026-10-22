package ui_test

// The exposure crawl (the web UI has the same one): every route the admin UI registers is requested in every
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

	"github.com/jayleaton/ai-patterns-go/apps/admin/internal/config"
	"github.com/jayleaton/ai-patterns-go/apps/admin/internal/services"
	"github.com/jayleaton/ai-patterns-go/apps/admin/ui"
	"github.com/jayleaton/ai-patterns-go/apps/admin/ui/mocks"
	"github.com/jayleaton/ai-patterns-go/apps/admin/ui/routes"
	"github.com/jayleaton/ai-patterns-go/apps/admin/ui/static"
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
	regexp.MustCompile(`\*?(services|auth|validate|model|adminrepo|pgx|pgconn|errors)\.[A-Z]\w+`), // Go type names
	regexp.MustCompile(`"data":|"error":`),                                                        // JSON envelopes
	regexp.MustCompile(`(?i)\son[a-z]+=`),                                                         // inline event handlers
	regexp.MustCompile(`\sstyle=`),                                                                // inline styles (the CSP has no 'unsafe-inline')
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
	{"GET " + config.AppRoutes.Login, []string{config.AppRoutes.Login, config.AppRoutes.Login + "?next=%2Fusers"}},
	{"POST " + config.AppRoutes.Login, []string{config.AppRoutes.Login}},
	{"GET " + config.AppRoutes.SetPassword, []string{config.AppRoutes.SetPassword, config.AppRoutes.SetPassword + "?token=invite-tok"}},
	{"POST " + config.AppRoutes.SetPassword, []string{config.AppRoutes.SetPassword}},
	{"GET " + config.AppRoutes.TwoFactor, []string{config.AppRoutes.TwoFactor}},
	{"POST " + config.AppRoutes.TwoFactor, []string{config.AppRoutes.TwoFactor}},
	{"GET " + config.AppRoutes.TwoFactorSetup, []string{config.AppRoutes.TwoFactorSetup}},
	{"POST " + config.AppRoutes.TwoFactorSetup, []string{config.AppRoutes.TwoFactorSetup}},
	{"POST " + routes.EnableTwoFactor, []string{routes.EnableTwoFactor}},
	{"GET " + config.AppRoutes.Users, []string{config.AppRoutes.Users, config.AppRoutes.Users + "?search=ada&banned=true&sortBy=email"}},
	{"GET " + config.AppRoutes.UserByID, []string{config.UserPage("u1")}},
	{"POST " + routes.UserBan, []string{routes.User(routes.UserBan, "u1")}},
	{"POST " + routes.UserUnban, []string{routes.User(routes.UserUnban, "u1")}},
	{"POST " + routes.UserRole, []string{routes.User(routes.UserRole, "u1")}},
	{"POST " + routes.UserSessions, []string{routes.User(routes.UserSessions, "u1")}},
	{"POST " + routes.UserSession, []string{routes.Session("u1", "us1")}},
	{"GET " + config.AppRoutes.Admins, []string{config.AppRoutes.Admins}},
	{"POST " + routes.AdminInvite, []string{routes.AdminInvite}},
	{"GET " + config.AppRoutes.Audit, []string{config.AppRoutes.Audit, config.AppRoutes.Audit + "?targetId=u1"}},
	{"GET " + config.AppRoutes.Analytics, []string{config.AppRoutes.Analytics}},
	{"POST " + routes.Logout, []string{routes.Logout}},
	{"POST " + routes.Locale, []string{routes.Locale}},
}

// crawlForm carries every field any admin form posts.
var crawlForm = url.Values{
	"email": {"root@example.com"}, "password": {"admin-password-1"}, "confirmPassword": {"admin-password-1"},
	"name": {"Root"}, "token": {"invite-tok"}, "code": {"123456"}, "method": {ui.MethodTOTP}, "trustDevice": {"on"},
	"secret": {"JBSWY3DPEHPK3PXP"}, "uri": {"otpauth://totp/Admin:root@example.com?secret=JBSWY3DPEHPK3PXP"},
	"banReason": {"spam"}, "banExpiresIn": {"3600"}, "role": {"admin"}, "locale": {"ja"}, "next": {"/users"},
}

func TestCrawlCoversEveryRoute(t *testing.T) {
	rec := &patternRecorder{}
	ctrl := gomock.NewController(t)
	ui.New(ui.Deps{
		Auth: mocks.NewMockAuth(ctrl), Users: mocks.NewMockUsers(ctrl), Admins: mocks.NewMockAdmins(ctrl),
		Audit: mocks.NewMockAudit(ctrl), Analytics: mocks.NewMockAnalytics(ctrl), UserFrom: userFrom,
	}).Routes(rec)
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
	a := mocks.NewMockAuth(ctrl)
	us, ad, au, an := mocks.NewMockUsers(ctrl), mocks.NewMockAdmins(ctrl), mocks.NewMockAudit(ctrl), mocks.NewMockAnalytics(ctrl)
	var fail error
	if failing {
		fail = errors.New(canaryInternal + " at " + canaryPath)
	}
	now := time.Now()
	admin := ui.Admin{ID: "a1", Name: "Root", Email: "root@example.com", TwoFactorEnabled: true}
	sess := ui.Session{ID: "s1", Token: canaryToken, ExpiresAt: now.Add(time.Hour)}
	setup := ui.TOTPSetup{Secret: "JBSWY3DPEHPK3PXP", URI: "otpauth://totp/Admin:root@example.com?secret=JBSWY3DPEHPK3PXP", BackupCodes: []string{"aaaa-bbbb", "cccc-dddd"}}
	ip, agent, reason := "192.0.2.1", "Mozilla/5.0 (X11; Linux x86_64)", "spam"
	user := services.User{ID: "u1", Name: "Ada", Email: "ada@example.com", EmailVerified: true, CreatedAt: now, UpdatedAt: now, Role: "user", Banned: true, BanReason: &reason, HasPassword: true}
	detail := services.UserDetail{User: user, Sessions: []services.Session{{ID: "us1", CreatedAt: now, ExpiresAt: now.Add(time.Hour), IPAddress: &ip, UserAgent: &agent}}}
	audit := services.AuditPage{Total: 1, Limit: 50, Entries: []services.AuditEntry{{ID: 1, ActorID: "a1", ActorEmail: "root@example.com", Action: services.ActionUserBan, TargetType: "user", TargetID: "u1", TargetLabel: "ada@example.com", Details: map[string]string{"reason": "spam"}, CreatedAt: now}}}

	a.EXPECT().SignIn(gomock.Any(), gomock.Any(), gomock.Any()).Return(sess, fail).AnyTimes()
	a.EXPECT().SignOut(gomock.Any(), gomock.Any()).Return(fail).AnyTimes()
	a.EXPECT().CompleteSetPassword(gomock.Any(), gomock.Any(), gomock.Any()).Return(sess, fail).AnyTimes()
	a.EXPECT().VerifyTwoFactor(gomock.Any(), gomock.Any()).Return(sess, "trust-tok", fail).AnyTimes()
	a.EXPECT().StartTwoFactorSetup(gomock.Any()).Return(setup, fail).AnyTimes()
	a.EXPECT().EnableTwoFactor(gomock.Any(), gomock.Any(), gomock.Any()).Return(setup, fail).AnyTimes()
	a.EXPECT().ConfirmTwoFactor(gomock.Any(), gomock.Any(), gomock.Any()).Return(fail).AnyTimes()
	a.EXPECT().SetSessionCookie(gomock.Any(), gomock.Any()).AnyTimes()
	a.EXPECT().ClearSessionCookie(gomock.Any()).AnyTimes()
	a.EXPECT().SetTwoFactorCookie(gomock.Any(), gomock.Any()).AnyTimes()
	a.EXPECT().SetTrustDeviceCookie(gomock.Any(), gomock.Any()).AnyTimes()

	us.EXPECT().ListUsers(gomock.Any(), gomock.Any(), gomock.Any()).Return(services.UserPage{Users: []services.User{user}, Total: 1, Limit: 50}, fail).AnyTimes()
	us.EXPECT().GetUser(gomock.Any(), gomock.Any(), gomock.Any()).Return(detail, fail).AnyTimes()
	us.EXPECT().BanUser(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Return(fail).AnyTimes()
	us.EXPECT().UnbanUser(gomock.Any(), gomock.Any(), gomock.Any()).Return(fail).AnyTimes()
	us.EXPECT().SetRole(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Return(fail).AnyTimes()
	us.EXPECT().RevokeSession(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Return(fail).AnyTimes()
	us.EXPECT().RevokeSessions(gomock.Any(), gomock.Any(), gomock.Any()).Return(fail).AnyTimes()
	ad.EXPECT().ListAdmins(gomock.Any(), gomock.Any()).Return([]services.Admin{{ID: "a1", Name: "Root", Email: "root@example.com", CreatedAt: now, TwoFactorEnabled: true, HasPassword: true}}, fail).AnyTimes()
	ad.EXPECT().InviteAdmin(gomock.Any(), gomock.Any(), gomock.Any()).Return(services.Admin{ID: "a2", Email: "new@example.com", CreatedAt: now}, fail).AnyTimes()
	au.EXPECT().List(gomock.Any(), gomock.Any(), gomock.Any()).Return(audit, fail).AnyTimes()
	an.EXPECT().Overview(gomock.Any(), gomock.Any()).Return(services.Overview{TotalUsers: 3, VerifiedUsers: 2, BannedUsers: 1, GeneratedAt: now}, fail).AnyTimes()

	return ui.New(ui.Deps{
		Auth: a, Users: us, Admins: ad, Audit: au, Analytics: an,
		UserFrom: func(context.Context) (ui.Admin, ui.Session, bool) {
			return admin, sess, signedIn
		},
		RequireTwoFactor: true,
		SecureCookies:    true,
		Logger:           slog.New(slog.NewTextHandler(io.Discard, nil)),
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
	r := httptest.NewRequestWithContext(ctx, method, "https://admin.example"+path, body)
	r.Header.Set("Accept", "text/html")
	r.Header.Set("Sec-Fetch-Site", "same-origin")
	if method == http.MethodPost {
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		r.Header.Set("Origin", "https://admin.example")
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
