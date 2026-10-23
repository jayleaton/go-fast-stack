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
type Envelope[Value any] struct {
	Data    *Value            `json:"data"`
	Error   *string           `json:"error"`
	Details map[string]string `json:"details,omitempty"`
}

// WriteData writes a success envelope.
func WriteData[Value any](writer http.ResponseWriter, status int, data Value) {
	write(writer, status, Envelope[Value]{Data: &data})
}

// WriteError writes an error envelope.
func WriteError(writer http.ResponseWriter, status int, msg string, details map[string]string) {
	write(writer, status, Envelope[struct{}]{Error: &msg, Details: details})
}

// writeErr maps validation and service errors to a status and safe message;
// anything else is logged and reported as a 500.
func writeErr(writer http.ResponseWriter, request *http.Request, err error) {
	var verr *validate.Error
	var serr *services.Error
	switch {
	case errors.As(err, &verr):
		WriteError(writer, http.StatusBadRequest, "Invalid request data", verr.Fields)
	case errors.As(err, &serr) && serr.Kind != services.KindInternal:
		WriteError(writer, statusFor(serr.Kind), serr.Message, nil)
	default:
		slog.ErrorContext(request.Context(), "request failed", "method", request.Method, "path", request.URL.Path, "err", err)
		WriteError(writer, http.StatusInternalServerError, "Internal server error", nil)
	}
}

func statusFor(kind services.Kind) int {
	switch kind {
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

func write(writer http.ResponseWriter, status int, value any) {
	writer.Header().Set("Content-Type", "application/json")
	writer.WriteHeader(status)
	_ = json.NewEncoder(writer).Encode(value)
}
