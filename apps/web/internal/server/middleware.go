package server

import (
	"errors"
	"log/slog"
	"net/http"
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
			"method", r.Method, "path", r.URL.Path, "status", rec.status,
			"duration", time.Since(start).Round(time.Microsecond))
	})
}

func recoverPanics(log *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if v := recover(); v != nil {
				if err, ok := v.(error); ok && errors.Is(err, http.ErrAbortHandler) {
					panic(v)
				}
				log.ErrorContext(r.Context(), "panic", "method", r.Method, "path", r.URL.Path, "panic", v)
				api.WriteError(w, http.StatusInternalServerError, "Internal server error", nil)
			}
		}()
		next.ServeHTTP(w, r)
	})
}
