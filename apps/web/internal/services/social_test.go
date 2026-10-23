package services_test

import (
	"testing"

	"go.uber.org/mock/gomock"

	"github.com/jayleaton/go-fast-stack/apps/web/internal/services"
	"github.com/jayleaton/go-fast-stack/apps/web/internal/services/mocks"
	"github.com/jayleaton/go-fast-stack/apps/web/internal/validate"
)

func newSocialService(test *testing.T) (*services.SocialSignInService, *mocks.MockSocialAuthMockRecorder) {
	port := mocks.NewMockSocialAuth(gomock.NewController(test))
	return services.NewSocialSignInService(port), port.EXPECT()
}

func TestSocialProviders(test *testing.T) {
	test.Parallel()
	for name, listed := range map[string][]string{"none configured": nil, "both": {"google", "apple"}} {
		test.Run(name, func(test *testing.T) {
			test.Parallel()
			service, port := newSocialService(test)
			port.Providers().Return(listed)
			got := service.Providers(ctx)
			if got == nil || len(got) != len(listed) {
				test.Fatalf("Providers = %#v, want %v and never nil", got, listed)
			}
		})
	}
}

// Start and Finish hand the provider and input to the port and return what
// it says, errors included (the port maps auth's errors to these values).
func TestSocialStartAndFinish(test *testing.T) {
	test.Parallel()
	started := services.SocialStart{URL: "https://accounts.example/authorize", Cookies: []services.Cookie{{Name: "state", Value: "signed"}}}
	for name, portErr := range map[string]error{"ok": nil, "unknown provider": services.ErrProviderNotFound, "rate limited": services.ErrRateLimited} {
		test.Run("start "+name, func(test *testing.T) {
			test.Parallel()
			service, port := newSocialService(test)
			port.Begin(ctx, "google", "/dashboard").Return(started, portErr)
			got, err := service.Start(ctx, "google", validate.StartSocialSignIn{CallbackURL: "/dashboard"})
			if err != portErr || (portErr == nil && got.URL != started.URL) { //nolint:errorlint // the port's own value must pass through unchanged
				test.Fatalf("Start = %+v, %v", got, err)
			}
		})
	}
	service, port := newSocialService(test)
	input := validate.FinishSocialSignIn{Params: map[string]string{"code": "c", "state": "s"}, Cookies: map[string]string{"state": "signed"}}
	outcome := services.SocialOutcome{Redirect: "/dashboard", Cookies: []services.Cookie{{Name: "session", Value: "token"}}}
	port.Complete(ctx, "apple", input).Return(outcome)
	if got := service.Finish(ctx, "apple", input); got.Redirect != outcome.Redirect || len(got.Cookies) != 1 {
		test.Fatalf("Finish = %+v", got)
	}
}
