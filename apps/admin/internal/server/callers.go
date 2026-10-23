package server

import (
	"log/slog"
	"net/http"

	"github.com/jayleaton/go-fast-stack/apps/admin/internal/access"
	"github.com/jayleaton/go-fast-stack/packages/auth"
)

// Callers turns the admin session that the admin auth.Middleware loaded into
// the caller both transports see. It also reads whether the admin has 2FA
// on, for services.Policy (sessions can predate ADMIN_REQUIRE_2FA). To give
// policies more to go on (roles), add the lookup here (docs/route-access.md);
// routes and middleware stay as they are.
type Callers struct {
	auth *auth.Auth
}

// NewCallers returns the caller resolver over the admin auth instance.
func NewCallers(admins *auth.Auth) Callers {
	return Callers{auth: admins}
}

// Resolve is the access.Resolver of the admin app.
func (callers Callers) Resolve(request *http.Request) (access.Caller, error) {
	ctx := request.Context()
	user, session, ok := auth.UserFrom(ctx)
	if !ok {
		return access.Caller{}, nil
	}
	twoFactor, err := callers.auth.TwoFactorEnabled(ctx, user.ID)
	if err != nil {
		// Fail closed: without the flag a 2FA-required dashboard refuses the
		// admin (services.ErrTwoFactorRequired) instead of letting them in.
		slog.WarnContext(ctx, "admin: two-factor lookup failed", "admin_id", user.ID, "err", err)
		twoFactor = false
	}
	return access.Caller{
		Admin:   access.Admin{ID: user.ID, Name: user.Name, Email: user.Email, TwoFactorEnabled: twoFactor},
		Session: access.Session{ID: session.ID, ExpiresAt: session.ExpiresAt, Token: session.Token},
	}, nil
}
