package server

import (
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/jayleaton/go-fast-stack/apps/admin/internal/api"
	"github.com/jayleaton/go-fast-stack/apps/admin/internal/config"
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
			"ms", float64(time.Since(start).Microseconds())/float64(time.Millisecond/time.Microsecond))
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

// apiPrefix is the admin JSON API subtree (config.APIRoutes), when mounted.
const apiPrefix = "/api/"

// apiOnly hides next from browsers: a request carrying Fetch Metadata
// (Sec-Fetch-*), Origin or Referer, or asking for HTML, gets the bare 404 of
// an unknown path. Browsers send Fetch Metadata on every https request and
// Origin on every POST and cross-origin fetch; over plain http (a dev box on
// the LAN) a typed-in URL still asks for text/html. Scripts and probes send
// none of these. No CORS headers are ever set, so preflights fail too.
func apiOnly(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := r.Header
		if h.Get("Sec-Fetch-Site") != "" || h.Get("Sec-Fetch-Mode") != "" || h.Get("Sec-Fetch-Dest") != "" ||
			h.Get("Origin") != "" || h.Get("Referer") != "" || strings.Contains(h.Get("Accept"), "text/html") {
			http.NotFound(w, r)
			return
		}
		next.ServeHTTP(w, r)
	})
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
