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

func (recorder *statusRecorder) WriteHeader(code int) {
	recorder.status = code
	recorder.ResponseWriter.WriteHeader(code)
}

// Unwrap lets http.ResponseController reach Flush for SSE (Datastar).
func (recorder *statusRecorder) Unwrap() http.ResponseWriter { return recorder.ResponseWriter }

func logRequests(log *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: writer, status: http.StatusOK}
		next.ServeHTTP(rec, request)
		level := slog.LevelInfo
		if request.URL.Path == config.APIRoutes.Healthz {
			level = slog.LevelDebug
		}
		log.Log(request.Context(), level, "request",
			"method", request.Method, "path", logPath(request.URL.Path), "status", rec.status,
			"ms", float64(time.Since(start).Microseconds())/float64(time.Millisecond/time.Microsecond))
	})
}

// recoverPanics answers a panic with a generic 500: the JSON envelope on the
// API, plain text elsewhere. The panic value is logged, never sent.
func recoverPanics(log *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		defer func() {
			if value := recover(); value != nil {
				if err, ok := value.(error); ok && errors.Is(err, http.ErrAbortHandler) {
					panic(value)
				}
				log.ErrorContext(request.Context(), "panic", "method", request.Method, "path", logPath(request.URL.Path), "panic", value)
				if strings.HasPrefix(request.URL.Path, apiPrefix) {
					api.WriteError(writer, http.StatusInternalServerError, "Internal server error", nil)
					return
				}
				http.Error(writer, http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)
			}
		}()
		next.ServeHTTP(writer, request)
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
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		header := request.Header
		if header.Get("Sec-Fetch-Site") != "" || header.Get("Sec-Fetch-Mode") != "" || header.Get("Sec-Fetch-Dest") != "" ||
			header.Get("Origin") != "" || header.Get("Referer") != "" || strings.Contains(header.Get("Accept"), "text/html") {
			http.NotFound(writer, request)
			return
		}
		next.ServeHTTP(writer, request)
	})
}

// hideMethods turns a 405 (with its Allow list) into the bare 404 of an
// unknown path, so the API does not reveal which methods a path has.
func hideMethods(next http.Handler) http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		next.ServeHTTP(&methodHider{ResponseWriter: writer, request: request}, request)
	})
}

type methodHider struct {
	http.ResponseWriter
	request *http.Request
	hidden  bool
}

func (hider *methodHider) WriteHeader(code int) {
	if code != http.StatusMethodNotAllowed {
		hider.ResponseWriter.WriteHeader(code)
		return
	}
	hider.hidden = true
	hider.Header().Del("Allow")
	http.NotFound(hider.ResponseWriter, hider.request)
}

func (hider *methodHider) Write(data []byte) (int, error) {
	if hider.hidden {
		return len(data), nil
	}
	return hider.ResponseWriter.Write(data)
}

func (hider *methodHider) Unwrap() http.ResponseWriter { return hider.ResponseWriter }

// tokenPaths carry a single-use credential in the path (emailed reset and
// set-password links). Logs keep the route, never the token: anyone reading
// them could otherwise use the link first.
var tokenPaths = []string{"/api/auth/reset-password/"}

func logPath(path string) string {
	for _, tokenPath := range tokenPaths {
		if strings.HasPrefix(path, tokenPath) && len(path) > len(tokenPath) {
			return tokenPath + "{token}"
		}
	}
	return path
}
