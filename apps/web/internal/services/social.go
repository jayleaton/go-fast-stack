package services

//go:generate go tool mockgen -source=social.go -destination=mocks/social.go -package=mocks

import (
	"context"

	"github.com/jayleaton/go-fast-stack/apps/web/internal/validate"
)

// ErrProviderNotFound is returned for a sign-in provider that is not
// configured (SocialAuth.Begin also returns ErrInvalidCallbackURL and
// ErrRateLimited).
var ErrProviderNotFound = &Error{Kind: KindNotFound, Message: "That sign-in provider is not available"}

// Cookie is a cookie for the browser, as data: the UI sets it with Path=/,
// HttpOnly and SameSite=Lax. MaxAge is in seconds; negative deletes it.
type Cookie struct {
	Name   string `json:"name"`
	Value  string `json:"value"`
	MaxAge int    `json:"maxAge"`
	Secure bool   `json:"secure"`
}

// SocialStart is a started browser sign-in: set Cookies (the signed state
// that binds the flow to this browser), then send the browser to URL.
type SocialStart struct {
	URL     string   `json:"url"`
	Cookies []Cookie `json:"cookies"`
}

// SocialOutcome is where a finished (or refused) sign-in sends the browser,
// and the cookies to set first: the session, and clearing the state.
type SocialOutcome struct {
	Redirect string   `json:"redirect"`
	Cookies  []Cookie `json:"cookies"`
}

// SocialAuth is the port to Google and Apple sign-in (server.SocialAuth over
// packages/auth). Begin returns ErrProviderNotFound, ErrInvalidCallbackURL
// or ErrRateLimited; Complete never fails: a refusal is a redirect to the
// error page with ?error=<code>.
type SocialAuth interface {
	Providers() []string
	Begin(ctx context.Context, provider, callbackURL string) (SocialStart, error)
	Complete(ctx context.Context, provider string, in validate.FinishSocialSignIn) SocialOutcome
}

// SocialSignInService is Google and Apple sign-in for the web UI's browser
// flow. Its endpoints are public: the person is not signed in yet.
type SocialSignInService struct {
	auth SocialAuth
}

// NewSocialSignInService returns the service over auth.
func NewSocialSignInService(auth SocialAuth) *SocialSignInService {
	return &SocialSignInService{auth: auth}
}

// Providers lists the configured providers ("google", "apple"), never nil.
func (service *SocialSignInService) Providers(_ context.Context) []string {
	providers := service.auth.Providers()
	if providers == nil {
		return []string{}
	}
	return providers
}

// Start begins a sign-in with provider that lands on in.CallbackURL.
func (service *SocialSignInService) Start(ctx context.Context, provider string, in validate.StartSocialSignIn) (SocialStart, error) {
	return service.auth.Begin(ctx, provider, in.CallbackURL)
}

// Finish completes the sign-in from the provider's redirect.
func (service *SocialSignInService) Finish(ctx context.Context, provider string, in validate.FinishSocialSignIn) SocialOutcome {
	return service.auth.Complete(ctx, provider, in)
}
