package server

import (
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/jayleaton/ai-patterns-go/apps/web/internal/api"
	"github.com/jayleaton/ai-patterns-go/apps/web/internal/config"
)

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (s *statusRecorder) WriteHeader(code int) {
	s.status = code
	s.ResponseWriter.WriteHeader(code)
}

// Unwrap lets http.ResponseController reach Flush for SSE (Datastar).
func (s *statusRecorder) Unwrap() http.ResponseWriter { return s.ResponseWriter }

func logRequests(log *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)
		level := slog.LevelInfo
		if r.URL.Path == config.APIRoutes.Healthz {
			level = slog.LevelDebug
		}
		log.Log(r.Context(), level, "request",
			"method", r.Method, "path", logPath(r.URL.Path), "status", rec.status,
			"ms", float64(time.Since(start).Microseconds())/1000)
	})
}

// recoverPanics answers a panic with a generic 500: the JSON envelope on the
// API, plain text elsewhere. The panic value is logged, never sent.
func recoverPanics(log *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if v := recover(); v != nil {
				if err, ok := v.(error); ok && errors.Is(err, http.ErrAbortHandler) {
					panic(v)
				}
				log.ErrorContext(r.Context(), "panic", "method", r.Method, "path", logPath(r.URL.Path), "panic", v)
				if strings.HasPrefix(r.URL.Path, apiPrefix) {
					api.WriteError(w, http.StatusInternalServerError, "Internal server error", nil)
					return
				}
				http.Error(w, http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)
			}
		}()
		next.ServeHTTP(w, r)
	})
}

// apiPrefix is the JSON API subtree (config.APIRoutes: auth and core).
const apiPrefix = "/api/"

// apiOnly hides next from browsers: a request carrying Fetch Metadata
// (Sec-Fetch-*), Origin or Referer, or asking for HTML, gets the bare 404 of
// an unknown path. Browsers send Fetch Metadata on every https request and
// Origin on every POST and cross-origin fetch; over plain http (a dev box on
// the LAN) a typed-in URL still asks for text/html. Native apps and
// server-to-server clients send none of these. No CORS headers are ever set, so preflights fail too. The one
// exception is a top-level navigation to an emailed auth link whose callback
// is a deep link (mobile): those endpoints only redirect.
func apiOnly(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if fromBrowser(r) && !linkNavigation(r) {
			http.NotFound(w, r)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func fromBrowser(r *http.Request) bool {
	h := r.Header
	return h.Get("Sec-Fetch-Site") != "" || h.Get("Sec-Fetch-Mode") != "" ||
		h.Get("Sec-Fetch-Dest") != "" || h.Get("Origin") != "" || h.Get("Referer") != "" ||
		strings.Contains(h.Get("Accept"), "text/html")
}

// linkPaths are auth's GET endpoints behind emailed links for absolute or
// deep-link callbacks; links for the web UI point at its pages instead.
var linkPaths = []string{config.APIRoutes.Auth + "verify-email", config.APIRoutes.Auth + "reset-password/"}

func linkNavigation(r *http.Request) bool {
	if r.Method != http.MethodGet || r.Header.Get("Sec-Fetch-Mode") != "navigate" ||
		r.Header.Get("Origin") != "" || r.URL.Query().Get("callbackURL") == "" {
		return false
	}
	for _, p := range linkPaths {
		if r.URL.Path == p || (strings.HasSuffix(p, "/") && strings.HasPrefix(r.URL.Path, p)) {
			return true
		}
	}
	return false
}

// hideMethods turns a 405 (with its Allow list) into the bare 404 of an
// unknown path, so the API does not reveal which methods a path has.
func hideMethods(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		next.ServeHTTP(&methodHider{ResponseWriter: w, r: r}, r)
	})
}

type methodHider struct {
	http.ResponseWriter
	r      *http.Request
	hidden bool
}

func (m *methodHider) WriteHeader(code int) {
	if code != http.StatusMethodNotAllowed {
		m.ResponseWriter.WriteHeader(code)
		return
	}
	m.hidden = true
	m.Header().Del("Allow")
	http.NotFound(m.ResponseWriter, m.r)
}

func (m *methodHider) Write(b []byte) (int, error) {
	if m.hidden {
		return len(b), nil
	}
	return m.ResponseWriter.Write(b)
}

func (m *methodHider) Unwrap() http.ResponseWriter { return m.ResponseWriter }

// tokenPaths carry a single-use credential in the path (emailed reset and
// set-password links). Logs keep the route, never the token: anyone reading
// them could otherwise use the link first.
var tokenPaths = []string{"/api/auth/reset-password/"}

func logPath(path string) string {
	for _, p := range tokenPaths {
		if strings.HasPrefix(path, p) && len(path) > len(p) {
			return p + "{token}"
		}
	}
	return path
}
