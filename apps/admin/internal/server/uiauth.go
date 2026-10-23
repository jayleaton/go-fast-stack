package server

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"github.com/jayleaton/go-fast-stack/apps/admin/internal/config"
	"github.com/jayleaton/go-fast-stack/apps/admin/ui"
	"github.com/jayleaton/go-fast-stack/packages/auth"
)

// UIAuth adapts the admin *auth.Auth to ui.Auth: it converts types and
// translates auth errors into the ui.Err* values. Auditing needs nothing
// here: the auth instance reports every sign-in, sign-out and failed 2FA code
// to its Config.Events (NewAuditEvents), whichever path it came through.
type UIAuth struct {
	auth *auth.Auth
}

var _ ui.Auth = UIAuth{}

// NewUIAuth returns the adapter over the admin auth instance.
func NewUIAuth(instance *auth.Auth) UIAuth {
	return UIAuth{auth: instance}
}

func (adapter UIAuth) SignIn(ctx context.Context, email, password string) (ui.Session, error) {
	_, session, err := adapter.auth.SignInEmail(ctx, auth.SignInInput{Email: email, Password: password})
	return uiSession(session), uiAuthErr(err)
}

func (adapter UIAuth) SignOut(ctx context.Context, sessionToken string) error {
	return uiAuthErr(adapter.auth.SignOut(ctx, sessionToken))
}

func (adapter UIAuth) CompleteSetPassword(ctx context.Context, token, newPassword string) (ui.Session, error) {
	_, session, err := adapter.auth.CompleteSetPassword(ctx, token, newPassword)
	return uiSession(session), uiAuthErr(err)
}

func (adapter UIAuth) RequestPasswordReset(ctx context.Context, email string) error {
	err := adapter.auth.RequestPasswordReset(ctx, email, config.AppRoutes.ResetPassword)
	if errors.Is(err, auth.ErrInvalidEmail) {
		return nil // same answer as an unknown address
	}
	return uiAuthErr(err)
}

func (adapter UIAuth) VerifyTwoFactor(ctx context.Context, in ui.VerifyTwoFactorInput) (ui.Session, string, error) {
	res, err := adapter.auth.VerifyTwoFactor(ctx, auth.VerifyTwoFactorInput{Code: in.Code, Method: in.Method, TrustDevice: in.TrustDevice})
	if err != nil {
		return ui.Session{}, "", uiAuthErr(err)
	}
	return uiSession(res.Session), res.TrustToken, nil
}

func (adapter UIAuth) StartTwoFactorSetup(ctx context.Context) (ui.TOTPSetup, error) {
	setup, err := adapter.auth.StartTwoFactorSetup(ctx, "") // "": the challenge from the request's cookie
	return ui.TOTPSetup(setup), uiAuthErr(err)
}

func (adapter UIAuth) EnableTwoFactor(ctx context.Context, adminID, password string) (ui.TOTPSetup, error) {
	setup, err := adapter.auth.EnableTwoFactor(ctx, adminID, password)
	return ui.TOTPSetup(setup), uiAuthErr(err)
}

func (adapter UIAuth) ConfirmTwoFactor(ctx context.Context, adminID, code string) error {
	return uiAuthErr(adapter.auth.ConfirmTwoFactor(ctx, adminID, code))
}

func (adapter UIAuth) SetSessionCookie(writer http.ResponseWriter, session ui.Session) {
	adapter.auth.SetSessionCookie(writer, auth.Session{ID: session.ID, Token: session.Token, ExpiresAt: session.ExpiresAt})
}

func (adapter UIAuth) ClearSessionCookie(writer http.ResponseWriter) {
	adapter.auth.ClearSessionCookie(writer)
}

func (adapter UIAuth) SetTwoFactorCookie(writer http.ResponseWriter, challenge *ui.TwoFactorChallenge) {
	adapter.auth.SetTwoFactorCookie(writer, &auth.TwoFactorChallenge{Token: challenge.Token, Methods: challenge.Methods, SetupRequired: challenge.SetupRequired, ExpiresAt: challenge.ExpiresAt})
}

func (adapter UIAuth) SetTrustDeviceCookie(writer http.ResponseWriter, token string) {
	adapter.auth.SetTrustDeviceCookie(writer, token)
}

func uiSession(session auth.Session) ui.Session {
	return ui.Session{ID: session.ID, Token: session.Token, ExpiresAt: session.ExpiresAt}
}

// uiAuthErrors translates auth errors into the ui port's.
var uiAuthErrors = []struct {
	auth error
	port error
}{
	{auth.ErrInvalidEmailOrPassword, ui.ErrInvalidCredentials},
	{auth.ErrBannedUser, ui.ErrInvalidCredentials},
	{auth.ErrInvalidEmail, ui.ErrInvalidCredentials},
	{auth.ErrInvalidCode, ui.ErrInvalidCode},
	{auth.ErrInvalidBackupCode, ui.ErrInvalidCode},
	{auth.ErrInvalidToken, ui.ErrInvalidToken},
	{auth.ErrTokenExpired, ui.ErrInvalidToken},
	{auth.ErrPasswordTooShort, ui.ErrPasswordPolicy},
	{auth.ErrPasswordTooLong, ui.ErrPasswordPolicy},
	{auth.ErrInvalidPassword, ui.ErrInvalidPassword},
	{auth.ErrInvalidTwoFactorCookie, ui.ErrChallengeExpired},
	{auth.ErrTwoFactorLocked, ui.ErrTooManyRequests},
	{auth.ErrTooManyRequests, ui.ErrTooManyRequests},
}

// uiAuthErr turns a 2FA challenge into the ui's and wraps a known auth error
// in its port error (keeping the original in the chain); everything else
// passes through.
func uiAuthErr(err error) error {
	if err == nil {
		return nil
	}
	var challenge *auth.TwoFactorChallenge
	if errors.As(err, &challenge) {
		return &ui.TwoFactorChallenge{Token: challenge.Token, Methods: challenge.Methods, SetupRequired: challenge.SetupRequired, ExpiresAt: challenge.ExpiresAt}
	}
	for _, mapping := range uiAuthErrors {
		if errors.Is(err, mapping.auth) {
			return fmt.Errorf("%w: %w", mapping.port, err)
		}
	}
	return err
}
