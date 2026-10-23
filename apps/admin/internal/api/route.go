package api

import (
	"errors"
	"log/slog"
	"net/http"

	"github.com/jayleaton/go-fast-stack/apps/admin/internal/access"
)

// maxBody caps JSON request bodies at 1 MiB.
const maxBody = 1 << 20

// Handler is a JSON endpoint: it gets the caller the gate admitted (the zero
// Caller on a public route without a session) and returns the data for the
// envelope or an error to map.
type Handler[Result any] func(request *http.Request, caller access.Caller) (Result, error)

// Route is the one route handler of the admin JSON API. The gate resolves
// the caller and enforces policy (signed out: 401, denied: 403, both in the
// envelope); then the body is capped, handler runs, and its result or error
// becomes the envelope. Declare the access on the registration line:
//
//	mux.Handle("GET "+routes.Users, Route(users.gate, access.SignedIn, users.list))
func Route[Result any](gate *access.Gate, policy access.Policy, handler Handler[Result]) http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		caller, err := gate.Admit(request, policy)
		switch {
		case err == nil:
		case errors.Is(err, access.ErrSignedOut):
			WriteError(writer, http.StatusUnauthorized, "Unauthorized", nil)
			return
		case errors.Is(err, access.ErrDenied):
			slog.InfoContext(request.Context(), "access denied", "method", request.Method, "path", request.URL.Path, "err", err)
			WriteError(writer, http.StatusForbidden, "Forbidden", nil)
			return
		default:
			writeErr(writer, request, err)
			return
		}
		request.Body = http.MaxBytesReader(writer, request.Body, maxBody)
		data, err := handler(request, caller)
		if err != nil {
			writeErr(writer, request, err)
			return
		}
		WriteData(writer, http.StatusOK, data)
	})
}
