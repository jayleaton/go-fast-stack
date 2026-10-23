// Package server composes the admin app's HTTP handler: global middleware,
// /healthz over both databases, the UI and (only when a deployment opts in)
// the JSON API with the admin auth endpoints that issue its tokens. With
// cmd/admin it is the only place in apps/admin that wires packages together,
// and the only one that imports packages/auth (the adapters in this package).
package server

import (
	"context"
	"log/slog"
	"maps"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/jayleaton/go-fast-stack/apps/admin/internal/api"
	"github.com/jayleaton/go-fast-stack/apps/admin/internal/config"
	"github.com/jayleaton/go-fast-stack/packages/auth"
)

// Auth is the part of the admin *auth.Auth the server mounts.
type Auth interface {
	Middleware(next http.Handler) http.Handler // loads the admin session into the request context
	Handler() http.Handler                     // JSON endpoints, mounted (authRoutes) only with Deps.API
}

// Pinger reports database health (*pgxpool.Pool).
type Pinger interface {
	Ping(ctx context.Context) error
}

// Router is one group of JSON endpoints (*api.Users, *api.Admins, ...).
type Router interface {
	Routes(mux *http.ServeMux)
}

// Deps are the wired dependencies.
type Deps struct {
	Config config.Config
	Log    *slog.Logger
	DBs    map[string]Pinger // by name, every one must answer /healthz
	// Secrets reports whether live secrets are loaded and fresh
	// (config.Secrets); nil: /healthz checks the databases only.
	Secrets SecretsHealth
	Auth    Auth // audited through its auth.Config.Events (NewAuditEvents)
	// API is the admin JSON API (/api/admin/v1). cmd/admin mounts none: the
	// dashboard calls the services in-process, so nothing about the API is
	// on the network. A deployment that wants scripts or agents to call it
	// passes the routers here; they are mounted with the admin auth
	// endpoints that issue their bearer tokens (authRoutes), and both are
	// hidden from browsers like /healthz.
	API []Router
	UI  http.Handler // apps/admin/ui routes, mounted at "/"
}

// authRoutes are the admin auth endpoints mounted with the JSON API: sign-in
// (with the 2FA step and enrolment), sign-out, the session, and setting the
// password from an invite token. Self-service account changes (password,
// email, disabling 2FA, backup codes, other sessions) are not exposed: they
// would bypass the audit log. Sign-up is off in the admin auth.Config.
var authRoutes = []string{
	"POST " + auth.BasePath + "/sign-in/email",
	"POST " + auth.BasePath + "/sign-out",
	"GET " + auth.BasePath + "/get-session",
	"POST " + auth.BasePath + "/set-password",
	"POST " + auth.BasePath + "/two-factor/enable",
	"POST " + auth.BasePath + "/two-factor/verify-totp",
	"POST " + auth.BasePath + "/two-factor/verify-backup-code",
}

// New returns the root handler. Without Deps.API nothing under /api/ is
// mounted: the UI signs in, sets passwords (invite links land on its
// /set-password page) and runs 2FA through packages/auth in-process.
func New(deps Deps) http.Handler {
	mux := http.NewServeMux()
	mux.Handle("GET "+config.APIRoutes.Healthz, apiOnly(healthz(deps.DBs, deps.Secrets)))
	if len(deps.API) > 0 {
		apiMux := http.NewServeMux()
		authHandler := deps.Auth.Handler()
		for _, authRoute := range authRoutes {
			apiMux.Handle(authRoute, authHandler)
		}
		for _, router := range deps.API {
			router.Routes(apiMux)
		}
		mux.Handle(apiPrefix, apiOnly(hideMethods(apiMux)))
		// Without this, ServeMux would answer /api with a 307 to /api/.
		mux.Handle(strings.TrimSuffix(apiPrefix, "/"), http.NotFoundHandler())
	}
	if deps.UI != nil {
		mux.Handle("/", deps.UI)
	}
	return securityHeaders(logRequests(deps.Log, recoverPanics(deps.Log, deps.Auth.Middleware(mux))))
}

// securityHeaders: the dashboard is never indexed, framed or cached,
// including /healthz and bare 404s; the UI adds its CSP and the rest of the
// browser headers, and the static handler overrides Cache-Control for its
// assets.
func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		header := writer.Header()
		header.Set("Cache-Control", "no-store")
		header.Set("X-Frame-Options", "DENY")
		header.Set("X-Content-Type-Options", "nosniff")
		header.Set("X-Robots-Tag", "noindex, nofollow")
		header.Set("Referrer-Policy", "strict-origin")
		next.ServeHTTP(writer, request)
	})
}

// SecretsHealth reports whether live secrets are loaded and fresh.
type SecretsHealth interface {
	Healthy() error
}

// healthzTimeout bounds the database pings behind /healthz, so a hung
// database answers 503 instead of hanging the probe.
const healthzTimeout = 2 * time.Second

// healthz is a readiness check: 503 when a database is down or live secrets
// are past their max staleness (see the web app's healthz).
func healthz(dbs map[string]Pinger, sec SecretsHealth) http.HandlerFunc {
	names := slices.Sorted(maps.Keys(dbs))
	return func(writer http.ResponseWriter, request *http.Request) {
		ctx, cancel := context.WithTimeout(request.Context(), healthzTimeout)
		defer cancel()
		for _, name := range names {
			if err := dbs[name].Ping(ctx); err != nil {
				api.WriteError(writer, http.StatusServiceUnavailable, name+" database unavailable", nil)
				return
			}
		}
		if sec != nil && sec.Healthy() != nil {
			api.WriteError(writer, http.StatusServiceUnavailable, "secrets stale", nil)
			return
		}
		api.WriteData(writer, http.StatusOK, map[string]string{"status": "ok"})
	}
}
