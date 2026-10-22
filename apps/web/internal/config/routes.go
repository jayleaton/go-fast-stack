package config

import "net/url"

// AppRoutes are the HTML pages. Link and redirect through these, never literals.
var AppRoutes = struct {
	Home, Why, Login, Signup, ForgotPassword, ResetPassword string
	VerifyEmail, VerifyEmailSuccess, Dashboard              string
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
}

// APIRoutes are the JSON endpoints. Patterns (with {id}) are for ServeMux;
// build concrete paths with the helper funcs.
var APIRoutes = struct {
	Auth     string // prefix, auth.Handler() is mounted here
	Users    string
	UserByID string
	Healthz  string
}{
	Auth:     "/api/auth/",
	Users:    "/api/core/v1/users",
	UserByID: "/api/core/v1/users/{id}",
	Healthz:  "/healthz",
}

// UserPath is the concrete path for APIRoutes.UserByID.
func UserPath(id string) string {
	return APIRoutes.Users + "/" + url.PathEscape(id)
}

// Absolute joins a route onto an origin (config.PublicURL for email links).
func Absolute(origin *url.URL, path string) string {
	return origin.JoinPath(path).String()
}
