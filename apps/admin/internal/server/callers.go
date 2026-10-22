package server

import (
	"context"
	"log/slog"

	"github.com/jayleaton/ai-patterns-go/apps/admin/internal/api"
	"github.com/jayleaton/ai-patterns-go/apps/admin/ui"
	"github.com/jayleaton/ai-patterns-go/packages/auth"
)

// Callers turns the admin session that the admin auth.Middleware loaded into
// each transport's caller. It also reads whether the admin has 2FA on, for
// services.Policy (sessions can predate ADMIN_REQUIRE_2FA).
type Callers struct {
	auth *auth.Auth
}

// NewCallers returns the caller adapters over the admin auth instance.
func NewCallers(a *auth.Auth) Callers {
	return Callers{auth: a}
}

// API is the api.CallerFrom of the admin JSON API.
func (c Callers) API(ctx context.Context) (api.Caller, bool) {
	u, s, twoFactor, ok := c.signedIn(ctx)
	if !ok {
		return api.Caller{}, false
	}
	return api.Caller{
		Admin:   api.Admin{ID: u.ID, Name: u.Name, Email: u.Email, TwoFactorEnabled: twoFactor},
		Session: api.Session{ID: s.ID, ExpiresAt: s.ExpiresAt},
	}, true
}

// UI is the ui.Deps.UserFrom of the admin UI.
func (c Callers) UI(ctx context.Context) (ui.Admin, ui.Session, bool) {
	u, s, twoFactor, ok := c.signedIn(ctx)
	if !ok {
		return ui.Admin{}, ui.Session{}, false
	}
	return ui.Admin{ID: u.ID, Name: u.Name, Email: u.Email, TwoFactorEnabled: twoFactor},
		ui.Session{ID: s.ID, Token: s.Token, ExpiresAt: s.ExpiresAt}, true
}

func (c Callers) signedIn(ctx context.Context) (auth.User, auth.Session, bool, bool) {
	u, s, ok := auth.UserFrom(ctx)
	if !ok {
		return auth.User{}, auth.Session{}, false, false
	}
	twoFactor, err := c.auth.TwoFactorEnabled(ctx, u.ID)
	if err != nil {
		// Fail closed: without the flag a 2FA-required dashboard refuses the
		// admin (services.ErrTwoFactorRequired) instead of letting them in.
		slog.WarnContext(ctx, "admin: two-factor lookup failed", "admin_id", u.ID, "err", err)
		return u, s, false, true
	}
	return u, s, twoFactor, true
}
