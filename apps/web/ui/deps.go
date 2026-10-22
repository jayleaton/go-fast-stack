// Package ui is the HTML transport: templ pages plus Datastar handlers.
// It calls auth and the user service in-process through the interfaces below,
// so it never imports the database or the auth implementation.
//
// Mount it with ui.New(deps).Routes(mux).
package ui

//go:generate go tool mockgen -source=deps.go -destination=mocks/mocks.go -package=mocks

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/jayleaton/ai-patterns-go/apps/web/internal/config"
	"github.com/jayleaton/ai-patterns-go/apps/web/internal/services"
	"github.com/jayleaton/ai-patterns-go/apps/web/internal/validate"
	"github.com/jayleaton/ai-patterns-go/apps/web/ui/i18n"
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

// Users is the account service the dashboard and the Account page call: the
// method set of *services.UserService. Every method names the user it acts
// on; the service answers KindForbidden for anyone but the actor.
type Users interface {
	GetProfile(ctx context.Context, actor services.Actor, userID string) (services.User, error)
	UpdateProfile(ctx context.Context, actor services.Actor, userID string, in validate.UpdateUserSettings) (services.User, error)
	RequestEmailChange(ctx context.Context, actor services.Actor, userID string, in validate.UpdateUserEmail) error
	ChangePassword(ctx context.Context, actor services.Actor, userID string, in validate.UpdateUserPassword) error
	ListSessions(ctx context.Context, actor services.Actor, userID string) ([]services.Session, error)
	RevokeSession(ctx context.Context, actor services.Actor, userID, sessionID string) error
	RevokeOtherSessions(ctx context.Context, actor services.Actor, userID string) error
	DeleteAccount(ctx context.Context, actor services.Actor, userID string) error
	DataSummary(ctx context.Context, actor services.Actor, userID string) (services.UserDataSummary, error)
}

// The real implementations plug in without adapters.
var (
	_ Auth  = (*auth.Auth)(nil)
	_ Users = (*services.UserService)(nil)
)

// AuthError pairs an auth error with the message key the UI shows for it.
type AuthError struct {
	Err error
	Key string
}

// AuthErrors are the auth errors the UI translates; any other auth error
// shows errors.somethingWentWrong and is logged. Handler tests iterate it, so
// they never import packages/auth.
var AuthErrors = []AuthError{
	{auth.ErrInvalidEmail, "errors.emailInvalid"},
	{auth.ErrInvalidEmailOrPassword, "errors.invalidCredentials"},
	{auth.ErrPasswordTooShort, "errors.passwordTooShort"},
	{auth.ErrPasswordTooLong, "errors.passwordTooLong"},
	{auth.ErrUserAlreadyExists, "errors.emailTaken"},
	{auth.ErrEmailNotVerified, "pages.verifyEmail.description"},
	{auth.ErrInvalidToken, "pages.verifyEmail.invalidToken"},
	{auth.ErrTokenExpired, "pages.verifyEmail.invalidToken"},
	{auth.ErrTooManyRequests, "errors.tooManyRequests"},
	{auth.ErrBannedUser, "errors.banned"},
	{auth.ErrEmailTaken, "errors.emailTaken"},
	{auth.ErrInvalidUser, "pages.verifyEmail.invalidUser"},
}

// authCodeMessage translates the ?error=<code> auth's GET /verify-email adds
// to the callbackURL when a link fails (Better Auth codes, e.g. INVALID_USER).
func authCodeMessage(ctx context.Context, code string) string {
	for _, e := range AuthErrors {
		if ae, ok := errors.AsType[*auth.Error](e.Err); ok && ae.Code == code {
			return i18n.T(ctx, e.Key)
		}
	}
	return i18n.T(ctx, "pages.verifyEmail.unexpectedErrorDuringVerification")
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
