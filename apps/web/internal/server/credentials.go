package server

import (
	"context"
	"errors"
	"fmt"

	"github.com/jayleaton/go-fast-stack/apps/web/internal/services"
	"github.com/jayleaton/go-fast-stack/packages/auth"
)

// AccountCredentials adapts *auth.Auth to services.AccountCredentials. It is
// translation only: rules belong in services.UserService, credential storage
// in packages/auth.
type AccountCredentials struct {
	auth *auth.Auth
}

var _ services.AccountCredentials = AccountCredentials{}

// NewAccountCredentials returns the adapter over a.
func NewAccountCredentials(auth *auth.Auth) AccountCredentials {
	return AccountCredentials{auth: auth}
}

func (credentials AccountCredentials) ChangePassword(ctx context.Context, userID, current, next string, revokeOthers bool, currentSessionID string) error {
	return credentialErr(credentials.auth.ChangePassword(ctx, userID, current, next, revokeOthers, currentSessionID))
}

func (credentials AccountCredentials) ChangeEmail(ctx context.Context, userID, newEmail, callbackURL string) error {
	return credentialErr(credentials.auth.ChangeEmail(ctx, userID, newEmail, callbackURL))
}

func (credentials AccountCredentials) ListSessions(ctx context.Context, userID string) ([]services.Session, error) {
	in, err := credentials.auth.ListSessions(ctx, userID)
	if err != nil {
		return nil, credentialErr(err)
	}
	out := make([]services.Session, len(in))
	for index, session := range in { // never the token
		out[index] = services.Session{ID: session.ID, CreatedAt: session.CreatedAt, ExpiresAt: session.ExpiresAt, IPAddress: session.IPAddress, UserAgent: session.UserAgent}
	}
	return out, nil
}

func (credentials AccountCredentials) RevokeSession(ctx context.Context, userID, sessionID string) error {
	return credentialErr(credentials.auth.RevokeSession(ctx, userID, sessionID))
}

func (credentials AccountCredentials) RevokeOtherSessions(ctx context.Context, userID, currentSessionID string) error {
	return credentialErr(credentials.auth.RevokeOtherSessions(ctx, userID, currentSessionID))
}

// credentialErrors translates auth errors into the port's.
var credentialErrors = []struct {
	auth error
	port *services.Error
}{
	{auth.ErrInvalidPassword, services.ErrInvalidPassword},
	{auth.ErrPasswordTooShort, services.ErrPasswordPolicy},
	{auth.ErrPasswordTooLong, services.ErrPasswordPolicy},
	{auth.ErrCredentialAccountNotFound, services.ErrNoPassword},
	{auth.ErrEmailUnchanged, services.ErrSameEmail},
	{auth.ErrEmailTaken, services.ErrEmailTaken},
	{auth.ErrUserAlreadyExists, services.ErrEmailTaken},
	{auth.ErrInvalidEmail, services.ErrValidation},
	{auth.ErrInvalidCallbackURL, services.ErrInvalidCallbackURL},
	{auth.ErrSessionNotFresh, services.ErrSessionNotFresh},
	{auth.ErrSessionNotFound, services.ErrSessionNotFound},
	{auth.ErrUserNotFound, services.ErrUserNotFound},
	{auth.ErrTooManyRequests, services.ErrRateLimited},
}

// credentialErr wraps a known auth error in its port error (keeping the
// original in the chain) and passes everything else through.
func credentialErr(err error) error {
	for _, credentialError := range credentialErrors {
		if errors.Is(err, credentialError.auth) {
			return fmt.Errorf("%w: %w", credentialError.port, err)
		}
	}
	return err
}
