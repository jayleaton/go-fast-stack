// Package api is the JSON transport (/api/core/v1). Handlers decode input with
// validate, call a service, and return a value; the route wrapper turns it into
// the envelope {"data": T|null, "error": string|null}.
package api

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"

	"github.com/jayleaton/go-fast-stack/apps/web/internal/services"
	"github.com/jayleaton/go-fast-stack/apps/web/internal/validate"
)

// Envelope is the shape of every JSON response. Details is only set for
// validation errors (field -> message).
type Envelope[T any] struct {
	Data    *T                `json:"data"`
	Error   *string           `json:"error"`
	Details map[string]string `json:"details,omitempty"`
}

// WriteData writes a success envelope.
func WriteData[T any](w http.ResponseWriter, status int, data T) {
	write(w, status, Envelope[T]{Data: &data})
}

// WriteError writes an error envelope.
func WriteError(w http.ResponseWriter, status int, msg string, details map[string]string) {
	write(w, status, Envelope[struct{}]{Error: &msg, Details: details})
}

// writeErr maps validation and service errors to a status and safe message;
// anything else is logged and reported as a 500.
func writeErr(w http.ResponseWriter, r *http.Request, err error) {
	var verr *validate.Error
	var serr *services.Error
	switch {
	case errors.As(err, &verr):
		WriteError(w, http.StatusBadRequest, "Invalid request data", verr.Fields)
	case errors.As(err, &serr) && serr.Kind != services.KindInternal:
		WriteError(w, statusFor(serr.Kind), serr.Message, nil)
	default:
		slog.ErrorContext(r.Context(), "request failed", "method", r.Method, "path", r.URL.Path, "err", err)
		WriteError(w, http.StatusInternalServerError, "Internal server error", nil)
	}
}

func statusFor(k services.Kind) int {
	switch k {
	case services.KindInvalid:
		return http.StatusBadRequest
	case services.KindUnauthorized:
		return http.StatusUnauthorized
	case services.KindForbidden:
		return http.StatusForbidden
	case services.KindNotFound:
		return http.StatusNotFound
	case services.KindConflict:
		return http.StatusConflict
	case services.KindRateLimited:
		return http.StatusTooManyRequests
	default:
		return http.StatusInternalServerError
	}
}

func write(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
