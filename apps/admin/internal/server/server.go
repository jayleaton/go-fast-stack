// Package server composes the admin app's HTTP handler: global middleware,
// /healthz over both databases, the admin auth mount (sign-up is off in its
// auth.Config: admins are invited), the
// JSON API and the UI. With cmd/admin it is the only place in apps/admin that
// wires packages together, and the only one that imports packages/auth (the
// adapters in this package).
package server

import (
	"context"
	"log/slog"
	"maps"
	"net/http"
	"slices"
	"time"

	"github.com/jayleaton/ai-patterns-go/apps/admin/internal/api"
	"github.com/jayleaton/ai-patterns-go/apps/admin/internal/config"
)

// Auth is the part of the admin *auth.Auth the server mounts.
type Auth interface {
	Middleware(next http.Handler) http.Handler // loads the admin session into the request context
	Handler() http.Handler                     // JSON endpoints under config.APIRoutes.Auth
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
	Auth   Auth
	API    []Router
	UI     http.Handler // apps/admin/ui routes, mounted at "/"
}

// New returns the root handler.
func New(d Deps) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET "+config.APIRoutes.Healthz, healthz(d.DBs))
	mux.Handle(config.APIRoutes.Auth, d.Auth.Handler())
	for _, r := range d.API {
		r.Routes(mux)
	}
	if d.UI != nil {
		mux.Handle("/", d.UI)
	}

	// Browsers send Sec-Fetch-Site/Origin; bearer-token clients (agents,
	// scripts) send neither and pass through.
	csrf := http.NewCrossOriginProtection()
	csrf.SetDenyHandler(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		api.WriteError(w, http.StatusForbidden, "Cross-origin request rejected", nil)
	}))
	for _, o := range d.Config.TrustedOrigins {
		if err := csrf.AddTrustedOrigin(o); err != nil {
			d.Log.Warn("trusted origin ignored by CSRF protection", "origin", o, "err", err)
		}
	}

	var h http.Handler = mux
	h = d.Auth.Middleware(h)
	h = csrf.Handler(h)
	return securityHeaders(logRequests(d.Log, recoverPanics(d.Log, h)))
}

// securityHeaders: the dashboard is never framed or indexed.
func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Frame-Options", "DENY")
		h.Set("X-Robots-Tag", "noindex, nofollow")
		h.Set("Referrer-Policy", "same-origin")
		next.ServeHTTP(w, r)
	})
}

func healthz(dbs map[string]Pinger) http.HandlerFunc {
	names := slices.Sorted(maps.Keys(dbs))
	return func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()
		for _, name := range names {
			if err := dbs[name].Ping(ctx); err != nil {
				api.WriteError(w, http.StatusServiceUnavailable, name+" database unavailable", nil)
				return
			}
		}
		api.WriteData(w, http.StatusOK, map[string]string{"status": "ok"})
	}
}
