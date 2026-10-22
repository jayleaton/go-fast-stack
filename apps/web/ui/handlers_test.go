package ui_test

import (
	"bufio"
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/jayleaton/ai-patterns-go/apps/web/ui"
	"github.com/jayleaton/ai-patterns-go/apps/web/ui/routes"
	"github.com/jayleaton/ai-patterns-go/apps/web/ui/uifake"
)

type env struct {
	t     *testing.T
	store *uifake.Store
	h     http.Handler
	user  ui.User
}

func newEnv(t *testing.T, f ui.Features) *env {
	t.Helper()
	store := uifake.NewStore()
	user := store.AddUser("Demo User", "demo@example.com", "password123", true)
	auth := uifake.Auth{S: store}
	mux := http.NewServeMux()
	ui.New(ui.Deps{
		Auth:         auth,
		Users:        uifake.Users{S: store},
		Features:     f,
		Logger:       slog.New(slog.NewTextHandler(io.Discard, nil)),
		LiveInterval: time.Hour, // the stream test drives events, not ticks
	}).Routes(mux)
	return &env{t: t, store: store, h: auth.Middleware(mux), user: user}
}

type reqOpt func(*http.Request)

// datastar marks the request as a Datastar action (SSE response).
func datastar(r *http.Request) { r.Header.Set("Datastar-Request", "true") }

func lang(l string) reqOpt { return func(r *http.Request) { r.Header.Set("Accept-Language", l) } }

func session(token string) reqOpt {
	return func(r *http.Request) { r.AddCookie(&http.Cookie{Name: uifake.CookieName, Value: token}) }
}

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

func (e *env) signedIn() string {
	return e.store.NewSession(e.user.ID).Token
}

func sessionCookie(rec *httptest.ResponseRecorder) *http.Cookie {
	for _, c := range rec.Result().Cookies() {
		if c.Name == uifake.CookieName {
			return c
		}
	}
	return nil
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

func TestPages(t *testing.T) {
	e := newEnv(t, ui.Features{TemplateWelcome: true})
	pages := []string{
		routes.Home, routes.Why, routes.Login, routes.Signup, routes.ForgotPassword,
		routes.ResetPassword + "?token=abc", routes.VerifyEmail + "?email=a%40b.c", routes.VerifyEmailSuccess,
	}
	for _, l := range []string{"en", "ja"} {
		for _, p := range pages {
			rec := e.do("GET", p, nil, lang(l))
			if rec.Code != http.StatusOK {
				t.Errorf("%s %s: status %d", l, p, rec.Code)
			}
			if got := rec.Header().Get("Content-Language"); got != l {
				t.Errorf("%s %s: Content-Language %q", l, p, got)
			}
		}
	}

	if rec := e.do("GET", routes.Dashboard, nil); rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != routes.Login {
		t.Errorf("dashboard signed out: %d %q", rec.Code, rec.Header().Get("Location"))
	}
	tok := e.signedIn()
	rec := e.do("GET", routes.Dashboard, nil, session(tok), lang("ja"))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "ダッシュボード") || !strings.Contains(rec.Body.String(), "Demo User") {
		t.Errorf("dashboard signed in: %d", rec.Code)
	}
	if rec := e.do("GET", routes.Home, nil, session(tok)); rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != routes.Dashboard {
		t.Errorf("home signed in: %d %q", rec.Code, rec.Header().Get("Location"))
	}

	off := newEnv(t, ui.Features{})
	if rec := off.do("GET", routes.Why, nil); rec.Code != http.StatusNotFound {
		t.Errorf("why with welcome off: %d", rec.Code)
	}
	if rec := off.do("GET", routes.Home, nil); !strings.Contains(rec.Body.String(), "Your app is running") {
		t.Error("home with welcome off is not the plain page")
	}
	if rec := e.do("GET", "/favicon.ico", nil); rec.Code != http.StatusMovedPermanently {
		t.Errorf("favicon: %d", rec.Code)
	}
}

func TestLogin(t *testing.T) {
	e := newEnv(t, ui.Features{})

	rec := e.do("POST", routes.Login, url.Values{"email": {"demo@example.com"}, "password": {"nope"}}, datastar, lang("ja"))
	assertSSE(t, rec, `id="form-error"`, "無効な認証情報")
	if sessionCookie(rec) != nil {
		t.Error("cookie set on failed login")
	}

	rec = e.do("POST", routes.Login, url.Values{"email": {"demo@example.com"}, "password": {"password123"}}, datastar)
	assertRedirectScript(t, rec, routes.Dashboard)
	if c := sessionCookie(rec); c == nil || c.Value == "" || !c.HttpOnly {
		t.Fatalf("session cookie: %+v", c)
	}

	// Without JS the same form posts normally.
	rec = e.do("POST", routes.Login, url.Values{"email": {"demo@example.com"}, "password": {"nope"}})
	if rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), "Invalid credentials") || !strings.Contains(rec.Body.String(), `value="demo@example.com"`) {
		t.Errorf("no-JS failed login: %d", rec.Code)
	}
	rec = e.do("POST", routes.Login, url.Values{"email": {"demo@example.com"}, "password": {"password123"}})
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != routes.Dashboard || sessionCookie(rec) == nil {
		t.Errorf("no-JS login: %d %q", rec.Code, rec.Header().Get("Location"))
	}
}

func TestLoginUnverifiedResendsLink(t *testing.T) {
	e := newEnv(t, ui.Features{EmailVerification: true})
	e.store.RequireEmailVerification = true
	e.store.AddUser("New", "new@example.com", "password123", false)
	rec := e.do("POST", routes.Login, url.Values{"email": {"new@example.com"}, "password": {"password123"}}, datastar)
	assertSSE(t, rec, "We sent you an email with a verification link")
	if e.store.Token("verify") == "" {
		t.Error("no verification email sent")
	}
}

func TestSignup(t *testing.T) {
	e := newEnv(t, ui.Features{})
	form := url.Values{"name": {"Grace"}, "email": {"grace@example.com"}, "password": {"password123"}}
	rec := e.do("POST", routes.Signup, form, datastar)
	assertRedirectScript(t, rec, routes.Dashboard)
	if sessionCookie(rec) == nil {
		t.Error("no session after signup")
	}

	rec = e.do("POST", routes.Signup, form, datastar)
	assertSSE(t, rec, "Email already in use")

	rec = e.do("POST", routes.Signup, url.Values{"name": {"X"}, "email": {"x@example.com"}, "password": {"short"}}, datastar)
	assertSSE(t, rec, "Password must be at least 8 characters")

	v := newEnv(t, ui.Features{EmailVerification: true})
	v.store.RequireEmailVerification = true
	rec = v.do("POST", routes.Signup, form, datastar)
	assertRedirectScript(t, rec, routes.VerifyEmail+"?email=grace%40example.com")
	if sessionCookie(rec) != nil {
		t.Error("signed in before verifying")
	}
}

func TestForgotAndResetPassword(t *testing.T) {
	e := newEnv(t, ui.Features{})
	rec := e.do("POST", routes.ForgotPassword, url.Values{"email": {"demo@example.com"}}, datastar)
	assertSSE(t, rec, `id="forgot-password"`, "Email sent!")
	tok := e.store.Token("reset")
	if tok == "" {
		t.Fatal("no reset email")
	}

	rec = e.do("POST", routes.ResetPassword, url.Values{"token": {tok}, "password": {"newpassword1"}, "confirmPassword": {"other12345"}}, datastar)
	assertSSE(t, rec, `id="form-error"`, "Passwords don&#39;t match")

	rec = e.do("POST", routes.ResetPassword, url.Values{"token": {tok}, "password": {"newpassword1"}, "confirmPassword": {"newpassword1"}}, datastar)
	assertSSE(t, rec, `id="reset-password"`, "Password reset successfully!")

	rec = e.do("POST", routes.ResetPassword, url.Values{"token": {tok}, "password": {"newpassword1"}, "confirmPassword": {"newpassword1"}}, datastar)
	assertSSE(t, rec, "Invalid reset link")

	rec = e.do("POST", routes.Login, url.Values{"email": {"demo@example.com"}, "password": {"newpassword1"}}, datastar)
	assertRedirectScript(t, rec, routes.Dashboard)
}

func TestVerifyEmail(t *testing.T) {
	e := newEnv(t, ui.Features{EmailVerification: true})
	e.store.RequireEmailVerification = true
	e.store.AddUser("New", "new@example.com", "password123", false)

	rec := e.do("POST", routes.VerifyEmail, url.Values{"email": {"new@example.com"}}, datastar, lang("ja"))
	assertSSE(t, rec, `id="resend"`, "{_cooldown: 300}", "確認メールを送信しました")
	tok := e.store.Token("verify")

	page := e.do("GET", routes.VerifyEmailSuccess+"?token="+tok, nil)
	if !strings.Contains(page.Body.String(), "@post(&#39;/auth/verify-email/success?token="+tok) {
		t.Fatal("success page does not post the token on load")
	}

	rec = e.do("POST", routes.VerifyEmailSuccess+"?token="+tok, nil, datastar)
	assertSSE(t, rec, `id="verify-result"`, "Email verified successfully!")
	if sessionCookie(rec) == nil {
		t.Error("not signed in after verifying")
	}

	rec = e.do("POST", routes.VerifyEmailSuccess+"?token="+tok, nil, datastar)
	assertSSE(t, rec, "Verification failed", "Invalid or expired token")

	rec = e.do("POST", routes.VerifyEmailSuccess, nil, datastar)
	assertSSE(t, rec, "automatic login failed")
}

func TestSettingsAndDelete(t *testing.T) {
	e := newEnv(t, ui.Features{})
	e.store.AddUser("Other", "taken@example.com", "password123", true)
	tok := e.signedIn()

	rec := e.do("POST", routes.UpdateSettings, url.Values{"name": {"Ada"}, "email": {"ada@example.com"}}, datastar, session(tok))
	assertSSE(t, rec, `id="settings-status"`, "Settings saved.", `id="account-card"`, "Ada", "ada@example.com")

	rec = e.do("POST", routes.UpdateSettings, url.Values{"name": {"Ada"}, "email": {"taken@example.com"}}, datastar, session(tok))
	assertSSE(t, rec, "Email already in use")

	rec = e.do("POST", routes.UpdateSettings, url.Values{"name": {""}, "email": {"ada@example.com"}}, datastar, session(tok), lang("ja"))
	assertSSE(t, rec, "入力内容を確認して")

	rec = e.do("POST", routes.UpdateSettings, url.Values{"name": {"Ada"}}, datastar)
	assertRedirectScript(t, rec, routes.Login)

	rec = e.do("POST", routes.DeleteAccount, nil, datastar, session(tok))
	assertRedirectScript(t, rec, routes.Home)
	if c := sessionCookie(rec); c == nil || c.MaxAge >= 0 {
		t.Errorf("session cookie not cleared: %+v", c)
	}
	if rec := e.do("GET", routes.Dashboard, nil, session(tok)); rec.Code != http.StatusSeeOther {
		t.Errorf("dashboard after delete: %d", rec.Code)
	}
	rec = e.do("POST", routes.Login, url.Values{"email": {"ada@example.com"}, "password": {"password123"}}, datastar)
	assertSSE(t, rec, "Invalid credentials")
}

func TestLogout(t *testing.T) {
	e := newEnv(t, ui.Features{})
	tok := e.signedIn()
	rec := e.do("POST", routes.Logout, nil, session(tok))
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != routes.Login {
		t.Fatalf("logout: %d %q", rec.Code, rec.Header().Get("Location"))
	}
	if rec := e.do("GET", routes.Dashboard, nil, session(tok)); rec.Code != http.StatusSeeOther {
		t.Errorf("session still valid after logout: %d", rec.Code)
	}
}

func TestLocaleSwitch(t *testing.T) {
	e := newEnv(t, ui.Features{})
	rec := e.do("POST", routes.Locale, url.Values{"locale": {"ja"}, "next": {"/login?x=1"}})
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/login?x=1" {
		t.Fatalf("switch: %d %q", rec.Code, rec.Header().Get("Location"))
	}
	var lc *http.Cookie
	for _, c := range rec.Result().Cookies() {
		if c.Name == "locale" {
			lc = c
		}
	}
	if lc == nil || lc.Value != "ja" {
		t.Fatalf("locale cookie: %+v", lc)
	}
	page := e.do("GET", routes.Login, nil, func(r *http.Request) { r.AddCookie(lc) }, lang("en"))
	if !strings.Contains(page.Body.String(), "アカウントにサインイン") {
		t.Error("cookie locale not applied over Accept-Language")
	}
	for _, next := range []string{"//evil.example", "https://evil.example", `/\evil.example`} {
		rec := e.do("POST", routes.Locale, url.Values{"locale": {"en"}, "next": {next}})
		if rec.Header().Get("Location") != routes.Home {
			t.Errorf("next %q redirected to %q", next, rec.Header().Get("Location"))
		}
	}
}

func TestActionsRejectCrossOrigin(t *testing.T) {
	e := newEnv(t, ui.Features{})
	rec := e.do("POST", routes.Login, url.Values{"email": {"demo@example.com"}, "password": {"password123"}}, func(r *http.Request) {
		r.Header.Set("Sec-Fetch-Site", "cross-site")
	})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("cross-site post: %d", rec.Code)
	}
}

// sseReader reads Datastar events from a live stream.
type sseReader struct {
	t  *testing.T
	sc *bufio.Scanner
}

// next returns the data of the next event.
func (s *sseReader) next() string {
	s.t.Helper()
	var data strings.Builder
	for s.sc.Scan() {
		line := s.sc.Text()
		if line == "" {
			if data.Len() > 0 {
				return data.String()
			}
			continue
		}
		if d, ok := strings.CutPrefix(line, "data: "); ok {
			data.WriteString(d + "\n")
		}
	}
	s.t.Fatalf("stream ended: %v", s.sc.Err())
	return ""
}

// TestLiveStream covers the server-push example end to end over a real
// connection: initial patch, cross-tab update with notice, own-tab update
// without notice, and the redirect when the account is deleted.
func TestLiveStream(t *testing.T) {
	e := newEnv(t, ui.Features{})
	srv := httptest.NewServer(e.h)
	defer srv.Close()
	tok := e.signedIn()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", srv.URL+routes.DashboardLive+"?tab=A", nil)
	req.Header.Set("Datastar-Request", "true")
	req.AddCookie(&http.Cookie{Name: uifake.CookieName, Value: tok})
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if ct := res.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("content-type %q", ct)
	}
	s := &sseReader{t: t, sc: bufio.NewScanner(res.Body)}
	if ev := s.next(); !strings.Contains(ev, `id="live-body"`) || !strings.Contains(ev, "Server time") {
		t.Fatalf("first event: %s", ev)
	}

	post := func(form url.Values) {
		t.Helper()
		rec := e.do("POST", routes.UpdateSettings, form, datastar, session(tok))
		assertSSE(t, rec, "Settings saved.")
	}

	post(url.Values{"name": {"From B"}, "email": {"demo@example.com"}, "tab": {"B"}})
	if ev := s.next(); !strings.Contains(ev, `id="account-card"`) || !strings.Contains(ev, "From B") {
		t.Fatalf("expected account card patch, got: %s", ev)
	}
	if ev := s.next(); !strings.Contains(ev, `id="live-notice"`) {
		t.Fatalf("expected notice for another tab's change, got: %s", ev)
	}
	_ = s.next() // live body after the event

	post(url.Values{"name": {"From A"}, "email": {"demo@example.com"}, "tab": {"A"}})
	if ev := s.next(); !strings.Contains(ev, "From A") {
		t.Fatalf("expected account card patch, got: %s", ev)
	}
	if ev := s.next(); !strings.Contains(ev, `id="live-body"`) {
		t.Fatalf("own-tab change must not show the notice, got: %s", ev)
	}

	rec := e.do("POST", routes.DeleteAccount, nil, datastar, session(tok))
	assertRedirectScript(t, rec, routes.Home)
	if ev := s.next(); !strings.Contains(ev, `window.location.href = "/"`) {
		t.Fatalf("expected redirect after delete, got: %s", ev)
	}
}
