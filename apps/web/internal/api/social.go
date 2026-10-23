package api

//go:generate go tool mockgen -source=social.go -destination=mocks/social.go -package=mocks

import (
	"context"
	"net/http"

	"github.com/jayleaton/go-fast-stack/apps/web/internal/access"
	"github.com/jayleaton/go-fast-stack/apps/web/internal/apiclient"
	"github.com/jayleaton/go-fast-stack/apps/web/internal/config"
	"github.com/jayleaton/go-fast-stack/apps/web/internal/services"
	"github.com/jayleaton/go-fast-stack/apps/web/internal/validate"
)

// SocialService is the port these endpoints call
// (*services.SocialSignInService).
type SocialService interface {
	Providers(ctx context.Context) []string
	Start(ctx context.Context, provider string, in validate.StartSocialSignIn) (services.SocialStart, error)
	Finish(ctx context.Context, provider string, in validate.FinishSocialSignIn) services.SocialOutcome
}

var _ SocialService = (*services.SocialSignInService)(nil)

// SocialProviders is the GET /auth/social/providers response.
type SocialProviders = apiclient.SocialProviders

// Social serves Google and Apple sign-in's browser flow under
// /api/core/v1/auth/social: the providers to show, and the two halves of a
// sign-in, which hand the cookies to set back as data. Every endpoint is
// public (nobody is signed in yet). The mobile app uses packages/auth's
// handoff endpoints instead (POST /api/auth/sign-in/social, /handoff).
type Social struct {
	svc  SocialService
	gate *access.Gate

	providers Endpoint[NoInput, SocialProviders]
	start     Endpoint[validate.StartSocialSignIn, services.SocialStart]
	finish    Endpoint[validate.FinishSocialSignIn, services.SocialOutcome]
}

// NewSocial returns the social sign-in endpoints.
func NewSocial(svc SocialService, gate *access.Gate) *Social {
	social := &Social{svc: svc, gate: gate}
	routes := config.APIRoutes
	social.providers = Endpoint[NoInput, SocialProviders]{"GET", routes.SocialProviders, access.Public, social.list}
	social.start = Endpoint[validate.StartSocialSignIn, services.SocialStart]{"POST", routes.SocialStart, access.Public, social.begin}
	social.finish = Endpoint[validate.FinishSocialSignIn, services.SocialOutcome]{"POST", routes.SocialFinish, access.Public, social.complete}
	return social
}

// Routes registers the endpoints on mux.
func (social *Social) Routes(mux *http.ServeMux) {
	mux.Handle(social.providers.Pattern(), social.providers.Serve(social.gate))
	mux.Handle(social.start.Pattern(), social.start.Serve(social.gate))
	mux.Handle(social.finish.Pattern(), social.finish.Serve(social.gate))
}

func (social *Social) list(request *http.Request, _ access.Caller, _ NoInput) (SocialProviders, error) {
	return SocialProviders{Providers: social.svc.Providers(request.Context())}, nil
}

func (social *Social) begin(request *http.Request, _ access.Caller, in validate.StartSocialSignIn) (services.SocialStart, error) {
	return social.svc.Start(request.Context(), request.PathValue("provider"), in)
}

func (social *Social) complete(request *http.Request, _ access.Caller, in validate.FinishSocialSignIn) (services.SocialOutcome, error) {
	return social.svc.Finish(request.Context(), request.PathValue("provider"), in), nil
}
