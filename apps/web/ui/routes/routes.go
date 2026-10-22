// Package routes holds the UI's own action paths: Datastar posts, the live
// stream and static assets. Pages live in config.AppRoutes.
package routes

const (
	// The Account page's forms (config.AppRoutes.Account).
	AccountProfile       = "/dashboard/account/profile"
	AccountEmail         = "/dashboard/account/email"
	AccountPassword      = "/dashboard/account/password"
	AccountRevokeSession = "/dashboard/account/sessions/revoke"
	AccountRevokeOthers  = "/dashboard/account/sessions/revoke-others"
	AccountDelete        = "/dashboard/account/delete"

	Logout        = "/logout"
	Locale        = "/locale"
	DashboardLive = "/dashboard/live"
	Static        = "/static/"
)
