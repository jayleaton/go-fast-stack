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
	"time"

	"github.com/jayleaton/go-fast-stack/apps/web/internal/apiclient"
	"github.com/jayleaton/go-fast-stack/apps/web/internal/config"
	"github.com/jayleaton/go-fast-stack/apps/web/ui/i18n"
)

// User is the signed-in user as the templates show them.
type User = apiclient.User

// AuthError pairs an auth error with the message key the UI shows for it.
type AuthError struct {
	Err error
	Key string
}

// AuthErrors are the sign-in flows' errors the UI translates (the API's
// apiclient values; packages/auth stays behind the API); any other error
// shows errors.somethingWentWrong and is logged.
var AuthErrors = []AuthError{
	{apiclient.ErrInvalidEmail, "errors.emailInvalid"},
	{apiclient.ErrInvalidCredentials, "errors.invalidCredentials"},
	{apiclient.ErrPasswordTooShort, "errors.passwordTooShort"},
	{apiclient.ErrPasswordTooLong, "errors.passwordTooLong"},
	{apiclient.ErrUserAlreadyExists, "errors.emailTaken"},
	{apiclient.ErrEmailNotVerified, "pages.verifyEmail.description"},
	{apiclient.ErrInvalidToken, "pages.verifyEmail.invalidToken"},
	{apiclient.ErrTokenExpired, "pages.verifyEmail.invalidToken"},
	{apiclient.ErrTooManyRequests, "errors.tooManyRequests"},
	{apiclient.ErrBannedUser, "errors.banned"},
	{apiclient.ErrEmailTaken, "errors.emailTaken"},
	{apiclient.ErrInvalidUser, "pages.verifyEmail.invalidUser"},
}

// authCodeMessage translates the ?error=<code> auth's GET /verify-email adds
// to the callbackURL when a link fails (Better Auth codes, e.g. INVALID_USER).
func (ui *UI) authCodeMessage(ctx context.Context, code string) string {
	if err := ui.api.AuthErrorForCode(code); err != nil {
		for _, known := range AuthErrors {
			if errors.Is(err, known.Err) {
				return i18n.Translate(ctx, known.Key)
			}
		}
	}
	return i18n.Translate(ctx, "pages.verifyEmail.unexpectedErrorDuringVerification")
}

// socialErrors are the ?error= codes a failed Google / Apple sign-in lands
// on the login page with (packages/auth README "Social sign-in") that get
// their own message; every other code shows pages.login.social.failed.
var socialErrors = map[string]string{
	"access_denied":       "pages.login.social.cancelled",
	"account_not_linked":  "pages.login.social.accountNotLinked",
	"email_not_verified":  "pages.login.social.emailNotVerified",
	"signup_disabled":     "pages.login.social.signupDisabled",
	"too_many_requests":   "errors.tooManyRequests",
	"two_factor_required": "pages.login.social.twoFactorRequired",
	"banned":              "errors.banned",
}

// socialErrorMessage translates a social sign-in ?error= code ("" for none).
func socialErrorMessage(ctx context.Context, code string) string {
	if code == "" {
		return ""
	}
	if key, ok := socialErrors[code]; ok {
		return i18n.Translate(ctx, key)
	}
	return i18n.Translate(ctx, "pages.login.social.failed")
}

// Deps are the in-process dependencies of the UI.
type Deps struct {
	// API is how pages and actions read and change data: the in-process
	// api.Client, so every call takes the API's access policy and
	// validation. There is no HTTP client for the UI and no API base URL:
	// browsers only ever talk to the UI's own routes.
	API apiclient.Client
	// Gate decides route access (apiclient.NewGate(server.Callers)); the API
	// gets the same one.
	Gate     *apiclient.Gate
	Features config.Features
	// AppName is the product name in the header and <title>; defaults to
	// config.DefaultAppName.
	AppName string
	// SecureCookies marks UI cookies (locale) Secure; true behind HTTPS.
	SecureCookies bool
	Logger        *slog.Logger
	// Now is the clock for the live dashboard; defaults to time.Now.
	Now func() time.Time
	// LiveInterval is the dashboard clock tick; defaults to one second.
	LiveInterval time.Duration
}
