package server

import (
	"context"

	"github.com/jayleaton/go-fast-stack/apps/web/internal/api"
	"github.com/jayleaton/go-fast-stack/apps/web/internal/services"
	"github.com/jayleaton/go-fast-stack/packages/auth"
)

// CallerFrom is the api.CallerFrom for packages/auth: it turns the session
// that auth.Middleware loaded into the API's caller.
func CallerFrom(ctx context.Context) (api.Caller, bool) {
	u, s, ok := auth.UserFrom(ctx)
	if !ok {
		return api.Caller{}, false
	}
	return api.Caller{
		User:    services.User(u), // same columns: packages/auth owns the "user" table
		Session: api.Session{ID: s.ID, UserID: s.UserID, ExpiresAt: s.ExpiresAt},
	}, true
}
