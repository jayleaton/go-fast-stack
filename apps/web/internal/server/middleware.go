package server

import (
	"errors"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/jayleaton/go-fast-stack/apps/web/internal/api"
	"github.com/jayleaton/go-fast-stack/apps/web/internal/config"
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

// apiPrefix is the JSON API subtree (config.APIRoutes: auth and core).
const apiPrefix = "/api/"

// apiOnly hides next from browsers: a request carrying Fetch Metadata
// (Sec-Fetch-*), Origin or Referer, or asking for HTML, gets the bare 404 of
// an unknown path. Browsers send Fetch Metadata on every https request and
// Origin on every POST and cross-origin fetch; over plain http (a dev box on
// the LAN) a typed-in URL still asks for text/html. Native apps and
// server-to-server clients send none of these. No CORS headers are ever set,
// so preflights fail too. The one exception is a top-level navigation to an
// emailed auth link whose callback is a deep link or another origin (the
// mobile app): those endpoints only redirect, and any JSON they would answer
// with becomes the same 404. publicOrigin is config.PublicURL.
func apiOnly(next http.Handler, publicOrigin string) http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if !fromBrowser(request) {
			next.ServeHTTP(writer, request)
			return
		}
		if linkNavigation(request, publicOrigin) {
			next.ServeHTTP(&jsonHider{ResponseWriter: writer, request: request}, request)
			return
		}
		http.NotFound(writer, request)
	})
}

func fromBrowser(request *http.Request) bool {
	header := request.Header
	return fetchMetadata(request) || header.Get("Origin") != "" || header.Get("Referer") != "" ||
		strings.Contains(header.Get("Accept"), "text/html")
}

func fetchMetadata(request *http.Request) bool {
	header := request.Header
	return header.Get("Sec-Fetch-Site") != "" || header.Get("Sec-Fetch-Mode") != "" || header.Get("Sec-Fetch-Dest") != ""
}

// linkPaths are auth's GET endpoints behind emailed links for absolute or
// deep-link callbacks; links for the web UI point at its pages instead.
var linkPaths = []string{config.APIRoutes.Auth + "verify-email", config.APIRoutes.Auth + "reset-password/"}

// linkNavigation reports a browser opening an emailed auth link meant for
// another app: a top-level GET (Fetch Metadata says navigate; without it, as
// over plain http or on older Safari, it asks for HTML) to a link endpoint
// whose callbackURL is a deep link or an absolute URL on another origin.
func linkNavigation(request *http.Request, publicOrigin string) bool {
	nav := request.Header.Get("Sec-Fetch-Mode") == "navigate" ||
		(!fetchMetadata(request) && strings.Contains(request.Header.Get("Accept"), "text/html"))
	if request.Method != http.MethodGet || request.Header.Get("Origin") != "" || !nav {
		return false
	}
	cb, err := url.Parse(request.URL.Query().Get("callbackURL"))
	if err != nil || cb.Scheme == "" {
		return false // missing or relative: links for this site open its pages
	}
	if (cb.Scheme == "http" || cb.Scheme == "https") && strings.EqualFold(cb.Scheme+"://"+cb.Host, publicOrigin) {
		return false
	}
	for _, linkPath := range linkPaths {
		if request.URL.Path == linkPath || (strings.HasSuffix(linkPath, "/") && strings.HasPrefix(request.URL.Path, linkPath)) {
			return true
		}
	}
	return false
}

// jsonHider turns a JSON answer (an auth error) into the bare 404, so a
// browser on a link endpoint only ever sees a redirect.
type jsonHider struct {
	http.ResponseWriter
	request *http.Request
	hidden  bool
}

func (hider *jsonHider) WriteHeader(code int) {
	if !strings.Contains(hider.Header().Get("Content-Type"), "json") {
		hider.ResponseWriter.WriteHeader(code)
		return
	}
	hider.hidden = true
	for _, key := range []string{"Set-Auth-Token", "Access-Control-Expose-Headers", "X-Retry-After"} {
		hider.Header().Del(key)
	}
	http.NotFound(hider.ResponseWriter, hider.request)
}

func (hider *jsonHider) Write(data []byte) (int, error) {
	if hider.hidden {
		return len(data), nil
	}
	return hider.ResponseWriter.Write(data)
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
