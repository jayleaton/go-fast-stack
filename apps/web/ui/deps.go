// Package ui is the HTML transport: templ pages plus Datastar handlers.
// It calls auth and user services in-process through the interfaces below,
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
)

// User is the account data the UI shows. Mirrors the auth contract's User.
type User struct {
	ID            string
	Name          string
	Email         string
	EmailVerified bool
	Image         string
	CreatedAt     time.Time
}

// Session is the signed-in session. Mirrors the auth contract's Session.
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
}

// SignInInput mirrors auth.SignInInput.
type SignInInput struct {
	Email    string
	Password string
}

// UpdateSettingsInput carries the dashboard settings form. Validation belongs
// to the service (apps/web/internal/validate), not to this transport.
type UpdateSettingsInput struct {
	Name  string
	Email string
}

// Auth is the slice of packages/auth the HTML transport needs. The method set
// matches the auth contract in docs/BUILD-PLAN.md; foundation adapts *auth.Auth
// to it (types and error mapping) at integration.
type Auth interface {
	SignUpEmail(ctx context.Context, in SignUpInput) (User, Session, error)
	SignInEmail(ctx context.Context, in SignInInput) (User, Session, error)
	SignOut(ctx context.Context, sessionToken string) error
	// RequestPasswordReset emails a reset link. redirectTo is the app path of the
	// reset page (routes.ResetPassword); auth joins it with its BaseURL.
	RequestPasswordReset(ctx context.Context, email, redirectTo string) error
	ResetPassword(ctx context.Context, token, newPassword string) error
	VerifyEmail(ctx context.Context, token string) (User, Session, error)
	// SendVerificationEmail backs the "resend verification email" button.
	// Not in the v0 contract yet; Better Auth's equivalent is
	// POST /api/auth/send-verification-email. callbackURL is an app path.
	SendVerificationEmail(ctx context.Context, email, callbackURL string) error
	SetSessionCookie(w http.ResponseWriter, s Session)
	ClearSessionCookie(w http.ResponseWriter)
	// UserFrom reads the session the auth middleware stored on the request.
	UserFrom(ctx context.Context) (User, Session, bool)
}

// Users is the user service the dashboard calls.
type Users interface {
	UpdateSettings(ctx context.Context, userID string, in UpdateSettingsInput) (User, error)
	DeleteAccount(ctx context.Context, userID string) error
}

// Errors the services return (wrapped is fine; matched with errors.Is). Any
// other error is shown as errors.somethingWentWrong and logged.
var (
	ErrInvalidCredentials = errors.New("invalid credentials")
	ErrEmailNotVerified   = errors.New("email not verified")
	ErrEmailTaken         = errors.New("email already in use")
	ErrPasswordTooShort   = errors.New("password too short")
	ErrInvalidToken       = errors.New("invalid or expired token")
	ErrInvalidInput       = errors.New("invalid input")
)

// errorKeys maps service errors to message keys.
var errorKeys = []struct {
	err error
	key string
}{
	{ErrInvalidCredentials, "errors.invalidCredentials"},
	{ErrEmailNotVerified, "pages.verifyEmail.description"},
	{ErrEmailTaken, "errors.emailTaken"},
	{ErrPasswordTooShort, "errors.passwordTooShort"},
	{ErrInvalidToken, "pages.verifyEmail.invalidToken"},
	{ErrInvalidInput, "errors.invalidInput"},
}

// Features are the toggles the UI reads (owned by config, see BUILD-PLAN).
type Features struct {
	// EmailVerification: signup sends users to the verify-email page instead of
	// signing them in. Off in dev, on in production.
	EmailVerification bool
	// TemplateWelcome shows the starter landing page at / and enables /why.
	TemplateWelcome bool
}

// Deps are the in-process dependencies of the UI.
type Deps struct {
	Auth     Auth
	Users    Users
	Features Features
	// SecureCookies marks UI cookies (locale) Secure; true behind HTTPS.
	SecureCookies bool
	Logger        *slog.Logger
	// Now is the clock for the live dashboard; defaults to time.Now.
	Now func() time.Time
	// LiveInterval is the dashboard clock tick; defaults to one second.
	LiveInterval time.Duration
}
