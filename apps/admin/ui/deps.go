// Package ui is the admin HTML transport: templ pages plus Datastar handlers.
// It calls the admin services and a UI-owned Auth port in-process, so it never
// imports packages/auth, the databases or pgx. apps/admin/internal/server
// adapts the admin *auth.Auth to Auth.
//
// Mount it with ui.New(deps).Routes(mux).
package ui

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/jayleaton/go-fast-stack/apps/admin/internal/apiclient"
)

// Session is an admin session.
type Session struct {
	ID, Token string
	ExpiresAt time.Time
}

// TwoFactorChallenge is returned AS THE ERROR by SignIn and
// CompleteSetPassword when a second factor (or, with SetupRequired, TOTP
// enrolment) is needed; no session exists yet. The handler stores it with
// SetTwoFactorCookie and sends the browser to the matching page.
type TwoFactorChallenge struct {
	Token         string
	Methods       []string
	SetupRequired bool
	ExpiresAt     time.Time
}

func (c *TwoFactorChallenge) Error() string {
	if c.SetupRequired {
		return "two-factor setup required"
	}
	return "two-factor verification required"
}

// TOTPSetup is a fresh TOTP secret, its otpauth:// URI and the one-time
// backup codes. Shown once.
type TOTPSetup struct {
	Secret, URI string
	BackupCodes []string
}

// Two-factor methods of VerifyTwoFactorInput.Method.
const (
	MethodTOTP       = "totp"
	MethodBackupCode = "backup_code"
)

// VerifyTwoFactorInput is the second sign-in step.
type VerifyTwoFactorInput struct {
	Code        string
	Method      string // MethodTOTP or MethodBackupCode
	TrustDevice bool
}

// Auth failures the adapter reports (matched with errors.Is). Anything else
// is logged and shown as "something went wrong".
var (
	ErrInvalidCredentials = errors.New("invalid email or password")
	ErrInvalidCode        = errors.New("invalid two-factor code")
	// ErrInvalidToken: a bad or expired set-password link.
	ErrInvalidToken   = errors.New("invalid or expired token")
	ErrPasswordPolicy = errors.New("password does not meet the policy")
	// ErrInvalidPassword: wrong current password when enabling 2FA.
	ErrInvalidPassword = errors.New("invalid password")
	// ErrChallengeExpired: the two-factor cookie is missing or expired.
	ErrChallengeExpired = errors.New("two-factor challenge expired")
	ErrTooManyRequests  = errors.New("too many requests")
)

// Auth is what the HTML transport needs from the admin auth instance. The
// caller of a request comes from Deps.Gate; the two-factor challenge
// of a request is the adapter's to read (from its cookie).
type Auth interface {
	SignIn(ctx context.Context, email, password string) (Session, error)
	SignOut(ctx context.Context, sessionToken string) error
	CompleteSetPassword(ctx context.Context, token, newPassword string) (Session, error)
	// RequestPasswordReset emails a reset link (to config.AppRoutes.ResetPassword)
	// if email belongs to an admin, pending or not, and returns nil either
	// way, so the page cannot tell who is an admin. ErrTooManyRequests when
	// rate limited. The link spends like an invite (CompleteSetPassword).
	RequestPasswordReset(ctx context.Context, email string) error
	// VerifyTwoFactor finishes sign-in (or SetupRequired enrolment) with the
	// challenge from the request's two-factor cookie; trustToken is set when
	// in.TrustDevice was asked for.
	VerifyTwoFactor(ctx context.Context, in VerifyTwoFactorInput) (s Session, trustToken string, err error)
	// StartTwoFactorSetup enrols the SetupRequired challenge from the cookie.
	StartTwoFactorSetup(ctx context.Context) (TOTPSetup, error)
	// EnableTwoFactor starts optional enrolment for a signed-in admin.
	EnableTwoFactor(ctx context.Context, adminID, password string) (TOTPSetup, error)
	ConfirmTwoFactor(ctx context.Context, adminID, code string) error
	SetSessionCookie(w http.ResponseWriter, s Session)
	ClearSessionCookie(w http.ResponseWriter)
	SetTwoFactorCookie(w http.ResponseWriter, c *TwoFactorChallenge)
	SetTrustDeviceCookie(w http.ResponseWriter, token string)
}

// Deps are the in-process dependencies of the UI. Auth, API and Gate are
// required.
type Deps struct {
	Auth Auth
	// API is how pages and actions read and change data: the in-process
	// api.Client, so every call takes the API's access policy and
	// validation. There is no HTTP client for the UI and no API base URL:
	// browsers only ever talk to the UI's own routes.
	API apiclient.Client
	// Gate decides route access (access.NewGate(callers.Resolve)), the same
	// one the API's endpoints use.
	Gate *apiclient.Gate
	// RequireTwoFactor is ADMIN_REQUIRE_2FA: the setup page says enrolment
	// is mandatory.
	RequireTwoFactor bool
	// AppName is the brand in the header and <title>; defaults to "Admin".
	AppName string
	// SecureCookies marks UI cookies (locale) Secure; true behind HTTPS.
	SecureCookies bool
	Logger        *slog.Logger
}
