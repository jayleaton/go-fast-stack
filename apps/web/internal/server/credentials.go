package server

import (
	"context"
	"errors"
	"fmt"

	"github.com/jayleaton/ai-patterns-go/apps/web/internal/services"
	"github.com/jayleaton/ai-patterns-go/packages/auth"
)

// AccountCredentials adapts *auth.Auth to services.AccountCredentials. It is
// translation only: rules belong in services.UserService, credential storage
// in packages/auth.
type AccountCredentials struct {
	auth *auth.Auth
}

var _ services.AccountCredentials = AccountCredentials{}

// NewAccountCredentials returns the adapter over a.
func NewAccountCredentials(a *auth.Auth) AccountCredentials {
	return AccountCredentials{auth: a}
}

// errNotWired marks port methods whose packages/auth counterpart has not landed.
// TODO(feat/account-auth): replace each stub body with the one-line call in
// its comment, then delete errNotWired.
var errNotWired = errors.New("account credentials: needs packages/auth from feat/account-auth")

// ChangePassword: return credentialErr(c.auth.ChangePassword(ctx, userID, current, next, revokeOthers, currentSessionID))
func (c AccountCredentials) ChangePassword(_ context.Context, _, _, _ string, _ bool, _ string) error {
	return credentialErr(errNotWired)
}

// ChangeEmail: return credentialErr(c.auth.ChangeEmail(ctx, userID, newEmail, callbackURL))
func (c AccountCredentials) ChangeEmail(_ context.Context, _, _, _ string) error {
	return credentialErr(errNotWired)
}

// ListSessions: s, err := c.auth.ListSessions(ctx, userID); return sessions(s), credentialErr(err)
func (c AccountCredentials) ListSessions(_ context.Context, _ string) ([]services.Session, error) {
	return sessions(nil), credentialErr(errNotWired)
}

// RevokeSession: return credentialErr(c.auth.RevokeSession(ctx, userID, sessionID))
func (c AccountCredentials) RevokeSession(_ context.Context, _, _ string) error {
	return credentialErr(errNotWired)
}

// RevokeOtherSessions: return credentialErr(c.auth.RevokeOtherSessions(ctx, userID, currentSessionID))
func (c AccountCredentials) RevokeOtherSessions(_ context.Context, _, _ string) error {
	return credentialErr(errNotWired)
}

// sessions converts auth sessions to the port type, dropping the token.
func sessions(in []auth.Session) []services.Session {
	out := make([]services.Session, len(in))
	for i, s := range in {
		out[i] = services.Session{ID: s.ID, CreatedAt: s.CreatedAt, ExpiresAt: s.ExpiresAt, IPAddress: s.IPAddress, UserAgent: s.UserAgent}
	}
	return out
}

// credentialErrors translates auth errors into the port's.
// TODO(feat/account-auth): add auth.ErrInvalidPassword -> ErrWrongPassword,
// auth.ErrEmailTaken -> ErrEmailTaken, auth.ErrSessionNotFound -> ErrSessionNotFound.
var credentialErrors = []struct{ auth, port error }{
	{auth.ErrPasswordTooShort, services.ErrPasswordPolicy},
	{auth.ErrPasswordTooLong, services.ErrPasswordPolicy},
	{auth.ErrUserAlreadyExists, services.ErrEmailTaken},
	{auth.ErrInvalidCallbackURL, services.ErrInvalidCallbackURL},
	{auth.ErrUserNotFound, services.ErrUserNotFound},
}

// credentialErr wraps a known auth error in its port error (keeping the
// original in the chain) and passes everything else through.
func credentialErr(err error) error {
	for _, e := range credentialErrors {
		if errors.Is(err, e.auth) {
			return fmt.Errorf("%w: %w", e.port, err)
		}
	}
	return err
}
