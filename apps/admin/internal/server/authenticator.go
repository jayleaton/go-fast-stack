package server

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"github.com/jayleaton/go-fast-stack/apps/admin/internal/api"
	"github.com/jayleaton/go-fast-stack/apps/admin/internal/apiclient"
	"github.com/jayleaton/go-fast-stack/apps/admin/internal/config"
	"github.com/jayleaton/go-fast-stack/apps/admin/internal/validate"
	"github.com/jayleaton/go-fast-stack/packages/auth"
)

// Authenticator is the api.Authenticator over the admin *auth.Auth: it
// converts types, captures the cookies auth would set into the results
// (the UI server action sets them) and translates auth errors into the
// apiclient.Err* values. Auditing needs nothing here: the auth instance
// reports every sign-in, sign-out and failed 2FA code to its Config.Events
// (NewAuditEvents), whichever path it came through.
type Authenticator struct {
	auth *auth.Auth
}

var _ api.Authenticator = Authenticator{}

// NewAuthenticator returns the adapter over the admin auth instance.
func NewAuthenticator(instance *auth.Auth) Authenticator {
	return Authenticator{auth: instance}
}

// SignIn checks the password; a challenge error when 2FA is next.
func (adapter Authenticator) SignIn(ctx context.Context, input validate.SignIn) (api.AuthResult, error) {
	_, session, err := adapter.auth.SignInEmail(ctx, auth.SignInInput{Email: input.Email, Password: input.Password})
	if err != nil {
		return api.AuthResult{}, adapter.authErr(err)
	}
	return adapter.signedIn(session), nil
}

// SignOut ends the session (when there is one) and always clears the cookie.
func (adapter Authenticator) SignOut(ctx context.Context, sessionToken string) (api.AuthResult, error) {
	var err error
	if sessionToken != "" {
		err = adapter.auth.SignOut(ctx, sessionToken)
	}
	return api.AuthResult{Cookies: capture(adapter.auth.ClearSessionCookie)}, adapter.authErr(err)
}

// SetPassword spends an invite or reset link and signs in (or challenges).
func (adapter Authenticator) SetPassword(ctx context.Context, input validate.SetPassword) (api.AuthResult, error) {
	_, session, err := adapter.auth.CompleteSetPassword(ctx, input.Token, input.Password)
	if err != nil {
		return api.AuthResult{}, adapter.authErr(err)
	}
	return adapter.signedIn(session), nil
}

// RequestPasswordReset emails a reset link; an invalid address gets the
// same answer as an unknown one.
func (adapter Authenticator) RequestPasswordReset(ctx context.Context, input validate.CreatePasswordReset) error {
	err := adapter.auth.RequestPasswordReset(ctx, input.Email, config.AppRoutes.ResetPassword)
	if errors.Is(err, auth.ErrInvalidEmail) {
		return nil
	}
	return adapter.authErr(err)
}

// VerifyTwoFactor finishes sign-in with the request's challenge cookie;
// the result carries the session cookie and, if asked, the trusted device.
func (adapter Authenticator) VerifyTwoFactor(ctx context.Context, input validate.VerifyTwoFactor) (api.AuthResult, error) {
	verified, err := adapter.auth.VerifyTwoFactor(ctx, auth.VerifyTwoFactorInput{Code: input.Code, Method: input.Method, TrustDevice: input.TrustDevice})
	if err != nil {
		return api.AuthResult{}, adapter.authErr(err)
	}
	result := adapter.signedIn(verified.Session)
	if verified.TrustToken != "" {
		result.Cookies = append(result.Cookies, capture(func(writer http.ResponseWriter) { adapter.auth.SetTrustDeviceCookie(writer, verified.TrustToken) })...)
	}
	return result, nil
}

// StartTwoFactorSetup enrols the SetupRequired challenge from the cookie.
func (adapter Authenticator) StartTwoFactorSetup(ctx context.Context) (api.TOTPSetup, error) {
	setup, err := adapter.auth.StartTwoFactorSetup(ctx, "") // "": the challenge from the request's cookie
	return api.TOTPSetup(setup), adapter.authErr(err)
}

// EnableTwoFactor starts optional enrolment after checking the password.
func (adapter Authenticator) EnableTwoFactor(ctx context.Context, adminID string, input validate.EnableTwoFactor) (api.TOTPSetup, error) {
	setup, err := adapter.auth.EnableTwoFactor(ctx, adminID, input.Password)
	return api.TOTPSetup(setup), adapter.authErr(err)
}

// ConfirmTwoFactor confirms optional enrolment.
func (adapter Authenticator) ConfirmTwoFactor(ctx context.Context, adminID string, input validate.ConfirmTwoFactor) error {
	return adapter.authErr(adapter.auth.ConfirmTwoFactor(ctx, adminID, input.Code))
}

func (adapter Authenticator) signedIn(session auth.Session) api.AuthResult {
	if session.Token == "" {
		return api.AuthResult{}
	}
	return api.AuthResult{SignedIn: true, Cookies: capture(func(writer http.ResponseWriter) { adapter.auth.SetSessionCookie(writer, session) })}
}

// authErrors translates auth errors into the API's.
var authErrors = []struct{ from, to error }{
	{auth.ErrInvalidEmailOrPassword, apiclient.ErrInvalidCredentials},
	{auth.ErrBannedUser, apiclient.ErrInvalidCredentials},
	{auth.ErrInvalidEmail, apiclient.ErrInvalidCredentials},
	{auth.ErrInvalidCode, apiclient.ErrInvalidCode},
	{auth.ErrInvalidBackupCode, apiclient.ErrInvalidCode},
	{auth.ErrInvalidToken, apiclient.ErrInvalidToken},
	{auth.ErrTokenExpired, apiclient.ErrInvalidToken},
	{auth.ErrPasswordTooShort, apiclient.ErrPasswordPolicy},
	{auth.ErrPasswordTooLong, apiclient.ErrPasswordPolicy},
	{auth.ErrInvalidPassword, apiclient.ErrInvalidPassword},
	{auth.ErrInvalidTwoFactorCookie, apiclient.ErrChallengeExpired},
	{auth.ErrTwoFactorLocked, apiclient.ErrTooManyRequests},
	{auth.ErrTooManyRequests, apiclient.ErrTooManyRequests},
}

// authErr turns a 2FA challenge into the client's (with the challenge
// cookie captured) and wraps a known auth error in its API error, keeping
// the original in the chain; everything else passes through.
func (adapter Authenticator) authErr(err error) error {
	if err == nil {
		return nil
	}
	var challenge *auth.TwoFactorChallenge
	if errors.As(err, &challenge) {
		return &apiclient.TwoFactorChallenge{
			Methods:       challenge.Methods,
			SetupRequired: challenge.SetupRequired,
			Cookies:       capture(func(writer http.ResponseWriter) { adapter.auth.SetTwoFactorCookie(writer, challenge) }),
		}
	}
	for _, mapping := range authErrors {
		if errors.Is(err, mapping.from) {
			return fmt.Errorf("%w: %w", mapping.to, err)
		}
	}
	return err
}

// capture runs write against a recorder and returns the cookies it set, as
// data: auth signs its cookies, the UI sets them (ui.setCookies).
func capture(write func(writer http.ResponseWriter)) []apiclient.Cookie {
	recorder := &headerRecorder{header: http.Header{}}
	write(recorder)
	var cookies []apiclient.Cookie
	for _, cookie := range (&http.Response{Header: recorder.header}).Cookies() {
		cookies = append(cookies, apiclient.Cookie{Name: cookie.Name, Value: cookie.Value, MaxAge: cookie.MaxAge, Secure: cookie.Secure})
	}
	return cookies
}

// headerRecorder is the http.ResponseWriter capture writes cookies to.
type headerRecorder struct{ header http.Header }

func (recorder *headerRecorder) Header() http.Header { return recorder.header }

func (*headerRecorder) Write(body []byte) (int, error) { return len(body), nil }

func (*headerRecorder) WriteHeader(int) {}
