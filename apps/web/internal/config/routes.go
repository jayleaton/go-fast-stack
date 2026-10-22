package config

import "net/url"

// AppRoutes are the HTML pages. Link and redirect through these, never literals.
var AppRoutes = struct {
	Home, Why, Login, Signup, ForgotPassword, ResetPassword string
	VerifyEmail, VerifyEmailSuccess, Dashboard              string
	// Account is the account section of the dashboard; EmailChanged is where
	// a change-email verification link lands (the default callbackURL).
	Account, EmailChanged string
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
}

// APIRoutes are the JSON endpoints. Patterns (with {id}) are for ServeMux;
// build concrete paths with the helper funcs.
var APIRoutes = struct {
	Auth string // prefix, auth.Handler() is mounted here

	// The signed-in user's account.
	Me, MeEmail, MePassword, MeData            string
	MeSessions, MeSessionByID, MeOtherSessions string
	// Another user by id (the service answers 403 unless it is the caller).
	UserByID string

	Healthz string
}{
	Auth:            "/api/auth/",
	Me:              "/api/core/v1/users/me",
	MeEmail:         "/api/core/v1/users/me/email",
	MePassword:      "/api/core/v1/users/me/password",
	MeData:          "/api/core/v1/users/me/data",
	MeSessions:      "/api/core/v1/users/me/sessions",
	MeSessionByID:   "/api/core/v1/users/me/sessions/{id}",
	MeOtherSessions: "/api/core/v1/users/me/sessions/others",
	UserByID:        "/api/core/v1/users/{id}",
	Healthz:         "/healthz",
}

// UserPath is the concrete path for APIRoutes.UserByID.
func UserPath(id string) string {
	return "/api/core/v1/users/" + url.PathEscape(id)
}

// MeSessionPath is the concrete path for APIRoutes.MeSessionByID.
func MeSessionPath(id string) string {
	return APIRoutes.MeSessions + "/" + url.PathEscape(id)
}

// Absolute joins a route onto an origin (config.PublicURL for email links).
func Absolute(origin *url.URL, path string) string {
	return origin.JoinPath(path).String()
}
