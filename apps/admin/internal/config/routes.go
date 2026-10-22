package config

import "net/url"

// AppRoutes are the admin HTML pages. Link and redirect through these, never
// literals. Patterns with {id} are for ServeMux; build concrete paths with the
// helper funcs.
var AppRoutes = struct {
	Home, Login, SetPassword, TwoFactor, TwoFactorSetup string
	Users, UserByID, Admins, Audit, Analytics           string
}{
	Home:           "/",
	Login:          "/login",
	SetPassword:    "/set-password", // where invite links land (?token=)
	TwoFactor:      "/two-factor",   // second step of sign-in
	TwoFactorSetup: "/two-factor/setup",
	Users:          "/users",
	UserByID:       "/users/{id}",
	Admins:         "/admins",
	Audit:          "/audit",
	Analytics:      "/analytics",
}

// APIRoutes are the admin JSON endpoints.
var APIRoutes = struct {
	Auth string // prefix of the admin auth endpoints (server mounts an allowlist of them)

	// App users (the main app's database).
	Users, UserByID, UserBan, UserUnban, UserRole string
	UserSessions, UserSessionByID                 string
	// Admins (the admin database).
	Admins, AdminInvites string
	Audit, Analytics     string

	Healthz string
}{
	Auth:            "/api/auth/",
	Users:           "/api/admin/v1/users",
	UserByID:        "/api/admin/v1/users/{id}",
	UserBan:         "/api/admin/v1/users/{id}/ban",
	UserUnban:       "/api/admin/v1/users/{id}/unban",
	UserRole:        "/api/admin/v1/users/{id}/role",
	UserSessions:    "/api/admin/v1/users/{id}/sessions",
	UserSessionByID: "/api/admin/v1/users/{id}/sessions/{sessionId}",
	Admins:          "/api/admin/v1/admins",
	AdminInvites:    "/api/admin/v1/admins/invites",
	Audit:           "/api/admin/v1/audit",
	Analytics:       "/api/admin/v1/analytics",
	Healthz:         "/healthz",
}

// UserPage is the concrete path for AppRoutes.UserByID.
func UserPage(id string) string {
	return AppRoutes.Users + "/" + url.PathEscape(id)
}

// UserPath is the concrete path for APIRoutes.UserByID; suffix is appended
// ("", "/ban", "/unban", "/role", "/sessions").
func UserPath(id, suffix string) string {
	return APIRoutes.Users + "/" + url.PathEscape(id) + suffix
}

// UserSessionPath is the concrete path for APIRoutes.UserSessionByID.
func UserSessionPath(id, sessionID string) string {
	return UserPath(id, "/sessions/"+url.PathEscape(sessionID))
}

// Absolute joins a route onto an origin (config.PublicURL for email links).
func Absolute(origin *url.URL, path string) string {
	return origin.JoinPath(path).String()
}
