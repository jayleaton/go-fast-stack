// Package apiclient is the JSON API as the UI calls it: the Client
// interface, one method per endpoint, and the types and errors those
// methods take and return. It is the only app package apps/web/ui may
// import for data (depguard): the UI never reaches services, repositories
// or the database, only the API, so every read and write passes through
// the API's access gate, validation and error mapping.
//
// api.Client is the implementation: it runs each api.Endpoint in-process
// (no network hop; browsers still only ever talk to the UI). A client over
// HTTP for a split deploy would implement the same interface against the
// same endpoints and map the envelope back to these errors.
//
// The types are aliases, so the UI and the API share one definition.
package apiclient

import (
	"context"

	"github.com/jayleaton/go-fast-stack/apps/web/internal/access"
	"github.com/jayleaton/go-fast-stack/apps/web/internal/services"
	"github.com/jayleaton/go-fast-stack/apps/web/internal/validate"
)

// Client is the signed-in user's account API (/api/core/v1/users/me...).
// The caller is the request the ctx belongs to; the API's access policy
// decides whether they may call (access.ErrSignedOut, access.ErrDenied).
type Client interface {
	Me(ctx context.Context) (CurrentUser, error)
	UpdateMe(ctx context.Context, input UpdateUserSettings) (UserUpdated, error)
	RequestEmailChange(ctx context.Context, input UpdateUserEmail) error
	ChangePassword(ctx context.Context, input UpdateUserPassword) error
	ListSessions(ctx context.Context) ([]Session, error)
	RevokeSession(ctx context.Context, sessionID string) error
	RevokeOtherSessions(ctx context.Context) error
	DeleteMe(ctx context.Context) error
	DataSummary(ctx context.Context) (UserDataSummary, error)

	// Google and Apple sign-in, the browser flow (public).
	SocialProviders(ctx context.Context) ([]string, error)
	StartSocialSignIn(ctx context.Context, provider string, input StartSocialSignIn) (SocialStart, error)
	FinishSocialSignIn(ctx context.Context, provider string, input FinishSocialSignIn) (SocialOutcome, error)
}

// Responses.
type (
	// CurrentUser is the GET /users/me response.
	CurrentUser struct {
		User    User           `json:"user"`
		Session access.Session `json:"session"`
	}
	// UserUpdated is the PATCH /users/me response.
	UserUpdated struct {
		Success bool `json:"success"`
		User    User `json:"user"`
	}
	// SocialProviders is the GET /auth/social/providers response.
	SocialProviders struct {
		Providers []string `json:"providers"`
	}
)

// Data the API returns.
type (
	User            = services.User
	Session         = services.Session // one of the user's sessions (ListSessions)
	UserDataSummary = services.UserDataSummary
	// SocialStart and SocialOutcome carry the cookies the UI sets as data.
	SocialStart   = services.SocialStart
	SocialOutcome = services.SocialOutcome
	Cookie        = services.Cookie
)

// Inputs, validated by the API (Validate runs there, not in the UI).
type (
	UpdateUserSettings  = validate.UpdateUserSettings
	UpdateUserEmail     = validate.UpdateUserEmail
	UpdateUserPassword  = validate.UpdateUserPassword
	Nullable[Value any] = validate.Nullable[Value]
	ValidationError     = validate.Error
	StartSocialSignIn   = validate.StartSocialSignIn
	FinishSocialSignIn  = validate.FinishSocialSignIn
)

// Password limits the API enforces, for form hints.
const (
	MinPasswordLength = services.MinPasswordLength
	MaxPasswordLength = services.MaxPasswordLength
)

// Errors the API returns, matched with errors.Is. The kind sentinels
// (ErrNotFound...) match every error of their kind.
var (
	ErrInvalidPassword = services.ErrInvalidPassword
	ErrPasswordPolicy  = services.ErrPasswordPolicy
	ErrSamePassword    = services.ErrSamePassword
	ErrNoPassword      = services.ErrNoPassword
	ErrSameEmail       = services.ErrSameEmail
	ErrEmailTaken      = services.ErrEmailTaken
	ErrSessionNotFresh = services.ErrSessionNotFresh
	ErrSessionNotFound = services.ErrSessionNotFound
	ErrCurrentSession  = services.ErrCurrentSession
	ErrUserNotFound    = services.ErrUserNotFound
	ErrRateLimited     = services.ErrRateLimited
	// Social sign-in.
	ErrProviderNotFound   = services.ErrProviderNotFound
	ErrInvalidCallbackURL = services.ErrInvalidCallbackURL
	ErrValidation         = services.ErrValidation
	ErrForbidden          = services.ErrForbidden
	ErrNotFound           = services.ErrNotFound
	ErrConflict           = services.ErrConflict
)

// Route access, as the UI declares it on its own pages (the same policies
// and gate as the API's endpoints).
type (
	Caller        = access.Caller
	CallerSession = access.Session
	Policy        = access.Policy
	Gate          = access.Gate
)

// The shipped policies and access errors (docs/route-access.md).
var (
	Public       = access.Public
	SignedIn     = access.SignedIn
	ErrSignedOut = access.ErrSignedOut
	ErrDenied    = access.ErrDenied
)
