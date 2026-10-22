package ui_test

import (
	"context"
	"html"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"go.uber.org/mock/gomock"

	"github.com/jayleaton/ai-patterns-go/apps/web/internal/config"
	"github.com/jayleaton/ai-patterns-go/apps/web/internal/services"
	"github.com/jayleaton/ai-patterns-go/apps/web/ui"
	"github.com/jayleaton/ai-patterns-go/apps/web/ui/i18n"
	"github.com/jayleaton/ai-patterns-go/apps/web/ui/mocks"
)

// The signed-in caller of the tests (when signedIn is called).
var (
	demoUser    = ui.User{ID: "u1", Name: "Demo User", Email: "demo@example.com", EmailVerified: true, CreatedAt: time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)}
	demoSession = ui.Session{ID: "sess-1", Token: "tok-1", UserID: "u1", ExpiresAt: time.Date(2026, 1, 9, 0, 0, 0, 0, time.UTC)}
	demoActor   = services.Actor{UserID: "u1", Email: "demo@example.com", SessionID: "sess-1"}
)

// env is one UI over generated mocks of its dependencies, with a mock
// session in place of auth.Middleware.
type env struct {
	t     *testing.T
	auth  *mocks.MockAuth
	users *mocks.MockUsers
	h     http.Handler

	mu      sync.Mutex
	session *ui.Session // nil: anonymous
}

func newEnv(t *testing.T, f config.Features) *env {
	t.Helper()
	ctrl := gomock.NewController(t)
	e := &env{t: t, auth: mocks.NewMockAuth(ctrl), users: mocks.NewMockUsers(ctrl)}
	e.h = ui.New(ui.Deps{
		Auth:         e.auth,
		Users:        e.users,
		Features:     f,
		Logger:       slog.New(slog.NewTextHandler(io.Discard, nil)),
		LiveInterval: time.Hour, // the stream test drives events, not ticks
		UserFrom:     e.userFrom,
	}).Handler()
	return e
}

func (e *env) userFrom(context.Context) (ui.User, ui.Session, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.session == nil {
		return ui.User{}, ui.Session{}, false
	}
	return demoUser, *e.session, true
}

// signedIn makes demoUser the caller on session id.
func (e *env) signedIn(sessionID string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	s := demoSession
	s.ID = sessionID
	e.session = &s
}

type reqOpt func(*http.Request)

// datastar marks the request as a Datastar action (SSE response).
func datastar(r *http.Request) { r.Header.Set("Datastar-Request", "true") }

func lang(l string) reqOpt { return func(r *http.Request) { r.Header.Set("Accept-Language", l) } }

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

// en and ja translate a key for assertions; HTML-escaped like the output.
func en(key string, pairs ...string) string {
	return htmlText(i18n.Translator{Locale: i18n.En}.Tf(key, pairs...))
}

func ja(key string, pairs ...string) string {
	return htmlText(i18n.Translator{Locale: i18n.Ja}.Tf(key, pairs...))
}

// htmlText escapes like templ does.
func htmlText(s string) string { return html.EscapeString(s) }

// assertSSE checks a Datastar response carries element patches containing want.
func assertSSE(t *testing.T, rec *httptest.ResponseRecorder, want ...string) {
	t.Helper()
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("content-type %q, want SSE; status %d; body:\n%s", ct, rec.Code, rec.Body)
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
		t.Fatalf("got %d %q, want 303 %q", rec.Code, rec.Header().Get("Location"), to)
	}
}

// authErr returns the auth error the UI shows with key (from ui.AuthErrors,
// so tests never import packages/auth).
func authErr(t *testing.T, key string) error {
	t.Helper()
	for _, e := range ui.AuthErrors {
		if e.Key == key {
			return e.Err
		}
	}
	t.Fatalf("no auth error with key %s", key)
	return nil
}

// setCookie stands in for auth's cookie writer.
func setCookie(w http.ResponseWriter, s ui.Session) {
	http.SetCookie(w, &http.Cookie{Name: "session", Value: s.Token, Path: "/"})
}

func hasCookie(rec *httptest.ResponseRecorder, value string) bool {
	for _, c := range rec.Result().Cookies() {
		if c.Name == "session" && c.Value == value {
			return true
		}
	}
	return false
}
