// Package ui is the admin HTML transport: templ pages plus Datastar handlers.
// It calls the admin services and a UI-owned Auth port in-process, so it never
// imports packages/auth, the databases or pgx. apps/admin/internal/server
// adapts the admin *auth.Auth to Auth.
//
// Mount it with ui.New(deps).Routes(mux).
package ui

import (
	"log/slog"

	"github.com/jayleaton/go-fast-stack/apps/admin/internal/apiclient"
)

// The sign-in flows' types and errors, as the API returns them (the UI
// reaches auth only through apiclient.Client).
type (
	TwoFactorChallenge = apiclient.TwoFactorChallenge
	TOTPSetup          = apiclient.TOTPSetup
)

// Two-factor methods of apiclient.VerifyTwoFactor.Method.
const (
	MethodTOTP       = apiclient.MethodTOTP
	MethodBackupCode = apiclient.MethodBackupCode
)

// Auth failures the API reports (matched with errors.Is). Anything else is
// logged and shown as "something went wrong".
var (
	ErrInvalidCredentials = apiclient.ErrInvalidCredentials
	ErrInvalidCode        = apiclient.ErrInvalidCode
	ErrInvalidToken       = apiclient.ErrInvalidToken
	ErrPasswordPolicy     = apiclient.ErrPasswordPolicy
	ErrInvalidPassword    = apiclient.ErrInvalidPassword
	ErrChallengeExpired   = apiclient.ErrChallengeExpired
	ErrTooManyRequests    = apiclient.ErrTooManyRequests
)

// Deps are the in-process dependencies of the UI. Auth, API and Gate are
// required.
type Deps struct {
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
