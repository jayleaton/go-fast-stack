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

	"github.com/jayleaton/ai-patterns-go/apps/admin/internal/api"
	"github.com/jayleaton/ai-patterns-go/apps/admin/internal/config"
)

type pinger struct{ err error }

func (p pinger) Ping(context.Context) error { return p.err }

// noAuth stands in for the admin *auth.Auth: nobody is signed in and the auth
// endpoints answer 501, so mounts and middleware order can be tested without
// a database (auth_test.go covers the real package).
type noAuth struct{}

func (noAuth) Middleware(next http.Handler) http.Handler { return next }
func (noAuth) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		api.WriteError(w, http.StatusNotImplemented, "not implemented", nil)
	})
}

func nobody(context.Context) (api.Caller, bool) { return api.Caller{}, false }

func newHandler(dbs map[string]Pinger, ui http.Handler) http.Handler {
	return New(Deps{
		Config: config.Config{TrustedOrigins: []string{"http://dev-box:3001"}},
		Log:    slog.New(slog.NewTextHandler(io.Discard, nil)),
		DBs:    dbs,
		Auth:   noAuth{},
		// Nobody is signed in, so no request reaches a service.
		API: []Router{api.NewUsers(nil, nobody), api.NewAdmins(nil, nobody), api.NewAudit(nil, nobody), api.NewAnalytics(nil, nobody)},
		UI:  ui,
	})
}

func serve(h http.Handler, req *http.Request) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestHealthzPingsEveryDatabase(t *testing.T) {
	get := func(dbs map[string]Pinger) *httptest.ResponseRecorder {
		return serve(newHandler(dbs, nil), httptest.NewRequest(http.MethodGet, config.APIRoutes.Healthz, nil))
	}
	if rec := get(map[string]Pinger{"app": pinger{}, "admin": pinger{}}); rec.Code != 200 {
		t.Fatalf("both up: %d %s", rec.Code, rec.Body)
	}
	rec := get(map[string]Pinger{"app": pinger{}, "admin": pinger{errors.New("down")}})
	if rec.Code != 503 || !strings.Contains(rec.Body.String(), "admin database unavailable") {
		t.Fatalf("admin down: %d %s", rec.Code, rec.Body)
	}
}

func TestMountsAndMiddleware(t *testing.T) {
	ui := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/boom" {
			panic("boom")
		}
		_, _ = io.WriteString(w, "ui")
	})
	h := newHandler(map[string]Pinger{"app": pinger{}}, ui)

	cases := []struct {
		name    string
		req     *http.Request
		status  int
		snippet string
	}{
		{"ui mounted at /", httptest.NewRequest(http.MethodGet, config.AppRoutes.Users, nil), 200, "ui"},
		{"auth sign-in mounted", httptest.NewRequest(http.MethodPost, "/api/auth/sign-in/email", nil), 501, `"error"`},
		{"auth set-password link mounted", httptest.NewRequest(http.MethodGet, "/api/auth/reset-password/tok", nil), 501, `"error"`},
		{"auth 2FA step mounted", httptest.NewRequest(http.MethodPost, "/api/auth/two-factor/verify-totp", nil), 501, `"error"`},
		{"users api needs a session", httptest.NewRequest(http.MethodGet, config.APIRoutes.Users, nil), 401, `"Unauthorized"`},
		{"admins api needs a session", httptest.NewRequest(http.MethodGet, config.APIRoutes.Admins, nil), 401, `"Unauthorized"`},
		{"audit api needs a session", httptest.NewRequest(http.MethodGet, config.APIRoutes.Audit, nil), 401, `"Unauthorized"`},
		{"analytics api needs a session", httptest.NewRequest(http.MethodGet, config.APIRoutes.Analytics, nil), 401, `"Unauthorized"`},
		{"panics become 500 envelopes", httptest.NewRequest(http.MethodGet, "/boom", nil), 500, `"Internal server error"`},
		{"cross-site writes are rejected", crossSite("https://evil.example"), 403, `"Cross-origin request rejected"`},
		{"trusted origins pass", crossSite("http://dev-box:3001"), 401, `"Unauthorized"`},
	}
	for _, c := range cases {
		rec := serve(h, c.req)
		if rec.Code != c.status || !strings.Contains(rec.Body.String(), c.snippet) {
			t.Errorf("%s: %d %s", c.name, rec.Code, rec.Body)
		}
		if rec.Header().Get("X-Frame-Options") != "DENY" || rec.Header().Get("Cache-Control") != "no-store" {
			t.Errorf("%s: headers %v", c.name, rec.Header())
		}
	}
}

func crossSite(origin string) *http.Request {
	req := httptest.NewRequest(http.MethodPost, config.UserPath("u1", "/ban"), strings.NewReader(`{}`))
	req.Header.Set("Origin", origin)
	req.Header.Set("Sec-Fetch-Site", "cross-site")
	return req
}

// Self-service account changes would bypass the audit log, so the admin
// auth endpoints are an allowlist.
func TestUnauditedAuthEndpointsAreNotMounted(t *testing.T) {
	h := newHandler(map[string]Pinger{"app": pinger{}}, nil)
	for _, p := range []string{"sign-up/email", "change-password", "change-email", "revoke-session", "revoke-other-sessions", "two-factor/disable", "two-factor/generate-backup-codes", "request-password-reset"} {
		req := httptest.NewRequest(http.MethodPost, "/api/auth/"+p, strings.NewReader(`{}`))
		if rec := serve(h, req); rec.Code != http.StatusNotFound {
			t.Errorf("POST /api/auth/%s: %d, want 404", p, rec.Code)
		}
	}
}

// Emailed set-password links carry a credential in the path; logs keep the
// route only.
func TestRequestLogsDropTokens(t *testing.T) {
	var logs strings.Builder
	h := New(Deps{Log: slog.New(slog.NewTextHandler(&logs, nil)), DBs: map[string]Pinger{}, Auth: noAuth{}})
	serve(h, httptest.NewRequest(http.MethodGet, "/api/auth/reset-password/s3cret-token?callbackURL=%2Fset-password", nil))
	if strings.Contains(logs.String(), "s3cret-token") || !strings.Contains(logs.String(), "/api/auth/reset-password/{token}") {
		t.Fatalf("log = %s", logs.String())
	}
}
