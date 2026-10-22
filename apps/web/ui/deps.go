// Package ui is the HTML transport: templ pages plus Datastar handlers.
// It calls auth and the user service in-process through the interfaces below,
// so it never imports the database or the auth implementation.
//
// Mount it with ui.New(deps).Routes(mux).
package ui

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/jayleaton/ai-patterns-go/apps/web/internal/config"
	"github.com/jayleaton/ai-patterns-go/apps/web/internal/services"
	"github.com/jayleaton/ai-patterns-go/apps/web/internal/validate"
)

// User is the account data the UI shows (the auth user, minus credentials).
type User struct {
	ID            string
	Name          string
	Email         string
	EmailVerified bool
	CreatedAt     time.Time
}

// Session is the signed-in session. Mirrors auth.Session.
type Session struct {
	Token     string
	UserID    string
	ExpiresAt time.Time
}

// SignUpInput mirrors auth.SignUpInput.
type SignUpInput struct {
	Name     string
	Email    string
	Password string
	// CallbackURL is where the verification link lands (an app path).
	CallbackURL string
}

// SignInInput mirrors auth.SignInInput.
type SignInInput struct {
	Email    string
	Password string
	// CallbackURL is where a verification link sent on sign-in lands.
	CallbackURL string
}

// Auth is the slice of packages/auth the HTML transport needs, with the same
// method set as *auth.Auth. Types are UI-local until packages/auth is on main;
// the server adapts *auth.Auth (field copy, and errors wrapped in the Err*
// values below with the same names).
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
	// UserFrom reads the session the auth middleware stored on the request.
	UserFrom(ctx context.Context) (User, Session, bool)
}

// Users is the user service the dashboard calls; *services.UserService
// satisfies it.
type Users interface {
	UpdateUserSettings(ctx context.Context, actor services.Actor, userID string, in validate.UpdateUserSettings) (services.User, error)
	DeleteAllUserData(ctx context.Context, actor services.Actor) error
}

// Auth errors, named after the auth.Err* values they stand for. Match with
// errors.Is; any other auth error shows errors.somethingWentWrong and is logged.
var (
	ErrInvalidEmail           = errors.New("invalid email")
	ErrInvalidEmailOrPassword = errors.New("invalid email or password")
	ErrPasswordTooShort       = errors.New("password too short")
	ErrPasswordTooLong        = errors.New("password too long")
	ErrUserAlreadyExists      = errors.New("user already exists")
	ErrEmailNotVerified       = errors.New("email not verified")
	ErrInvalidToken           = errors.New("invalid token")
	ErrTokenExpired           = errors.New("token expired")
	ErrTooManyRequests        = errors.New("too many requests")
)

// errorKeys maps auth errors to message keys.
var errorKeys = []struct {
	err error
	key string
}{
	{ErrInvalidEmail, "errors.emailInvalid"},
	{ErrInvalidEmailOrPassword, "errors.invalidCredentials"},
	{ErrPasswordTooShort, "errors.passwordTooShort"},
	{ErrPasswordTooLong, "errors.passwordTooLong"},
	{ErrUserAlreadyExists, "errors.emailTaken"},
	{ErrEmailNotVerified, "pages.verifyEmail.description"},
	{ErrInvalidToken, "pages.verifyEmail.invalidToken"},
	{ErrTokenExpired, "pages.verifyEmail.invalidToken"},
	{ErrTooManyRequests, "errors.tooManyRequests"},
}

// Deps are the in-process dependencies of the UI.
type Deps struct {
	Auth     Auth
	Users    Users
	Features config.Features
	// SecureCookies marks UI cookies (locale) Secure; true behind HTTPS.
	SecureCookies bool
	Logger        *slog.Logger
	// Now is the clock for the live dashboard; defaults to time.Now.
	Now func() time.Time
	// LiveInterval is the dashboard clock tick; defaults to one second.
	LiveInterval time.Duration
}
