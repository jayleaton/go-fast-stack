package api_test

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	"go.uber.org/mock/gomock"

	"github.com/jayleaton/go-fast-stack/apps/web/internal/api"
	"github.com/jayleaton/go-fast-stack/apps/web/internal/api/apitest"
	"github.com/jayleaton/go-fast-stack/apps/web/internal/api/mocks"
	"github.com/jayleaton/go-fast-stack/apps/web/internal/services"
	"github.com/jayleaton/go-fast-stack/apps/web/internal/validate"
)

func newSocialRouter(test *testing.T) (*mocks.MockSocialServiceMockRecorder, http.Handler) {
	service := mocks.NewMockSocialService(gomock.NewController(test))
	mux := http.NewServeMux()
	api.NewSocial(service, apitest.Gate()).Routes(mux)
	return service.EXPECT(), mux
}

func socialPath(path, provider string) string {
	return strings.Replace(path, "{provider}", provider, 1)
}

// The social endpoints are public: they answer signed out and signed in
// alike, reject invalid input with 400, map the service's errors, and hand
// cookies back as data.
func TestSocialRoutes(test *testing.T) {
	test.Parallel()
	started := services.SocialStart{URL: "https://accounts.example/authorize?state=s", Cookies: []services.Cookie{{Name: "better-auth.oauth_state", Value: "signed", MaxAge: 300}}}
	outcome := services.SocialOutcome{Redirect: "/dashboard", Cookies: []services.Cookie{{Name: "better-auth.session_token", Value: "token", MaxAge: 604800}}}
	start := socialPath(r.SocialStart, "google")
	finish := socialPath(r.SocialFinish, "google")
	cases := map[string]struct {
		as           apitest.Session
		method, path string
		body         string
		expect       func(service *mocks.MockSocialServiceMockRecorder)
		status       int
		check        func(test *testing.T, response apitest.Response)
	}{
		"providers signed out": {
			as: apitest.Anonymous, method: "GET", path: r.SocialProviders, status: http.StatusOK,
			expect: func(service *mocks.MockSocialServiceMockRecorder) {
				service.Providers(anyCtx).Return([]string{"google"})
			},
			check: func(test *testing.T, response apitest.Response) {
				if got := apitest.Data[api.SocialProviders](test, response); len(got.Providers) != 1 || got.Providers[0] != "google" {
					test.Errorf("providers = %+v", got)
				}
			},
		},
		"providers signed in": {
			as: apitest.AsUser(ada), method: "GET", path: r.SocialProviders, status: http.StatusOK,
			expect: func(service *mocks.MockSocialServiceMockRecorder) { service.Providers(anyCtx).Return([]string{}) },
		},
		"start": {
			as: apitest.Anonymous, method: "POST", path: start, body: `{"callbackURL":"/dashboard"}`, status: http.StatusOK,
			expect: func(service *mocks.MockSocialServiceMockRecorder) {
				service.Start(anyCtx, "google", validate.StartSocialSignIn{CallbackURL: "/dashboard"}).Return(started, nil)
			},
			check: func(test *testing.T, response apitest.Response) {
				if got := apitest.Data[services.SocialStart](test, response); got.URL != started.URL || len(got.Cookies) != 1 || got.Cookies[0].MaxAge != 300 {
					test.Errorf("start = %+v", got)
				}
			},
		},
		"start invalid": {
			as: apitest.Anonymous, method: "POST", path: start, body: `{"callbackURL":"/` + strings.Repeat("x", 2048) + `"}`, status: http.StatusBadRequest,
			expect: func(*mocks.MockSocialServiceMockRecorder) {},
		},
		"start unknown provider": {
			as: apitest.Anonymous, method: "POST", path: socialPath(r.SocialStart, "github"), body: `{}`, status: http.StatusNotFound,
			expect: func(service *mocks.MockSocialServiceMockRecorder) {
				service.Start(anyCtx, "github", gomock.Any()).Return(services.SocialStart{}, services.ErrProviderNotFound)
			},
		},
		"start rate limited": {
			as: apitest.Anonymous, method: "POST", path: start, body: `{}`, status: http.StatusTooManyRequests,
			expect: func(service *mocks.MockSocialServiceMockRecorder) {
				service.Start(anyCtx, "google", gomock.Any()).Return(services.SocialStart{}, services.ErrRateLimited)
			},
		},
		"finish": {
			as: apitest.Anonymous, method: "POST", path: finish, body: `{"params":{"code":"c","state":"s"},"cookies":{"better-auth.oauth_state":"signed"}}`, status: http.StatusOK,
			expect: func(service *mocks.MockSocialServiceMockRecorder) {
				service.Finish(anyCtx, "google", validate.FinishSocialSignIn{
					Params: map[string]string{"code": "c", "state": "s"}, Cookies: map[string]string{"better-auth.oauth_state": "signed"},
				}).Return(outcome)
			},
			check: func(test *testing.T, response apitest.Response) {
				if got := apitest.Data[services.SocialOutcome](test, response); got.Redirect != "/dashboard" || len(got.Cookies) != 1 {
					test.Errorf("finish = %+v", got)
				}
			},
		},
		"finish invalid": {
			as: apitest.Anonymous, method: "POST", path: finish, body: `{"params":{"user":"` + strings.Repeat("x", 9000) + `"}}`, status: http.StatusBadRequest,
			expect: func(*mocks.MockSocialServiceMockRecorder) {},
		},
	}
	for name, testCase := range cases {
		test.Run(name, func(test *testing.T) {
			test.Parallel()
			service, router := newSocialRouter(test)
			testCase.expect(service)
			response := apitest.Do(test, router, testCase.as, testCase.method, testCase.path, testCase.body)
			if response.Status != testCase.status {
				test.Fatalf("status = %d, want %d (error %v, details %v)", response.Status, testCase.status, response.Error, response.Details)
			}
			if testCase.check != nil {
				testCase.check(test, response)
			}
		})
	}
}

// The UI's in-process client reaches the same endpoints, and without them
// (WithSocial not called) reports no providers instead of failing.
func TestClientSocial(test *testing.T) {
	test.Parallel()
	service := mocks.NewMockSocialService(gomock.NewController(test))
	expect := service.EXPECT()
	expect.Providers(anyCtx).Return([]string{"google", "apple"})
	expect.Start(anyCtx, "apple", validate.StartSocialSignIn{CallbackURL: "/dashboard"}).Return(services.SocialStart{URL: "https://apple.example"}, nil)
	expect.Finish(anyCtx, "apple", gomock.Any()).Return(services.SocialOutcome{Redirect: "/dashboard"})
	usersService := mocks.NewMockUserService(gomock.NewController(test))
	client := api.NewClient(api.NewUsers(usersService, apitest.Gate())).WithSocial(api.NewSocial(service, apitest.Gate()))
	ctx := signedIn(test, apitest.Anonymous)

	if providers, err := client.SocialProviders(ctx); err != nil || len(providers) != 2 {
		test.Errorf("SocialProviders = %v, %v", providers, err)
	}
	if start, err := client.StartSocialSignIn(ctx, "apple", validate.StartSocialSignIn{CallbackURL: "/dashboard"}); err != nil || start.URL != "https://apple.example" {
		test.Errorf("StartSocialSignIn = %+v, %v", start, err)
	}
	if outcome, err := client.FinishSocialSignIn(ctx, "apple", validate.FinishSocialSignIn{Params: map[string]string{"code": "c"}}); err != nil || outcome.Redirect != "/dashboard" {
		test.Errorf("FinishSocialSignIn = %+v, %v", outcome, err)
	}

	bare := api.NewClient(api.NewUsers(usersService, apitest.Gate()))
	if providers, err := bare.SocialProviders(context.Background()); err != nil || len(providers) != 0 {
		test.Errorf("no social: providers = %v, %v", providers, err)
	}
	if _, err := bare.StartSocialSignIn(context.Background(), "google", validate.StartSocialSignIn{}); !errors.Is(err, services.ErrProviderNotFound) {
		test.Errorf("no social: start err = %v", err)
	}
	if _, err := bare.FinishSocialSignIn(context.Background(), "google", validate.FinishSocialSignIn{}); !errors.Is(err, services.ErrProviderNotFound) {
		test.Errorf("no social: finish err = %v", err)
	}
}
