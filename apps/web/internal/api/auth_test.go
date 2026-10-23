package api_test

import (
	"context"
	"errors"
	"testing"

	"go.uber.org/mock/gomock"

	"github.com/jayleaton/go-fast-stack/apps/web/internal/api"
	"github.com/jayleaton/go-fast-stack/apps/web/internal/api/apitest"
	"github.com/jayleaton/go-fast-stack/apps/web/internal/api/mocks"
	"github.com/jayleaton/go-fast-stack/apps/web/internal/apiclient"
	"github.com/jayleaton/go-fast-stack/apps/web/internal/validate"
)

// sessionCookie is what a signed-in result carries for the UI to set.
var sessionCookie = apiclient.Cookie{Name: "better-auth.session_token", Value: "signed", MaxAge: 3600}

// newAuthClient is the in-process client with the sign-in endpoints over a
// mocked Authenticator.
func newAuthClient(test *testing.T) (*mocks.MockAuthenticatorMockRecorder, *api.Client) {
	ctrl := gomock.NewController(test)
	authenticator := mocks.NewMockAuthenticator(ctrl)
	gate := apitest.Gate()
	client := api.NewClient(api.NewUsers(mocks.NewMockUserService(ctrl), gate), api.NewSessions(authenticator, gate))
	return authenticator.EXPECT(), client
}

// TestSignInFlows: each sign-in flow is an endpoint the UI reaches through
// the client. Inputs arrive validated (trimmed), results carry the cookies
// for the UI to set, and auth's errors come back as they are for the UI to
// translate.
func TestSignInFlows(test *testing.T) {
	test.Parallel()
	withSession := api.AuthResult{SignedIn: true, Cookies: []apiclient.Cookie{sessionCookie}}
	cases := map[string]struct {
		as     apitest.Session
		expect func(auth *mocks.MockAuthenticatorMockRecorder)
		call   func(ctx context.Context, client *api.Client) (api.AuthResult, error)
		want   api.AuthResult
		err    error
	}{
		"sign up trims the name and email": {
			apitest.Anonymous,
			func(auth *mocks.MockAuthenticatorMockRecorder) {
				auth.SignUp(anyCtx, validate.SignUpEmail{Name: "Ada", Email: "ada@example.com", Password: "password123", CallbackURL: "/verify-email/success"}).Return(api.AuthResult{}, nil)
			},
			func(ctx context.Context, client *api.Client) (api.AuthResult, error) {
				return client.SignUp(ctx, validate.SignUpEmail{Name: " Ada ", Email: " ada@example.com ", Password: "password123", CallbackURL: "/verify-email/success"})
			},
			api.AuthResult{}, nil,
		},
		"sign in returns the session cookie": {
			apitest.Anonymous,
			func(auth *mocks.MockAuthenticatorMockRecorder) {
				auth.SignIn(anyCtx, validate.SignInEmail{Email: "ada@example.com", Password: "password123"}).Return(withSession, nil)
			},
			func(ctx context.Context, client *api.Client) (api.AuthResult, error) {
				return client.SignIn(ctx, validate.SignInEmail{Email: "ada@example.com ", Password: "password123"})
			},
			withSession, nil,
		},
		"sign in passes auth's error on": {
			apitest.Anonymous,
			func(auth *mocks.MockAuthenticatorMockRecorder) {
				auth.SignIn(anyCtx, gomock.Any()).Return(api.AuthResult{}, apiclient.ErrInvalidCredentials)
			},
			func(ctx context.Context, client *api.Client) (api.AuthResult, error) {
				return client.SignIn(ctx, validate.SignInEmail{Email: "ada@example.com", Password: "wrong"})
			},
			api.AuthResult{}, apiclient.ErrInvalidCredentials,
		},
		"sign out ends the caller's own session": {
			apitest.AsUser(ada),
			func(auth *mocks.MockAuthenticatorMockRecorder) {
				auth.SignOut(anyCtx, apitest.Caller(ada).Session.Token).Return(api.AuthResult{Cookies: []apiclient.Cookie{{Name: sessionCookie.Name, MaxAge: -1}}}, nil)
			},
			func(ctx context.Context, client *api.Client) (api.AuthResult, error) { return client.SignOut(ctx) },
			api.AuthResult{Cookies: []apiclient.Cookie{{Name: sessionCookie.Name, MaxAge: -1}}}, nil,
		},
		"sign out signed out still clears the cookie": {
			apitest.Anonymous,
			func(auth *mocks.MockAuthenticatorMockRecorder) {
				auth.SignOut(anyCtx, "").Return(api.AuthResult{Cookies: []apiclient.Cookie{{Name: sessionCookie.Name, MaxAge: -1}}}, nil)
			},
			func(ctx context.Context, client *api.Client) (api.AuthResult, error) { return client.SignOut(ctx) },
			api.AuthResult{Cookies: []apiclient.Cookie{{Name: sessionCookie.Name, MaxAge: -1}}}, nil,
		},
		"verify email signs in": {
			apitest.Anonymous,
			func(auth *mocks.MockAuthenticatorMockRecorder) {
				auth.VerifyEmail(anyCtx, validate.VerifyEmail{Token: "tok"}).Return(withSession, nil)
			},
			func(ctx context.Context, client *api.Client) (api.AuthResult, error) {
				return client.VerifyEmail(ctx, validate.VerifyEmail{Token: "tok"})
			},
			withSession, nil,
		},
		"request password reset": {
			apitest.Anonymous,
			func(auth *mocks.MockAuthenticatorMockRecorder) {
				auth.RequestPasswordReset(anyCtx, validate.CreatePasswordReset{Email: "ada@example.com", RedirectTo: "/reset-password"}).Return(nil)
			},
			func(ctx context.Context, client *api.Client) (api.AuthResult, error) {
				return api.AuthResult{}, client.RequestPasswordReset(ctx, validate.CreatePasswordReset{Email: " ada@example.com", RedirectTo: "/reset-password"})
			},
			api.AuthResult{}, nil,
		},
		"reset password passes an expired token on": {
			apitest.Anonymous,
			func(auth *mocks.MockAuthenticatorMockRecorder) {
				auth.ResetPassword(anyCtx, validate.ResetPassword{Token: "old", NewPassword: "password456"}).Return(apiclient.ErrTokenExpired)
			},
			func(ctx context.Context, client *api.Client) (api.AuthResult, error) {
				return api.AuthResult{}, client.ResetPassword(ctx, validate.ResetPassword{Token: "old", NewPassword: "password456"})
			},
			api.AuthResult{}, apiclient.ErrTokenExpired,
		},
		"send verification email": {
			apitest.Anonymous,
			func(auth *mocks.MockAuthenticatorMockRecorder) {
				auth.SendVerificationEmail(anyCtx, validate.CreateVerificationEmail{Email: "ada@example.com", CallbackURL: "/x"}).Return(nil)
			},
			func(ctx context.Context, client *api.Client) (api.AuthResult, error) {
				return api.AuthResult{}, client.SendVerificationEmail(ctx, validate.CreateVerificationEmail{Email: "ada@example.com ", CallbackURL: "/x"})
			},
			api.AuthResult{}, nil,
		},
		"check callback URL": {
			apitest.Anonymous,
			func(auth *mocks.MockAuthenticatorMockRecorder) {
				auth.CheckCallbackURL("https://evil.example").Return(apiclient.ErrInvalidCallbackURL)
			},
			func(ctx context.Context, client *api.Client) (api.AuthResult, error) {
				return api.AuthResult{}, client.CheckCallbackURL(ctx, "https://evil.example")
			},
			api.AuthResult{}, apiclient.ErrInvalidCallbackURL,
		},
		"error for an emailed link's code": {
			apitest.Anonymous,
			func(auth *mocks.MockAuthenticatorMockRecorder) {
				auth.ErrorForCode("INVALID_USER").Return(apiclient.ErrInvalidUser)
			},
			func(_ context.Context, client *api.Client) (api.AuthResult, error) {
				return api.AuthResult{}, client.AuthErrorForCode("INVALID_USER")
			},
			api.AuthResult{}, apiclient.ErrInvalidUser,
		},
	}
	for name, testCase := range cases {
		test.Run(name, func(test *testing.T) {
			test.Parallel()
			auth, client := newAuthClient(test)
			testCase.expect(auth)
			got, err := testCase.call(signedIn(test, testCase.as), client)
			if !errors.Is(err, testCase.err) || (testCase.err == nil && err != nil) {
				test.Fatalf("err = %v, want %v", err, testCase.err)
			}
			if got.SignedIn != testCase.want.SignedIn || len(got.Cookies) != len(testCase.want.Cookies) {
				test.Errorf("result = %+v, want %+v", got, testCase.want)
			}
		})
	}
}
