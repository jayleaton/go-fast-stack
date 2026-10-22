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
	"github.com/jayleaton/ai-patterns-go/apps/web/internal/server/devauth"
	"github.com/jayleaton/ai-patterns-go/apps/web/internal/services"
	"github.com/jayleaton/ai-patterns-go/apps/web/internal/services/servicetest"
)

type pinger struct{ err error }

func (p pinger) Ping(context.Context) error { return p.err }

func newHandler(db Pinger, ui http.Handler) http.Handler {
	users := services.NewUserService(servicetest.NewUsers())
	return New(Deps{
		Config: config.Config{TrustedOrigins: []string{"http://dev-box:3000"}},
		Log:    slog.New(slog.NewTextHandler(io.Discard, nil)),
		DB:     db,
		Auth:   devauth.NoAuth{},
		Users:  api.NewUsers(users, devauth.CallerFrom),
		UI:     ui,
	})
}

func serve(h http.Handler, req *http.Request) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestHealthz(t *testing.T) {
	ok := serve(newHandler(pinger{}, nil), httptest.NewRequest("GET", "/healthz", nil))
	down := serve(newHandler(pinger{errors.New("down")}, nil), httptest.NewRequest("GET", "/healthz", nil))
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
		{"ui mounted at /", httptest.NewRequest("GET", "/dashboard", nil), 200, "ui"},
		{"auth mounted", httptest.NewRequest("POST", "/api/auth/sign-in/email", nil), 501, `"error"`},
		{"api needs a session", httptest.NewRequest("GET", "/api/core/v1/users", nil), 401, `"Unauthorized"`},
		{"panics become 500 envelopes", httptest.NewRequest("GET", "/boom", nil), 500, `"Internal server error"`},
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
	req := httptest.NewRequest("PATCH", "/api/core/v1/users", strings.NewReader(`{}`))
	req.Header.Set("Origin", origin)
	req.Header.Set("Sec-Fetch-Site", "cross-site")
	return req
}
