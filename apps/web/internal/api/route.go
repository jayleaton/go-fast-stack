package api

import (
	"context"
	"net/http"
	"time"

	"github.com/jayleaton/ai-patterns-go/apps/web/internal/services"
)

// maxBody caps JSON request bodies.
const maxBody = 1 << 20

// Session is the auth session exposed to API clients.
type Session struct {
	ID        string    `json:"id"`
	UserID    string    `json:"userId"`
	ExpiresAt time.Time `json:"expiresAt"`
}

// Caller is the signed-in user and session for a request.
type Caller struct {
	User    services.User
	Session Session
}

// Actor is the service-layer identity of the caller.
func (c Caller) Actor() services.Actor {
	return services.Actor{UserID: c.User.ID, Email: c.User.Email}
}

// CallerFrom reads the caller the auth middleware stored in the request
// context. It mirrors auth.UserFrom (the server adapts one to the other).
type CallerFrom func(ctx context.Context) (Caller, bool)

// Public wraps a handler that needs no session.
func Public[T any](h func(r *http.Request) (T, error)) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.Body = http.MaxBytesReader(w, r.Body, maxBody)
		respond(w, r, h)
	})
}

// Authenticated wraps a handler that requires a session: no caller is a 401.
func Authenticated[T any](from CallerFrom, h func(r *http.Request, c Caller) (T, error)) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, ok := from(r.Context())
		if !ok {
			WriteError(w, http.StatusUnauthorized, "Unauthorized", nil)
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, maxBody)
		respond(w, r, func(r *http.Request) (T, error) { return h(r, c) })
	})
}

func respond[T any](w http.ResponseWriter, r *http.Request, h func(r *http.Request) (T, error)) {
	data, err := h(r)
	if err != nil {
		writeErr(w, r, err)
		return
	}
	WriteData(w, http.StatusOK, data)
}
