package api_test

import (
	"errors"
	"testing"

	"go.uber.org/mock/gomock"

	"github.com/jayleaton/go-fast-stack/apps/admin/internal/access"
	"github.com/jayleaton/go-fast-stack/apps/admin/internal/api"
	"github.com/jayleaton/go-fast-stack/apps/admin/internal/api/apitest"
	"github.com/jayleaton/go-fast-stack/apps/admin/internal/api/mocks"
	"github.com/jayleaton/go-fast-stack/apps/admin/internal/apiclient"
	"github.com/jayleaton/go-fast-stack/apps/admin/internal/validate"
)

// newAuthClient is the in-process client with the sign-in endpoints over a
// mocked Authenticator.
func newAuthClient(test *testing.T) (*mocks.MockAuthenticatorMockRecorder, *api.Client) {
	ctrl := gomock.NewController(test)
	authenticator := mocks.NewMockAuthenticator(ctrl)
	gate := apitest.Gate()
	client := api.NewClient(
		api.NewUsers(mocks.NewMockUserService(ctrl), gate), api.NewAdmins(mocks.NewMockAdminService(ctrl), gate),
		api.NewAudit(mocks.NewMockAuditService(ctrl), gate), api.NewAnalytics(mocks.NewMockAnalyticsService(ctrl), gate),
		api.NewSessions(authenticator, gate))
	return authenticator.EXPECT(), client
}

// TestSignInFlows: the admin sign-in steps are endpoints. Public steps run
// signed out, a two-factor challenge comes back as the error with its
// cookie, and enrolling a second factor needs a session and acts on the
// caller only.
func TestSignInFlows(test *testing.T) {
	test.Parallel()
	session := []apiclient.Cookie{{Name: "admin.session_token", Value: "signed", MaxAge: 3600}}

	test.Run("sign in trims the email and returns the session cookie", func(test *testing.T) {
		test.Parallel()
		auth, client := newAuthClient(test)
		auth.SignIn(anyCtx, validate.SignIn{Email: "grace@example.com", Password: "pw"}).Return(api.AuthResult{SignedIn: true, Cookies: session}, nil)
		got, err := client.SignIn(signedIn(test, apitest.Anonymous), validate.SignIn{Email: " grace@example.com ", Password: "pw"})
		if err != nil || !got.SignedIn || len(got.Cookies) != 1 {
			test.Fatalf("got %+v, %v", got, err)
		}
	})
	test.Run("a two-factor challenge is the error, with its cookie", func(test *testing.T) {
		test.Parallel()
		auth, client := newAuthClient(test)
		challenge := &apiclient.TwoFactorChallenge{Methods: []string{"totp"}, Cookies: []apiclient.Cookie{{Name: "admin.two_factor", Value: "c"}}}
		auth.SignIn(anyCtx, gomock.Any()).Return(api.AuthResult{}, challenge)
		_, err := client.SignIn(signedIn(test, apitest.Anonymous), validate.SignIn{Email: "grace@example.com", Password: "pw"})
		got, ok := errors.AsType[*apiclient.TwoFactorChallenge](err)
		if !ok || len(got.Cookies) != 1 {
			test.Fatalf("err = %v, want the challenge", err)
		}
	})
	test.Run("set password, reset, verify and setup are public", func(test *testing.T) {
		test.Parallel()
		auth, client := newAuthClient(test)
		ctx := signedIn(test, apitest.Anonymous)
		auth.SetPassword(anyCtx, validate.SetPassword{Token: "tok", Password: "pw"}).Return(api.AuthResult{SignedIn: true, Cookies: session}, nil)
		auth.RequestPasswordReset(anyCtx, validate.CreatePasswordReset{Email: "grace@example.com"}).Return(nil)
		auth.VerifyTwoFactor(anyCtx, validate.VerifyTwoFactor{Code: "123456", Method: validate.MethodTOTP, TrustDevice: true}).Return(api.AuthResult{SignedIn: true, Cookies: session}, nil)
		auth.StartTwoFactorSetup(anyCtx).Return(api.TOTPSetup{Secret: "s"}, nil)
		if _, err := client.SetPassword(ctx, validate.SetPassword{Token: "tok", Password: "pw"}); err != nil {
			test.Error(err)
		}
		if err := client.RequestPasswordReset(ctx, validate.CreatePasswordReset{Email: " grace@example.com"}); err != nil {
			test.Error(err)
		}
		if _, err := client.VerifyTwoFactor(ctx, validate.VerifyTwoFactor{Code: " 123456 ", Method: validate.MethodTOTP, TrustDevice: true}); err != nil {
			test.Error(err)
		}
		if setup, err := client.StartTwoFactorSetup(ctx); err != nil || setup.Secret != "s" {
			test.Errorf("setup = %+v, %v", setup, err)
		}
	})
	test.Run("enrolment acts on the caller and needs a session", func(test *testing.T) {
		test.Parallel()
		auth, client := newAuthClient(test)
		auth.EnableTwoFactor(anyCtx, grace.ID, validate.EnableTwoFactor{Password: "pw"}).Return(api.TOTPSetup{Secret: "s"}, nil)
		auth.ConfirmTwoFactor(anyCtx, grace.ID, validate.ConfirmTwoFactor{Code: "123456"}).Return(nil)
		ctx := signedIn(test, apitest.AsAdmin(grace))
		if _, err := client.EnableTwoFactor(ctx, validate.EnableTwoFactor{Password: "pw"}); err != nil {
			test.Error(err)
		}
		if err := client.ConfirmTwoFactor(ctx, validate.ConfirmTwoFactor{Code: " 123456"}); err != nil {
			test.Error(err)
		}
		anonymous := signedIn(test, apitest.Anonymous)
		if _, err := client.EnableTwoFactor(anonymous, validate.EnableTwoFactor{Password: "pw"}); !errors.Is(err, access.ErrSignedOut) {
			test.Errorf("signed out enable: err = %v", err)
		}
		if err := client.ConfirmTwoFactor(anonymous, validate.ConfirmTwoFactor{Code: "1"}); !errors.Is(err, access.ErrSignedOut) {
			test.Errorf("signed out confirm: err = %v", err)
		}
	})
	test.Run("sign out ends the caller's own session", func(test *testing.T) {
		test.Parallel()
		auth, client := newAuthClient(test)
		cleared := []apiclient.Cookie{{Name: "admin.session_token", MaxAge: -1}}
		auth.SignOut(anyCtx, apitest.Caller(grace).Session.Token).Return(api.AuthResult{Cookies: cleared}, nil)
		got, err := client.SignOut(signedIn(test, apitest.AsAdmin(grace)))
		if err != nil || len(got.Cookies) != 1 || got.Cookies[0].MaxAge >= 0 {
			test.Fatalf("got %+v, %v", got, err)
		}
	})
}
