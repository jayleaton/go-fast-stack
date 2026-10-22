// Package devauth signs every request in as a seeded dev user so the API can
// be exercised before packages/auth is wired. It is only used when
// config.Features.DevAuth is on (development).
//
// TODO(feat/auth): delete this package and wire auth.New in cmd/server.
package devauth

import (
	"context"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/jayleaton/ai-patterns-go/apps/web/internal/api"
	"github.com/jayleaton/ai-patterns-go/packages/database"
)

// User is the seeded dev user.
const (
	UserID    = "dev-user"
	UserEmail = "dev@example.com"
)

type callerKey struct{}

// Fake implements server.Auth with a fixed user.
type Fake struct {
	users *database.UserRepository
}

// New seeds the dev user (if missing) and returns the fake.
func New(ctx context.Context, pool *pgxpool.Pool) (*Fake, error) {
	_, err := pool.Exec(ctx, `INSERT INTO "user" (id, name, email, "emailVerified") VALUES ($1, 'Dev User', $2, true) ON CONFLICT (id) DO NOTHING`, UserID, UserEmail)
	if err != nil {
		return nil, err
	}
	return &Fake{users: database.NewUserRepository(pool)}, nil
}

// Middleware loads the dev user into the context. A deleted dev user means
// signed out until the next restart re-seeds it.
func (f *Fake) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if u, err := f.users.GetUserByID(r.Context(), UserID); err == nil {
			c := api.Caller{User: u, Session: api.Session{ID: "dev-session", UserID: u.ID, ExpiresAt: time.Now().Add(24 * time.Hour)}}
			r = r.WithContext(context.WithValue(r.Context(), callerKey{}, c))
		}
		next.ServeHTTP(w, r)
	})
}

// Handler answers the auth endpoints with 501 until packages/auth is wired.
func (f *Fake) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		api.WriteError(w, http.StatusNotImplemented, "auth is not wired yet (dev auth fake)", nil)
	})
}

// CallerFrom is the api.CallerFrom for the fake.
func CallerFrom(ctx context.Context) (api.Caller, bool) {
	c, ok := ctx.Value(callerKey{}).(api.Caller)
	return c, ok
}

// NoAuth is used outside development until packages/auth is wired: nobody is
// signed in, so authenticated routes return 401.
type NoAuth struct{}

func (NoAuth) Middleware(next http.Handler) http.Handler { return next }
func (NoAuth) Handler() http.Handler                     { return (&Fake{}).Handler() }
