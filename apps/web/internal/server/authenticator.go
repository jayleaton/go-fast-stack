package server

import (
	"context"
	"errors"
	"net/http"

	"github.com/jayleaton/go-fast-stack/apps/web/internal/api"
	"github.com/jayleaton/go-fast-stack/apps/web/internal/apiclient"
	"github.com/jayleaton/go-fast-stack/apps/web/internal/services"
	"github.com/jayleaton/go-fast-stack/apps/web/internal/validate"
	"github.com/jayleaton/go-fast-stack/packages/auth"
)

// Authenticator is the api.Authenticator over packages/auth: it converts
// inputs, captures the cookies auth would set into the result's Cookies
// (the UI server action writes them on its own response) and translates
// auth's errors into the apiclient.Err* values.
type Authenticator struct {
	auth *auth.Auth
}

var _ api.Authenticator = Authenticator{}

// NewAuthenticator returns the adapter over instance.
func NewAuthenticator(instance *auth.Auth) Authenticator {
	return Authenticator{auth: instance}
}

// SignUp creates the user; a session only when verification is off.
func (adapter Authenticator) SignUp(ctx context.Context, input validate.SignUpEmail) (api.AuthResult, error) {
	_, session, err := adapter.auth.SignUpEmail(ctx, auth.SignUpInput{Name: input.Name, Email: input.Email, Password: input.Password, CallbackURL: input.CallbackURL})
	if err != nil {
		return api.AuthResult{}, authError(err)
	}
	return adapter.signedIn(session), nil
}

// SignIn checks the password and starts a session.
func (adapter Authenticator) SignIn(ctx context.Context, input validate.SignInEmail) (api.AuthResult, error) {
	_, session, err := adapter.auth.SignInEmail(ctx, auth.SignInInput{Email: input.Email, Password: input.Password, CallbackURL: input.CallbackURL})
	if err != nil {
		return api.AuthResult{}, authError(err)
	}
	return adapter.signedIn(session), nil
}

// SignOut ends the session (when there is one) and always clears the
// cookie; a failed revocation is reported after the cookie is cleared.
func (adapter Authenticator) SignOut(ctx context.Context, sessionToken string) (api.AuthResult, error) {
	var err error
	if sessionToken != "" {
		err = adapter.auth.SignOut(ctx, sessionToken)
	}
	result := api.AuthResult{Cookies: capture(adapter.auth.ClearSessionCookie)}
	if err != nil {
		return result, authError(err)
	}
	return result, nil
}

// RequestPasswordReset emails a reset link.
func (adapter Authenticator) RequestPasswordReset(ctx context.Context, input validate.CreatePasswordReset) error {
	return authError(adapter.auth.RequestPasswordReset(ctx, input.Email, input.RedirectTo))
}

// ResetPassword spends a reset token.
func (adapter Authenticator) ResetPassword(ctx context.Context, input validate.ResetPassword) error {
	return authError(adapter.auth.ResetPassword(ctx, input.Token, input.NewPassword))
}

// VerifyEmail spends a verification token; a session unless the email was
// already verified.
func (adapter Authenticator) VerifyEmail(ctx context.Context, input validate.VerifyEmail) (api.AuthResult, error) {
	_, session, err := adapter.auth.VerifyEmail(ctx, input.Token)
	if err != nil {
		return api.AuthResult{}, authError(err)
	}
	return adapter.signedIn(session), nil
}

// SendVerificationEmail sends a fresh verification link.
func (adapter Authenticator) SendVerificationEmail(ctx context.Context, input validate.CreateVerificationEmail) error {
	return authError(adapter.auth.SendVerificationEmail(ctx, input.Email, input.CallbackURL))
}

// CheckCallbackURL reports whether a link's callbackURL may be followed.
func (adapter Authenticator) CheckCallbackURL(callbackURL string) error {
	return authError(adapter.auth.CheckCallbackURL(callbackURL))
}

// ErrorForCode is the translated error for a Better Auth error code.
func (Authenticator) ErrorForCode(code string) error {
	for _, translation := range authErrors {
		if coded, ok := errors.AsType[*auth.Error](translation.from); ok && coded.Code == code {
			return translation.to
		}
	}
	return nil
}

// signedIn is the result for session: its cookie, or no session at all
// (a zero Session: verification pending, already verified).
func (adapter Authenticator) signedIn(session auth.Session) api.AuthResult {
	if session.Token == "" {
		return api.AuthResult{}
	}
	return api.AuthResult{
		SignedIn: true,
		Cookies:  capture(func(writer http.ResponseWriter) { adapter.auth.SetSessionCookie(writer, session) }),
	}
}

// authErrors translate packages/auth's errors into the API's, most
// specific first; anything else passes through (and is a 500).
var authErrors = []struct{ from, to error }{
	{auth.ErrInvalidEmail, apiclient.ErrInvalidEmail},
	{auth.ErrInvalidEmailOrPassword, apiclient.ErrInvalidCredentials},
	{auth.ErrPasswordTooShort, apiclient.ErrPasswordTooShort},
	{auth.ErrPasswordTooLong, apiclient.ErrPasswordTooLong},
	{auth.ErrUserAlreadyExists, apiclient.ErrUserAlreadyExists},
	{auth.ErrEmailNotVerified, apiclient.ErrEmailNotVerified},
	{auth.ErrInvalidToken, apiclient.ErrInvalidToken},
	{auth.ErrTokenExpired, apiclient.ErrTokenExpired},
	{auth.ErrTooManyRequests, apiclient.ErrTooManyRequests},
	{auth.ErrBannedUser, apiclient.ErrBannedUser},
	{auth.ErrEmailTaken, apiclient.ErrEmailTaken},
	{auth.ErrInvalidUser, apiclient.ErrInvalidUser},
	{auth.ErrInvalidCallbackURL, apiclient.ErrInvalidCallbackURL},
}

func authError(err error) error {
	if err == nil {
		return nil
	}
	for _, translation := range authErrors {
		if errors.Is(err, translation.from) {
			return translation.to
		}
	}
	return err
}

// capture runs write against a recorder and returns the cookies it set, as
// data (cookiesOf): auth signs its cookies, the UI sets them (ui.setCookies).
func capture(write func(writer http.ResponseWriter)) []services.Cookie {
	recorder := &headerRecorder{header: http.Header{}}
	write(recorder)
	return cookiesOf((&http.Response{Header: recorder.header}).Cookies())
}

// headerRecorder is the http.ResponseWriter capture writes cookies to; it
// keeps headers and drops anything else.
type headerRecorder struct{ header http.Header }

func (recorder *headerRecorder) Header() http.Header { return recorder.header }

func (*headerRecorder) Write(body []byte) (int, error) { return len(body), nil }

func (*headerRecorder) WriteHeader(int) {}
