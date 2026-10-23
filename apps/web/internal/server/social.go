package server

import (
	"context"
	"errors"
	"net/http"
	"net/url"

	"github.com/jayleaton/go-fast-stack/apps/web/internal/services"
	"github.com/jayleaton/go-fast-stack/apps/web/internal/validate"
	"github.com/jayleaton/go-fast-stack/packages/auth"
)

// SocialAuth adapts *auth.Auth to services.SocialAuth: it converts cookies
// to data and auth's errors to service errors, nothing more.
type SocialAuth struct {
	authn *auth.Auth
}

var _ services.SocialAuth = SocialAuth{}

// NewSocialAuth returns the adapter over authn.
func NewSocialAuth(authn *auth.Auth) SocialAuth { return SocialAuth{authn: authn} }

// Providers lists the configured providers.
func (adapter SocialAuth) Providers() []string { return adapter.authn.SocialProviders() }

// Begin starts the browser flow.
func (adapter SocialAuth) Begin(ctx context.Context, provider, callbackURL string) (services.SocialStart, error) {
	begin, err := adapter.authn.BeginSocialSignIn(ctx, provider, callbackURL)
	switch {
	case errors.Is(err, auth.ErrProviderNotFound):
		return services.SocialStart{}, services.ErrProviderNotFound
	case errors.Is(err, auth.ErrInvalidCallbackURL):
		return services.SocialStart{}, services.ErrInvalidCallbackURL
	case errors.Is(err, auth.ErrTooManyRequests):
		return services.SocialStart{}, services.ErrRateLimited
	case err != nil:
		return services.SocialStart{}, err
	}
	return services.SocialStart{URL: begin.URL, Cookies: cookiesOf(begin.Cookies)}, nil
}

// Complete finishes it from the provider's redirect.
func (adapter SocialAuth) Complete(ctx context.Context, provider string, in validate.FinishSocialSignIn) services.SocialOutcome {
	params := url.Values{}
	for name, value := range in.Params {
		params.Set(name, value)
	}
	outcome := adapter.authn.CompleteSocialSignIn(ctx, auth.SocialCallbackInput{
		Provider: provider, FormPost: in.FormPost, Params: params, Cookies: in.Cookies,
	})
	return services.SocialOutcome{Redirect: outcome.Redirect, Cookies: cookiesOf(outcome.Cookies)}
}

func cookiesOf(cookies []*http.Cookie) []services.Cookie {
	out := make([]services.Cookie, 0, len(cookies))
	for _, cookie := range cookies {
		out = append(out, services.Cookie{Name: cookie.Name, Value: cookie.Value, MaxAge: cookie.MaxAge, Secure: cookie.Secure})
	}
	return out
}
