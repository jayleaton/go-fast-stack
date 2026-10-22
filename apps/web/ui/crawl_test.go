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
	"html"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
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
	// xssProbe goes wherever user data or the query reaches a page: it must
	// come out escaped, never as a tag or as code in a data-* expression.
	xssProbe = `X'+XSSPROBE+'"><img data-init="XSSPROBE">`
	// evil is where open-redirect probes try to send the browser.
	evil = "evil.example"
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
	dataInit  = regexp.MustCompile(`\sdata-init="([^"]*)"`)
	autoPost  = regexp.MustCompile(`@(post|put|patch|delete)\(`)
	dataAttr  = regexp.MustCompile(`\sdata-[\w:.\-]+="([^"]*)"`)
	// jsBreakout is the probe's quote closing a JS string: not escaped.
	jsBreakout = regexp.MustCompile(`(^|[^\\])'\+XSSPROBE\+'`)
	// navTarget is where a link, assign() or Datastar redirect sends the browser.
	navTarget = regexp.MustCompile(`(?:assign\('|location\.href = "|href=")([^'"]*)`)
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
	{"GET " + config.AppRoutes.ResetPassword, []string{config.AppRoutes.ResetPassword, config.AppRoutes.ResetPassword + "?token=" + url.QueryEscape(xssProbe)}},
	{"POST " + config.AppRoutes.ResetPassword, []string{config.AppRoutes.ResetPassword}},
	{"GET " + config.AppRoutes.VerifyEmail, []string{config.AppRoutes.VerifyEmail + "?email=demo%40example.com", config.AppRoutes.VerifyEmail + "?email=" + url.QueryEscape(xssProbe)}},
	{"POST " + config.AppRoutes.VerifyEmail, []string{config.AppRoutes.VerifyEmail}},
	{"GET " + config.AppRoutes.VerifyEmailSuccess, append([]string{
		config.AppRoutes.VerifyEmailSuccess, config.AppRoutes.VerifyEmailSuccess + "?error=INVALID_TOKEN",
	}, verifyProbes...)},
	{"POST " + config.AppRoutes.VerifyEmailSuccess, []string{config.AppRoutes.VerifyEmailSuccess}},
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

// verifyProbes are verification links whose token is markup and whose
// callbackURL tries to leave the site.
var verifyProbes = []string{
	config.AppRoutes.VerifyEmailSuccess + "?token=verify-tok&callbackURL=%2Fdashboard",
	config.AppRoutes.VerifyEmailSuccess + "?token=" + url.QueryEscape(xssProbe+"-tok") + "&callbackURL=" + url.QueryEscape("cb-"+xssProbe),
	config.AppRoutes.VerifyEmailSuccess + "?token=probe-tok-7c1&callbackURL=%2F%09%2F" + evil,
	config.AppRoutes.VerifyEmailSuccess + "?token=probe-tok-7c1&callbackURL=%2F%0A%2F" + evil,
	config.AppRoutes.VerifyEmailSuccess + "?token=probe-tok-7c1&callbackURL=%2F%5C" + evil,
	config.AppRoutes.VerifyEmailSuccess + "?token=probe-tok-7c1&callbackURL=https%3A%2F%2F" + evil,
	config.AppRoutes.VerifyEmailSuccess + "?callbackURL=%2F%09%2F" + evil,
}

// crawlForm carries every field any UI form posts; next tries to leave the
// site and the name carries markup.
var crawlForm = url.Values{
	"email": {"demo@example.com"}, "password": {"password123"}, "confirmPassword": {"password123"},
	"name": {xssProbe}, "image": {""}, "newEmail": {"new@example.com"},
	"currentPassword": {"password123"}, "newPassword": {"password456"}, "revokeOtherSessions": {"on"},
	"sessionId": {"sess-2"}, "token": {"reset-tok"}, "locale": {"ja"}, "next": {"/\t/" + evil},
	"callbackURL": {"/\t/" + evil},
}

func TestCrawlCoversEveryRoute(t *testing.T) {
	t.Parallel()
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

// escapeHatch is templ's unescaped output. templ escaping is what stops
// injected markup (the CSP cannot: Datastar runs any data-* expression with
// the page nonce), so templates and handlers never bypass it. Generated
// _templ.go files are skipped by golangci-lint, so this scans the sources.
var escapeHatch = regexp.MustCompile(`templ\.(Raw|Unsafe\w*|SafeScript\w*|SafeCSS\w*|JSUnsafeFuncCall|ComponentScript)\b`)

func TestNoUnescapedOutput(t *testing.T) {
	t.Parallel()
	err := filepath.WalkDir(".", func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || strings.HasSuffix(path, "_templ.go") || strings.HasSuffix(path, "_test.go") ||
			(!strings.HasSuffix(path, ".templ") && !strings.HasSuffix(path, ".go")) {
			return err
		}
		src, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		for _, m := range escapeHatch.FindAllString(string(src), -1) {
			t.Errorf("%s uses %s: render through templ's escaping instead", path, m)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestCrawlExposesNothing(t *testing.T) {
	t.Parallel()
	// One parallel subtest per state and method, each with one handler (and
	// mock controller) built for the method it serves: notOnGET needs no
	// shared state, and a handler per state keeps setup off the CPU budget.
	for _, failing := range []bool{false, true} {
		for _, signedIn := range []bool{false, true} {
			for _, method := range []string{http.MethodGet, http.MethodPost} {
				t.Run(strings.Join([]string{state(failing, "failing", "ok"), state(signedIn, "signed-in", "anonymous"), method}, "/"), func(t *testing.T) {
					t.Parallel()
					h := crawlUI(t, method, failing, signedIn)
					for _, c := range crawlTargets {
						if m, _, _ := strings.Cut(c.pattern, " "); m != method {
							continue
						}
						for _, path := range c.paths {
							for _, ds := range []bool{false, true} {
								name := strings.Join([]string{method, path, state(failing, "failing", "ok"), state(signedIn, "signed-in", "anonymous"), state(ds, "datastar", "plain")}, " ")
								checkResponse(t, name, method, path, crawl(h, method, path, ds))
							}
						}
					}
				})
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
// or (failing) internal errors carrying canary text. It serves only requests
// with method, which notOnGET checks state-changing port calls against.
func crawlUI(t *testing.T, method string, failing, signedIn bool) http.Handler {
	ctrl := gomock.NewController(t)
	a, u := mocks.NewMockAuth(ctrl), mocks.NewMockUsers(ctrl)
	var fail error
	if failing {
		fail = errors.New(canaryInternal + " at " + canaryPath)
	}
	sess := ui.Session{ID: "sess-1", Token: canaryToken, UserID: "u1", ExpiresAt: time.Now().Add(time.Hour)}
	user := ui.User{ID: "u1", Name: "Demo " + xssProbe, Email: "demo@example.com", EmailVerified: true, CreatedAt: time.Now()}
	ip, agent := "192.0.2.1", "Mozilla/5.0 "+xssProbe
	sessions := []services.Session{
		{ID: "sess-1", CreatedAt: time.Now(), ExpiresAt: time.Now().Add(time.Hour), IPAddress: &ip, UserAgent: &agent, Current: true},
		{ID: "sess-2", CreatedAt: time.Now(), ExpiresAt: time.Now().Add(time.Hour), IPAddress: &ip, UserAgent: &agent},
	}

	a.EXPECT().SignUpEmail(notOnGET(t, method, "SignUpEmail"), gomock.Any()).Return(user, sess, fail).AnyTimes()
	a.EXPECT().SignInEmail(notOnGET(t, method, "SignInEmail"), gomock.Any()).Return(user, sess, fail).AnyTimes()
	a.EXPECT().SignOut(notOnGET(t, method, "SignOut"), gomock.Any()).Return(fail).AnyTimes()
	a.EXPECT().RequestPasswordReset(notOnGET(t, method, "RequestPasswordReset"), gomock.Any(), gomock.Any()).Return(fail).AnyTimes()
	a.EXPECT().ResetPassword(notOnGET(t, method, "ResetPassword"), gomock.Any(), gomock.Any()).Return(fail).AnyTimes()
	a.EXPECT().VerifyEmail(notOnGET(t, method, "VerifyEmail"), gomock.Any()).Return(user, sess, fail).AnyTimes()
	a.EXPECT().SendVerificationEmail(notOnGET(t, method, "SendVerificationEmail"), gomock.Any(), gomock.Any()).Return(fail).AnyTimes()
	a.EXPECT().CheckCallbackURL(gomock.Any()).DoAndReturn(func(cb string) error {
		if cb == "betterstack://verified" { // the mobile app's deep link
			return nil
		}
		return errors.New("untrusted callback")
	}).AnyTimes()
	a.EXPECT().SetSessionCookie(notOnGET(t, method, "SetSessionCookie"), gomock.Any()).AnyTimes()
	a.EXPECT().ClearSessionCookie(notOnGET(t, method, "ClearSessionCookie")).AnyTimes()

	u.EXPECT().GetProfile(gomock.Any(), gomock.Any(), gomock.Any()).Return(services.User(user), fail).AnyTimes()
	u.EXPECT().UpdateProfile(notOnGET(t, method, "UpdateProfile"), gomock.Any(), gomock.Any(), gomock.Any()).Return(services.User(user), fail).AnyTimes()
	u.EXPECT().RequestEmailChange(notOnGET(t, method, "RequestEmailChange"), gomock.Any(), gomock.Any(), gomock.Any()).Return(fail).AnyTimes()
	u.EXPECT().ChangePassword(notOnGET(t, method, "ChangePassword"), gomock.Any(), gomock.Any(), gomock.Any()).Return(fail).AnyTimes()
	u.EXPECT().ListSessions(gomock.Any(), gomock.Any(), gomock.Any()).Return(sessions, fail).AnyTimes()
	u.EXPECT().RevokeSession(notOnGET(t, method, "RevokeSession"), gomock.Any(), gomock.Any(), gomock.Any()).Return(fail).AnyTimes()
	u.EXPECT().RevokeOtherSessions(notOnGET(t, method, "RevokeOtherSessions"), gomock.Any(), gomock.Any()).Return(fail).AnyTimes()
	u.EXPECT().DeleteAccount(notOnGET(t, method, "DeleteAccount"), gomock.Any(), gomock.Any()).Return(fail).AnyTimes()
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

// notOnGET matches any argument but fails the test when a state-changing
// port is called while a GET is served (method is the one method the
// handler under test serves): mail scanners and link previews fetch links,
// so opening a page (an emailed link above all) must never spend a token,
// sign in or change anything.
func notOnGET(t *testing.T, method, port string) gomock.Matcher {
	return gomock.Cond(func(any) bool {
		if method == http.MethodGet {
			t.Errorf("GET called %s: only a POST may change state", port)
		}
		return true
	})
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
	rec := &firstEvent{ResponseRecorder: httptest.NewRecorder(), cancel: cancel}
	h.ServeHTTP(rec, r)
	return rec.ResponseRecorder
}

// firstEvent ends the request once a whole SSE event has been flushed, so
// live streams return; every other response runs to completion.
type firstEvent struct {
	*httptest.ResponseRecorder
	cancel context.CancelFunc
	once   sync.Once
}

func (f *firstEvent) Flush() {
	f.ResponseRecorder.Flush()
	if strings.Contains(f.Body.String(), "\n\n") {
		f.once.Do(f.cancel)
	}
}

func checkResponse(t *testing.T, name, method, path string, rec *httptest.ResponseRecorder) {
	t.Helper()
	h, body := rec.Header(), rec.Body.String()
	checkBody(t, name, h.Get("Content-Type"), body)
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
		"Strict-Transport-Security":  "max-age=63072000; includeSubDomains",
		"Cross-Origin-Opener-Policy": "same-origin",
	} {
		if got := h.Get(k); got != want {
			t.Errorf("%s: %s = %q, want %q", name, k, got, want)
		}
	}
	// An emailed token in the URL: never sent on as a Referer, and rendered
	// only as the page's own hidden token field.
	tok := ""
	if u, err := url.Parse(path); err == nil && method == http.MethodGet {
		tok = u.Query().Get("token")
	}
	if rp := h.Get("Referrer-Policy"); (tok != "" && rp != "no-referrer") || (rp != "no-referrer" && rp != "strict-origin") {
		t.Errorf("%s: Referrer-Policy = %q", name, rp)
	}
	if tok != "" {
		esc := html.EscapeString(tok)
		field := `<input type="hidden" name="token" value="` + esc + `">`
		enc := url.QueryEscape(tok)
		if strings.Count(body, esc) != strings.Count(body, field) || (enc != esc && strings.Contains(body, enc)) {
			t.Errorf("%s: token rendered outside its hidden field", name)
		}
	}
	// Nothing posts on load: a page acts only when the person submits it.
	for _, v := range dataInit.FindAllStringSubmatch(body, -1) {
		if autoPost.MatchString(html.UnescapeString(v[1])) {
			t.Errorf("%s: data-init sends a request on load: %s", name, v[0])
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
	// header's (Datastar reads it from <html data-nonce>). An SSE patch may
	// carry only Datastar's redirect script, which it re-creates with the
	// page's nonce.
	sse := strings.HasPrefix(h.Get("Content-Type"), "text/event-stream")
	if sse && !strings.Contains(body, "event: datastar-") {
		t.Errorf("%s: SSE response without an event: nothing was checked", name)
	}
	if m := cspNonce.FindStringSubmatch(csp); m != nil {
		for _, tag := range scriptTag.FindAllString(body, -1) {
			if sse && tag == `<script data-effect="el.remove()">` {
				continue
			}
			if n := nonceAttr.FindStringSubmatch(tag); sse || n == nil || n[1] != m[1] {
				t.Errorf("%s: script without this response's nonce: %s", name, tag)
			}
		}
		if strings.Contains(body, "<html") && !strings.Contains(body, `data-nonce="`+m[1]+`"`) {
			t.Errorf("%s: <html> lacks data-nonce", name)
		}
	}

	// Open-redirect probes never become a destination.
	if loc := h.Get("Location"); strings.Contains(loc, evil) {
		t.Errorf("%s: redirects to %s", name, loc)
	}
}

// checkedBodies holds the bodies (content type + body) that passed checkBody.
// Static assets come back byte-identical in every state and mode, and
// re-scanning them is most of the crawl's time under -race; a failing body
// is not stored, so it is reported for every response that carries it.
var checkedBodies sync.Map

// checkBody runs the checks that depend only on the response body.
func checkBody(t *testing.T, name, contentType, body string) {
	t.Helper()
	key := contentType + "\x00" + body
	if _, ok := checkedBodies.Load(key); ok {
		return
	}
	ok := true
	errorf := func(format string, args ...any) {
		t.Helper()
		ok = false
		t.Errorf(format, args...)
	}
	for _, re := range leaks {
		if strings.HasPrefix(contentType, "text/javascript") && re.String() != "CANARY" && re.String() != `/api/` {
			continue // the Datastar bundle is pinned (TestDatastarPin); only check it for secrets and API paths
		}
		if m := re.FindString(body); m != "" {
			errorf("%s: body contains %q (%s)", name, m, re)
		}
	}
	// Injected markup stays text, and never breaks out of a JS string in a
	// Datastar expression.
	if strings.Contains(body, `<img data-init="XSSPROBE"`) {
		errorf("%s: probe rendered as a tag", name)
	}
	for _, v := range dataAttr.FindAllStringSubmatch(body, -1) {
		if jsBreakout.MatchString(html.UnescapeString(v[1])) {
			errorf("%s: probe breaks out of a data-* expression: %s", name, v[0])
		}
	}
	for _, m := range navTarget.FindAllStringSubmatch(html.UnescapeString(body), -1) {
		// Browsers drop tabs and newlines and treat \ as /.
		target := strings.NewReplacer("\t", "", "\n", "", "\r", "", `\\`, "/").Replace(m[1])
		if strings.Contains(target, evil) && (strings.HasPrefix(target, "//") || strings.Contains(target, "://")) {
			errorf("%s: navigates off-site: %q", name, m[1])
		}
	}
	if ok {
		checkedBodies.Store(key, struct{}{})
	}
}
