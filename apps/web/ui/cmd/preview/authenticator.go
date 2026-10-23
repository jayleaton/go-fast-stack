package main

import (
	"context"
	"errors"
	"net/http"

	"github.com/jayleaton/go-fast-stack/apps/web/internal/api"
	"github.com/jayleaton/go-fast-stack/apps/web/internal/apiclient"
	"github.com/jayleaton/go-fast-stack/apps/web/internal/validate"
	"github.com/jayleaton/go-fast-stack/packages/auth"
)

// authenticator is the preview's api.Authenticator over the fake Auth, the
// counterpart of server.Authenticator: cookies captured into the result,
// auth errors translated to the apiclient values.
type authenticator struct{ fake Auth }

var _ api.Authenticator = authenticator{}

func (adapter authenticator) SignUp(ctx context.Context, input validate.SignUpEmail) (api.AuthResult, error) {
	_, session, err := adapter.fake.SignUpEmail(ctx, auth.SignUpInput{Name: input.Name, Email: input.Email, Password: input.Password, CallbackURL: input.CallbackURL})
	return adapter.result(session), translate(err)
}

func (adapter authenticator) SignIn(ctx context.Context, input validate.SignInEmail) (api.AuthResult, error) {
	_, session, err := adapter.fake.SignInEmail(ctx, auth.SignInInput{Email: input.Email, Password: input.Password, CallbackURL: input.CallbackURL})
	return adapter.result(session), translate(err)
}

func (adapter authenticator) SignOut(ctx context.Context, sessionToken string) (api.AuthResult, error) {
	var err error
	if sessionToken != "" {
		err = adapter.fake.SignOut(ctx, sessionToken)
	}
	return api.AuthResult{Cookies: capture(adapter.fake.ClearSessionCookie)}, translate(err)
}

func (adapter authenticator) RequestPasswordReset(ctx context.Context, input validate.CreatePasswordReset) error {
	return translate(adapter.fake.RequestPasswordReset(ctx, input.Email, input.RedirectTo))
}

func (adapter authenticator) ResetPassword(ctx context.Context, input validate.ResetPassword) error {
	return translate(adapter.fake.ResetPassword(ctx, input.Token, input.NewPassword))
}

func (adapter authenticator) VerifyEmail(ctx context.Context, input validate.VerifyEmail) (api.AuthResult, error) {
	_, session, err := adapter.fake.VerifyEmail(ctx, input.Token)
	return adapter.result(session), translate(err)
}

func (adapter authenticator) SendVerificationEmail(ctx context.Context, input validate.CreateVerificationEmail) error {
	return translate(adapter.fake.SendVerificationEmail(ctx, input.Email, input.CallbackURL))
}

func (authenticator) CheckCallbackURL(string) error { return nil }

func (authenticator) ErrorForCode(string) error { return nil }

func (adapter authenticator) result(session auth.Session) api.AuthResult {
	if session.Token == "" {
		return api.AuthResult{}
	}
	return api.AuthResult{SignedIn: true, Cookies: capture(func(writer http.ResponseWriter) { adapter.fake.SetSessionCookie(writer, session) })}
}

// translations are the fake's auth errors as the API returns them.
var translations = []struct{ from, to error }{
	{auth.ErrInvalidEmail, apiclient.ErrInvalidEmail},
	{auth.ErrInvalidEmailOrPassword, apiclient.ErrInvalidCredentials},
	{auth.ErrPasswordTooShort, apiclient.ErrPasswordTooShort},
	{auth.ErrPasswordTooLong, apiclient.ErrPasswordTooLong},
	{auth.ErrUserAlreadyExists, apiclient.ErrUserAlreadyExists},
	{auth.ErrEmailNotVerified, apiclient.ErrEmailNotVerified},
	{auth.ErrInvalidToken, apiclient.ErrInvalidToken},
}

func translate(err error) error {
	for _, translation := range translations {
		if errors.Is(err, translation.from) {
			return translation.to
		}
	}
	return err
}

func capture(write func(writer http.ResponseWriter)) []apiclient.Cookie {
	recorder := &headerRecorder{header: http.Header{}}
	write(recorder)
	var cookies []apiclient.Cookie
	for _, cookie := range (&http.Response{Header: recorder.header}).Cookies() {
		cookies = append(cookies, apiclient.Cookie{Name: cookie.Name, Value: cookie.Value, MaxAge: cookie.MaxAge, Secure: cookie.Secure})
	}
	return cookies
}

type headerRecorder struct{ header http.Header }

func (recorder *headerRecorder) Header() http.Header { return recorder.header }

func (*headerRecorder) Write(body []byte) (int, error) { return len(body), nil }

func (*headerRecorder) WriteHeader(int) {}
