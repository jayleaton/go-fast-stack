package server

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"github.com/jayleaton/ai-patterns-go/apps/admin/ui"
	"github.com/jayleaton/ai-patterns-go/packages/auth"
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
func NewUIAuth(a *auth.Auth) UIAuth {
	return UIAuth{auth: a}
}

func (a UIAuth) SignIn(ctx context.Context, email, password string) (ui.Session, error) {
	_, s, err := a.auth.SignInEmail(ctx, auth.SignInInput{Email: email, Password: password})
	return uiSession(s), uiAuthErr(err)
}

func (a UIAuth) SignOut(ctx context.Context, sessionToken string) error {
	return uiAuthErr(a.auth.SignOut(ctx, sessionToken))
}

func (a UIAuth) CompleteSetPassword(ctx context.Context, token, newPassword string) (ui.Session, error) {
	_, s, err := a.auth.CompleteSetPassword(ctx, token, newPassword)
	return uiSession(s), uiAuthErr(err)
}

func (a UIAuth) VerifyTwoFactor(ctx context.Context, in ui.VerifyTwoFactorInput) (ui.Session, string, error) {
	res, err := a.auth.VerifyTwoFactor(ctx, auth.VerifyTwoFactorInput{Code: in.Code, Method: in.Method, TrustDevice: in.TrustDevice})
	if err != nil {
		return ui.Session{}, "", uiAuthErr(err)
	}
	return uiSession(res.Session), res.TrustToken, nil
}

func (a UIAuth) StartTwoFactorSetup(ctx context.Context) (ui.TOTPSetup, error) {
	s, err := a.auth.StartTwoFactorSetup(ctx, "") // "": the challenge from the request's cookie
	return ui.TOTPSetup(s), uiAuthErr(err)
}

func (a UIAuth) EnableTwoFactor(ctx context.Context, adminID, password string) (ui.TOTPSetup, error) {
	s, err := a.auth.EnableTwoFactor(ctx, adminID, password)
	return ui.TOTPSetup(s), uiAuthErr(err)
}

func (a UIAuth) ConfirmTwoFactor(ctx context.Context, adminID, code string) error {
	return uiAuthErr(a.auth.ConfirmTwoFactor(ctx, adminID, code))
}

func (a UIAuth) SetSessionCookie(w http.ResponseWriter, s ui.Session) {
	a.auth.SetSessionCookie(w, auth.Session{ID: s.ID, Token: s.Token, ExpiresAt: s.ExpiresAt})
}

func (a UIAuth) ClearSessionCookie(w http.ResponseWriter) { a.auth.ClearSessionCookie(w) }

func (a UIAuth) SetTwoFactorCookie(w http.ResponseWriter, c *ui.TwoFactorChallenge) {
	a.auth.SetTwoFactorCookie(w, &auth.TwoFactorChallenge{Token: c.Token, Methods: c.Methods, SetupRequired: c.SetupRequired, ExpiresAt: c.ExpiresAt})
}

func (a UIAuth) SetTrustDeviceCookie(w http.ResponseWriter, token string) {
	a.auth.SetTrustDeviceCookie(w, token)
}

func uiSession(s auth.Session) ui.Session {
	return ui.Session{ID: s.ID, Token: s.Token, ExpiresAt: s.ExpiresAt}
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
	var c *auth.TwoFactorChallenge
	if errors.As(err, &c) {
		return &ui.TwoFactorChallenge{Token: c.Token, Methods: c.Methods, SetupRequired: c.SetupRequired, ExpiresAt: c.ExpiresAt}
	}
	for _, e := range uiAuthErrors {
		if errors.Is(err, e.auth) {
			return fmt.Errorf("%w: %w", e.port, err)
		}
	}
	return err
}
