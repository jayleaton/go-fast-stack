package server

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jayleaton/ai-patterns-go/apps/web/internal/api"
	"github.com/jayleaton/ai-patterns-go/apps/web/internal/config"
)

type pinger struct{ err error }

type secretsHealth struct{ err error }

func (s secretsHealth) Healthy() error { return s.err }

func (p pinger) Ping(context.Context) error { return p.err }

// noAuth stands in for *auth.Auth: nobody is signed in and the auth
// endpoints answer 501, so mount and middleware order can be tested without
// a database (auth_test.go covers the real package).
type noAuth struct{}

func (noAuth) Middleware(next http.Handler) http.Handler { return next }
func (noAuth) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		api.WriteError(w, http.StatusNotImplemented, "not implemented", nil)
	})
}

func newHandler(db Pinger, ui http.Handler) http.Handler {
	return newHandlerWith(db, nil, ui)
}

func newHandlerWith(db Pinger, sec SecretsHealth, ui http.Handler) http.Handler {
	return New(Deps{
		Secrets: sec,
		Config:  config.Config{TrustedOrigins: []string{"http://dev-box:3000"}},
		Log:     slog.New(slog.NewTextHandler(io.Discard, nil)),
		DB:      db,
		Auth:    noAuth{},
		Users:   api.NewUsers(nil, CallerFrom), // nobody is signed in, so no request reaches the service
		UI:      ui,
	})
}

func serve(h http.Handler, req *http.Request) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestHealthz(t *testing.T) {
	ok := serve(newHandler(pinger{}, nil), httptest.NewRequest(http.MethodGet, "/healthz", nil))
	down := serve(newHandler(pinger{errors.New("down")}, nil), httptest.NewRequest(http.MethodGet, "/healthz", nil))
	fresh := serve(newHandlerWith(pinger{}, secretsHealth{}, nil), httptest.NewRequest(http.MethodGet, "/healthz", nil))
	stale := serve(newHandlerWith(pinger{}, secretsHealth{errors.New("stale")}, nil), httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if ok.Code != 200 || down.Code != 503 || fresh.Code != 200 || stale.Code != 503 {
		t.Fatalf("healthz ok=%d down=%d fresh=%d stale=%d", ok.Code, down.Code, fresh.Code, stale.Code)
	}
	if !strings.Contains(stale.Body.String(), "secrets stale") {
		t.Fatalf("stale body = %s", stale.Body.String())
	}
}

func TestMountsAndMiddleware(t *testing.T) {
	ui := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/boom" {
			panic("boom: /home/dev/secret.go:12")
		}
		_, _ = io.WriteString(w, "ui")
	})
	h := newHandler(pinger{}, ui)

	cases := []struct {
		name    string
		req     *http.Request
		status  int
		snippet string
	}{
		{"ui mounted at /", httptest.NewRequest(http.MethodGet, "/dashboard", nil), 200, "ui"},
		{"auth mounted", httptest.NewRequest(http.MethodPost, "/api/auth/sign-in/email", nil), 501, `"error"`},
		{"api needs a session", httptest.NewRequest(http.MethodGet, "/api/core/v1/users/me", nil), 401, `"Unauthorized"`},
		{"ui panics become a plain 500", httptest.NewRequest(http.MethodGet, "/boom", nil), 500, "Internal Server Error"},
		{"unknown api paths are a bare 404", httptest.NewRequest(http.MethodGet, "/api/core/v2/nope", nil), 404, "404 page not found"},
		{"wrong methods are a 404, not a 405", httptest.NewRequest(http.MethodPut, "/api/core/v1/users/me/sessions/others", nil), 404, "404 page not found"},
	}
	for _, c := range cases {
		rec := serve(h, c.req)
		if rec.Code != c.status || !strings.Contains(rec.Body.String(), c.snippet) {
			t.Errorf("%s: %d %s", c.name, rec.Code, rec.Body)
		}
		if strings.Contains(rec.Body.String(), "secret.go") || rec.Header().Get("Allow") != "" {
			t.Errorf("%s leaks: Allow %q body %s", c.name, rec.Header().Get("Allow"), rec.Body)
		}
	}
}

// The JSON API and /healthz are invisible to browsers: any request a browser
// could send there (fetch, form post, preflight, typed-in URL) gets the bare
// 404 of an unknown path, with no CORS headers.
func TestAPIIsHiddenFromBrowsers(t *testing.T) {
	h := newHandler(pinger{}, http.NotFoundHandler())
	browser := map[string]func(*http.Request){
		"same-origin fetch": func(r *http.Request) {
			r.Header.Set("Sec-Fetch-Site", "same-origin")
			r.Header.Set("Sec-Fetch-Mode", "cors")
		},
		"cross-site post": func(r *http.Request) { r.Header.Set("Origin", "https://evil.example") },
		"trusted origin":  func(r *http.Request) { r.Header.Set("Origin", "http://dev-box:3000") },
		"navigation": func(r *http.Request) {
			r.Header.Set("Sec-Fetch-Mode", "navigate")
			r.Header.Set("Sec-Fetch-Dest", "document")
		},
		"plain-http typed URL": func(r *http.Request) {
			r.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8")
		},
		"followed link": func(r *http.Request) { r.Header.Set("Referer", "http://dev-box:3000/dashboard") },
	}
	targets := []struct{ method, path string }{
		{http.MethodGet, "/api/core/v1/users/me"},
		{http.MethodPatch, "/api/core/v1/users/me"},
		{http.MethodOptions, "/api/core/v1/users/me"},
		{http.MethodPost, "/api/auth/sign-in/email"},
		{http.MethodGet, "/api/auth/get-session"},
		{http.MethodGet, "/healthz"},
	}
	unknown := serve(h, httptest.NewRequest(http.MethodGet, "/api/nope", nil))
	for name, set := range browser {
		for _, tg := range targets {
			req := httptest.NewRequest(tg.method, tg.path, strings.NewReader(`{}`))
			req.Header.Set("Access-Control-Request-Method", "PATCH")
			set(req)
			rec := serve(h, req)
			if rec.Code != http.StatusNotFound || rec.Body.String() != unknown.Body.String() {
				t.Errorf("%s %s %s: %d %s", name, tg.method, tg.path, rec.Code, rec.Body)
			}
			for k := range rec.Header() {
				if strings.HasPrefix(k, "Access-Control-") {
					t.Errorf("%s %s %s: CORS header %s", name, tg.method, tg.path, k)
				}
			}
		}
	}

	// Native apps and servers send no browser headers and reach the API.
	if rec := serve(h, httptest.NewRequest(http.MethodGet, "/healthz", nil)); rec.Code != http.StatusOK {
		t.Errorf("healthz from a probe: %d", rec.Code)
	}
	// A browser opening an emailed link whose callback is a deep link
	// (mobile) reaches auth's redirecting link endpoints, and nothing else.
	link := func(target string) *http.Request {
		req := httptest.NewRequest(http.MethodGet, target, nil)
		req.Header.Set("Sec-Fetch-Mode", "navigate")
		req.Header.Set("Sec-Fetch-Site", "none")
		req.Header.Set("Accept", "text/html")
		return req
	}
	for target, want := range map[string]int{
		"/api/auth/verify-email?token=t&callbackURL=myapp%3A%2F%2Fdone":     501,
		"/api/auth/reset-password/tok?callbackURL=myapp%3A%2F%2Freset":      501,
		"/api/auth/verify-email?token=t":                                    404,
		"/api/auth/get-session?callbackURL=myapp%3A%2F%2F":                  404,
		"/api/core/v1/users/me?callbackURL=myapp%3A%2F%2F":                  404,
		"/api/auth/reset-password-evil/tok?callbackURL=myapp%3A%2F%2Freset": 404,
	} {
		if rec := serve(h, link(target)); rec.Code != want {
			t.Errorf("link %s: %d, want %d", target, rec.Code, want)
		}
	}
}

// Emailed reset links carry a credential in the path; logs keep the route only.
func TestRequestLogsDropTokens(t *testing.T) {
	var logs strings.Builder
	h := New(Deps{
		Log: slog.New(slog.NewTextHandler(&logs, nil)), DB: pinger{}, Auth: noAuth{},
		Users: api.NewUsers(nil, CallerFrom),
	})
	serve(h, httptest.NewRequest(http.MethodGet, "/api/auth/reset-password/s3cret-token", nil))
	if strings.Contains(logs.String(), "s3cret-token") || !strings.Contains(logs.String(), "/api/auth/reset-password/{token}") {
		t.Fatalf("log = %s", logs.String())
	}
}
