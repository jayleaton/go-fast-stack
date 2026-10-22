// Package ui is the HTML transport: templ pages plus Datastar handlers.
// It calls auth and the user service in-process through the interfaces below,
// so it never imports the database or the auth implementation.
//
// Mount it with ui.New(deps).Routes(mux).
package ui

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	"github.com/jayleaton/ai-patterns-go/apps/web/internal/config"
	"github.com/jayleaton/ai-patterns-go/apps/web/internal/services"
	"github.com/jayleaton/ai-patterns-go/apps/web/internal/validate"
	"github.com/jayleaton/ai-patterns-go/packages/auth"
)

// The auth types, used as they are so *auth.Auth satisfies Auth directly.
type (
	User        = auth.User
	Session     = auth.Session
	SignUpInput = auth.SignUpInput
	SignInInput = auth.SignInInput
)

// Auth is the slice of *auth.Auth the HTML transport calls; *auth.Auth
// satisfies it. The session of a request is read with Deps.UserFrom.
type Auth interface {
	// SignUpEmail returns a zero Session when email verification is required.
	SignUpEmail(ctx context.Context, in SignUpInput) (User, Session, error)
	SignInEmail(ctx context.Context, in SignInInput) (User, Session, error)
	SignOut(ctx context.Context, sessionToken string) error
	// RequestPasswordReset emails a reset link that ends at redirectTo?token=
	// (an app path; required).
	RequestPasswordReset(ctx context.Context, email, redirectTo string) error
	ResetPassword(ctx context.Context, token, newPassword string) error
	// VerifyEmail returns a zero Session when the email was already verified.
	VerifyEmail(ctx context.Context, token string) (User, Session, error)
	SendVerificationEmail(ctx context.Context, email, callbackURL string) error
	SetSessionCookie(w http.ResponseWriter, s Session)
	ClearSessionCookie(w http.ResponseWriter)
}

// Users is the user service the dashboard calls; *services.UserService
// satisfies it.
type Users interface {
	UpdateProfile(ctx context.Context, actor services.Actor, userID string, in validate.UpdateUserSettings) (services.User, error)
	DeleteAccount(ctx context.Context, actor services.Actor, userID string) error
}

// The real implementations plug in without adapters.
var (
	_ Auth  = (*auth.Auth)(nil)
	_ Users = (*services.UserService)(nil)
)

// errorKeys maps auth errors to message keys. Any other auth error shows
// errors.somethingWentWrong and is logged.
var errorKeys = []struct {
	err error
	key string
}{
	{auth.ErrInvalidEmail, "errors.emailInvalid"},
	{auth.ErrInvalidEmailOrPassword, "errors.invalidCredentials"},
	{auth.ErrPasswordTooShort, "errors.passwordTooShort"},
	{auth.ErrPasswordTooLong, "errors.passwordTooLong"},
	{auth.ErrUserAlreadyExists, "errors.emailTaken"},
	{auth.ErrEmailNotVerified, "pages.verifyEmail.description"},
	{auth.ErrInvalidToken, "pages.verifyEmail.invalidToken"},
	{auth.ErrTokenExpired, "pages.verifyEmail.invalidToken"},
	{auth.ErrTooManyRequests, "errors.tooManyRequests"},
}

// Deps are the in-process dependencies of the UI.
type Deps struct {
	Auth  Auth
	Users Users
	// UserFrom reads the session auth.Middleware stored on the request;
	// defaults to auth.UserFrom.
	UserFrom func(ctx context.Context) (User, Session, bool)
	Features config.Features
	// SecureCookies marks UI cookies (locale) Secure; true behind HTTPS.
	SecureCookies bool
	Logger        *slog.Logger
	// Now is the clock for the live dashboard; defaults to time.Now.
	Now func() time.Time
	// LiveInterval is the dashboard clock tick; defaults to one second.
	LiveInterval time.Duration
}
