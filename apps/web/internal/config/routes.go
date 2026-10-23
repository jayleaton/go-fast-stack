package config

import "net/url"

// AppRoutes are the HTML pages. Link and redirect through these, never literals.
var AppRoutes = struct {
	Home, Why, Login, Signup, ForgotPassword, ResetPassword string
	VerifyEmail, VerifyEmailSuccess, Dashboard              string
	// Account is the account section of the dashboard; EmailChanged is where
	// a change-email verification link lands (the default callbackURL).
	Account, EmailChanged string
	// SocialSignIn starts Google / Apple sign-in (POST, {provider}).
	// SocialCallback is where the provider sends the browser back (GET, and
	// Apple's form POST); its prefix is the callback to register.
	SocialSignIn, SocialCallback string
}{
	Home:               "/",
	Why:                "/why",
	Login:              "/login",
	Signup:             "/signup",
	ForgotPassword:     "/auth/forgot-password",
	ResetPassword:      "/auth/reset-password",
	VerifyEmail:        "/auth/verify-email",
	VerifyEmailSuccess: "/auth/verify-email/success",
	Dashboard:          "/dashboard",
	Account:            "/dashboard/account",
	EmailChanged:       "/dashboard/account?email-changed=1",
	SocialSignIn:       "/auth/social/{provider}",
	SocialCallback:     SocialCallbackPrefix + "{provider}",
}

// SocialCallbackPrefix is AppRoutes.SocialCallback without {provider}: the
// providers redirect to <PUBLIC_URL>/auth/callback/google (or apple).
const SocialCallbackPrefix = "/auth/callback/"

// APIRoutes are the JSON endpoints. Patterns (with {id}) are for ServeMux;
// build concrete paths with the helper funcs.
var APIRoutes = struct {
	Auth string // prefix, auth.Handler() is mounted here

	// The signed-in user's account.
	Me, MeEmail, MePassword, MeData            string
	MeSessions, MeSessionByID, MeOtherSessions string
	// Another user by id (the service answers 403 unless it is the caller).
	UserByID string
	// Google and Apple sign-in, the web UI's browser flow (public).
	SocialProviders, SocialStart, SocialFinish string
	// Email and password sign-in flows (api.Sessions), called in-process by
	// the UI. Not mounted: native clients use auth's endpoints under Auth.
	SignUp, SignIn, SignOut        string
	PasswordReset, Password        string
	VerifyEmail, VerificationEmail string

	Healthz string
}{
	Auth:              "/api/auth/",
	Me:                "/api/core/v1/users/me",
	MeEmail:           "/api/core/v1/users/me/email",
	MePassword:        "/api/core/v1/users/me/password",
	MeData:            "/api/core/v1/users/me/data",
	MeSessions:        "/api/core/v1/users/me/sessions",
	MeSessionByID:     "/api/core/v1/users/me/sessions/{id}",
	MeOtherSessions:   "/api/core/v1/users/me/sessions/others",
	UserByID:          "/api/core/v1/users/{id}",
	SocialProviders:   "/api/core/v1/auth/social/providers",
	SocialStart:       "/api/core/v1/auth/social/{provider}/start",
	SocialFinish:      "/api/core/v1/auth/social/{provider}/finish",
	SignUp:            "/api/core/v1/auth/sign-up",
	SignIn:            "/api/core/v1/auth/sign-in",
	SignOut:           "/api/core/v1/auth/sign-out",
	PasswordReset:     "/api/core/v1/auth/password-reset",
	Password:          "/api/core/v1/auth/password",
	VerifyEmail:       "/api/core/v1/auth/verify-email",
	VerificationEmail: "/api/core/v1/auth/verification-email",
	Healthz:           "/healthz",
}

// UserPath is the concrete path for APIRoutes.UserByID.
func UserPath(id string) string {
	return "/api/core/v1/users/" + url.PathEscape(id)
}

// MeSessionPath is the concrete path for APIRoutes.MeSessionByID.
func MeSessionPath(id string) string {
	return APIRoutes.MeSessions + "/" + url.PathEscape(id)
}

// SocialSignInPath is the concrete path for AppRoutes.SocialSignIn.
func SocialSignInPath(provider string) string {
	return "/auth/social/" + url.PathEscape(provider)
}

// Absolute joins a route onto an origin (config.PublicURL for email links).
func Absolute(origin *url.URL, path string) string {
	return origin.JoinPath(path).String()
}
