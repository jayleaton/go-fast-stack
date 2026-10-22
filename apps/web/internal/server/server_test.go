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
	return New(Deps{
		Config: config.Config{TrustedOrigins: []string{"http://dev-box:3000"}},
		Log:    slog.New(slog.NewTextHandler(io.Discard, nil)),
		DB:     db,
		Auth:   noAuth{},
		Users:  api.NewUsers(nil, CallerFrom), // nobody is signed in, so no request reaches the service
		UI:     ui,
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
	if ok.Code != 200 || down.Code != 503 {
		t.Fatalf("healthz ok=%d down=%d", ok.Code, down.Code)
	}
}

func TestMountsAndMiddleware(t *testing.T) {
	ui := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/boom" {
			panic("boom")
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
		{"panics become 500 envelopes", httptest.NewRequest(http.MethodGet, "/boom", nil), 500, `"Internal server error"`},
		{"cross-site writes are rejected", crossSite("https://evil.example"), 403, `"Cross-origin request rejected"`},
		{"trusted origins pass", crossSite("http://dev-box:3000"), 401, `"Unauthorized"`},
	}
	for _, c := range cases {
		rec := serve(h, c.req)
		if rec.Code != c.status || !strings.Contains(rec.Body.String(), c.snippet) {
			t.Errorf("%s: %d %s", c.name, rec.Code, rec.Body)
		}
	}
}

func crossSite(origin string) *http.Request {
	req := httptest.NewRequest(http.MethodPatch, "/api/core/v1/users/me", strings.NewReader(`{}`))
	req.Header.Set("Origin", origin)
	req.Header.Set("Sec-Fetch-Site", "cross-site")
	return req
}
