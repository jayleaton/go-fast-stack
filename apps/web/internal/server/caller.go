package server

import (
	"context"

	"github.com/jayleaton/go-fast-stack/apps/web/internal/access"
	"github.com/jayleaton/go-fast-stack/apps/web/internal/services"
	"github.com/jayleaton/go-fast-stack/packages/auth"
)

// Callers is the access.Resolver over packages/auth: it turns the session
// that auth.Middleware loaded into the caller both transports see. To give
// policies more to go on (roles, the tenant), wrap it here with the lookup
// (docs/route-access.md); routes and middleware stay as they are.
var Callers = access.FromContext(callerFrom)

func callerFrom(ctx context.Context) (access.Caller, bool) {
	user, session, ok := auth.UserFrom(ctx)
	if !ok {
		return access.Caller{}, false
	}
	return access.Caller{
		User:    services.User(user), // same columns: packages/auth owns the "user" table
		Session: access.Session{ID: session.ID, UserID: session.UserID, ExpiresAt: session.ExpiresAt, Token: session.Token},
	}, true
}
