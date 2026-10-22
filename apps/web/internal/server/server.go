// Package server composes the HTTP handler: global middleware, /healthz, the
// auth mount, the JSON API and the UI. It is the only place that wires
// packages together.
package server

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	"github.com/jayleaton/ai-patterns-go/apps/web/internal/api"
	"github.com/jayleaton/ai-patterns-go/apps/web/internal/config"
)

// Auth is the part of *auth.Auth the server mounts.
type Auth interface {
	Middleware(next http.Handler) http.Handler // loads the session into the request context
	Handler() http.Handler                     // JSON endpoints under config.APIRoutes.Auth
}

// Pinger reports database health (*pgxpool.Pool).
type Pinger interface {
	Ping(ctx context.Context) error
}

// SecretsHealth reports whether live secrets are loaded and fresh
// (config.Secrets).
type SecretsHealth interface {
	Healthy() error
}

// Deps are the wired dependencies.
type Deps struct {
	Config  config.Config
	Log     *slog.Logger
	DB      Pinger
	Secrets SecretsHealth // nil: /healthz checks the database only
	Auth    Auth
	Users   *api.Users
	UI      http.Handler // apps/web/ui routes, mounted at "/"; nil until the UI lands
}

// New returns the root handler. The JSON API (auth and core) and /healthz
// are for the mobile app and server-to-server clients only: browser requests
// get the same bare 404 as an unknown path (apiOnly), so the web UI's pages
// and network log never touch them. The UI calls services in-process and
// guards its own actions against cross-origin posts.
func New(d Deps) http.Handler {
	apiMux := http.NewServeMux()
	apiMux.Handle(config.APIRoutes.Auth, d.Auth.Handler())
	d.Users.Routes(apiMux)

	mux := http.NewServeMux()
	mux.Handle("GET "+config.APIRoutes.Healthz, apiOnly(healthz(d.DB, d.Secrets)))
	mux.Handle(apiPrefix, apiOnly(hideMethods(apiMux)))
	if d.UI != nil {
		mux.Handle("/", d.UI)
	}
	return logRequests(d.Log, recoverPanics(d.Log, d.Auth.Middleware(mux)))
}

// healthz is a readiness check: 503 when the database is down or live
// secrets are past their max staleness (the provider has been unreachable
// too long). Don't use it as a liveness probe that restarts the process: a
// fresh process cannot load its secrets during a provider outage either.
func healthz(db Pinger, sec SecretsHealth) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()
		if err := db.Ping(ctx); err != nil {
			api.WriteError(w, http.StatusServiceUnavailable, "database unavailable", nil)
			return
		}
		if sec != nil && sec.Healthy() != nil {
			api.WriteError(w, http.StatusServiceUnavailable, "secrets stale", nil)
			return
		}
		api.WriteData(w, http.StatusOK, map[string]string{"status": "ok"})
	}
}
