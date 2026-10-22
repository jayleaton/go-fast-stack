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

// noAuth stands in for the admin *auth.Auth: nobody is signed in, so mounts
// and middleware order can be tested without a database (auth_test.go covers
// the real package).
type noAuth struct{}

func (noAuth) Middleware(next http.Handler) http.Handler { return next }

func nobody(context.Context) (api.Caller, bool) { return api.Caller{}, false }

// stubUI serves /users, panics on /boom and 404s the rest, like the real UI.
var stubUI = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "/boom":
		panic("boom: /srv/admin/secret.go:12")
	case config.AppRoutes.Users:
		_, _ = io.WriteString(w, "ui")
	default:
		http.NotFound(w, r)
	}
})

// newHandler is cmd/admin's wiring: no JSON API on the network.
func newHandler(dbs map[string]Pinger, ui http.Handler) http.Handler {
	return New(Deps{Log: slog.New(slog.NewTextHandler(io.Discard, nil)), DBs: dbs, Auth: noAuth{}, UI: ui})
}

// withAPI is a deployment that opts in to the JSON API.
func withAPI() http.Handler {
	return New(Deps{
		Log:  slog.New(slog.NewTextHandler(io.Discard, nil)),
		DBs:  map[string]Pinger{"app": pinger{}},
		Auth: noAuth{},
		// Nobody is signed in, so no request reaches a service.
		API: []Router{api.NewUsers(nil, nobody), api.NewAdmins(nil, nobody), api.NewAudit(nil, nobody), api.NewAnalytics(nil, nobody)},
		UI:  stubUI,
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
	browser := httptest.NewRequest(http.MethodGet, config.APIRoutes.Healthz, nil)
	browser.Header.Set("Sec-Fetch-Mode", "navigate")
	if rec := serve(newHandler(map[string]Pinger{"app": pinger{}}, nil), browser); rec.Code != http.StatusNotFound {
		t.Fatalf("healthz from a browser: %d", rec.Code)
	}
}

func TestMountsAndMiddleware(t *testing.T) {
	h := newHandler(map[string]Pinger{"app": pinger{}}, stubUI)
	cases := []struct {
		name    string
		req     *http.Request
		status  int
		snippet string
	}{
		{"ui mounted at /", httptest.NewRequest(http.MethodGet, config.AppRoutes.Users, nil), 200, "ui"},
		{"no JSON API by default", httptest.NewRequest(http.MethodGet, config.APIRoutes.Users, nil), 404, "404 page not found"},
		{"ui panics become a plain 500", httptest.NewRequest(http.MethodGet, "/boom", nil), 500, "Internal Server Error"},
	}
	for _, c := range cases {
		rec := serve(h, c.req)
		if rec.Code != c.status || !strings.Contains(rec.Body.String(), c.snippet) || strings.Contains(rec.Body.String(), "secret.go") {
			t.Errorf("%s: %d %s", c.name, rec.Code, rec.Body)
		}
		if rec.Header().Get("X-Frame-Options") != "DENY" || rec.Header().Get("Cache-Control") != "no-store" || rec.Header().Get("X-Robots-Tag") == "" {
			t.Errorf("%s: headers %v", c.name, rec.Header())
		}
	}
}

// The UI runs sign-in, set-password and 2FA through packages/auth
// in-process, so no admin auth endpoint is on the network: none can bypass
// the audit log or be probed.
func TestNoAuthEndpointsAreMounted(t *testing.T) {
	for _, h := range []http.Handler{newHandler(map[string]Pinger{"app": pinger{}}, stubUI), withAPI()} {
		for _, p := range []string{
			"POST sign-in/email", "POST sign-out", "GET get-session", "GET reset-password/tok?callbackURL=%2Fset-password",
			"POST set-password", "POST two-factor/enable", "POST two-factor/verify-totp", "POST two-factor/verify-backup-code",
			"POST sign-up/email", "POST change-password", "POST change-email", "POST revoke-other-sessions", "POST request-password-reset",
		} {
			method, path, _ := strings.Cut(p, " ")
			if rec := serve(h, httptest.NewRequest(method, "/api/auth/"+path, strings.NewReader(`{}`))); rec.Code != http.StatusNotFound {
				t.Errorf("%s /api/auth/%s: %d, want 404", method, path, rec.Code)
			}
		}
	}
}

// When a deployment mounts the JSON API it is for scripts only: browsers
// get the bare 404 of an unknown path, and so does a wrong method.
func TestOptInAPIIsHiddenFromBrowsers(t *testing.T) {
	h := withAPI()
	for _, path := range []string{config.APIRoutes.Users, config.APIRoutes.Admins, config.APIRoutes.Audit, config.APIRoutes.Analytics} {
		if rec := serve(h, httptest.NewRequest(http.MethodGet, path, nil)); rec.Code != http.StatusUnauthorized || !strings.Contains(rec.Body.String(), `"Unauthorized"`) {
			t.Errorf("script GET %s: %d %s", path, rec.Code, rec.Body)
		}
		for name, set := range map[string]func(*http.Request){
			"fetch":      func(r *http.Request) { r.Header.Set("Sec-Fetch-Site", "same-origin") },
			"origin":     func(r *http.Request) { r.Header.Set("Origin", "http://dev-box:3001") },
			"typed URL":  func(r *http.Request) { r.Header.Set("Accept", "text/html") },
			"via a link": func(r *http.Request) { r.Header.Set("Referer", "http://dev-box:3001/users") },
		} {
			req := httptest.NewRequest(http.MethodGet, path, nil)
			set(req)
			if rec := serve(h, req); rec.Code != http.StatusNotFound || rec.Body.String() != "404 page not found\n" {
				t.Errorf("browser %s GET %s: %d %s", name, path, rec.Code, rec.Body)
			}
		}
	}
	if rec := serve(h, httptest.NewRequest(http.MethodGet, "/api", nil)); rec.Code != http.StatusNotFound || rec.Header().Get("Location") != "" {
		t.Errorf("/api: %d %q", rec.Code, rec.Header().Get("Location"))
	}
	rec := serve(h, httptest.NewRequest(http.MethodPatch, config.APIRoutes.Users, nil))
	if rec.Code != http.StatusNotFound || rec.Header().Get("Allow") != "" {
		t.Errorf("wrong method: %d Allow %q", rec.Code, rec.Header().Get("Allow"))
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
