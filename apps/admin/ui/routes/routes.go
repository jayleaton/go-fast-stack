// Package routes holds the admin UI's own action endpoints (Datastar posts and
// static assets). Page paths live in config.AppRoutes. Patterns with {id} are
// for ServeMux; build concrete paths with the helper funcs.
package routes

import "net/url"

const (
	Logout          = "/logout"
	Locale          = "/locale"
	Static          = "/static/"
	EnableTwoFactor = "/two-factor/setup/enable"
	UserBan         = "/users/{id}/ban"
	UserUnban       = "/users/{id}/unban"
	UserRole        = "/users/{id}/role"
	UserSessions    = "/users/{id}/sessions/revoke"
	UserSession     = "/users/{id}/sessions/{sessionId}/revoke"
	AdminInvite     = "/admins/invite"
	AdminDisable    = "/admins/{id}/disable"
	AdminEnable     = "/admins/{id}/enable"
	AdminRemove     = "/admins/{id}/remove"
)

// User returns the concrete path of a user action pattern for id
// (UserBan, UserUnban, UserRole, UserSessions).
func User(pattern, id string) string {
	return "/users/" + url.PathEscape(id) + pattern[len("/users/{id}"):]
}

// Session returns the concrete path of UserSession.
func Session(userID, sessionID string) string {
	return "/users/" + url.PathEscape(userID) + "/sessions/" + url.PathEscape(sessionID) + "/revoke"
}

// Admin returns the concrete path of an admin action pattern for id
// (AdminDisable, AdminEnable, AdminRemove).
func Admin(pattern, id string) string {
	return "/admins/" + url.PathEscape(id) + pattern[len("/admins/{id}"):]
}
