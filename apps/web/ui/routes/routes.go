// Package routes holds the UI's own action endpoints (Datastar posts, the
// live stream, static assets). Page paths live in config.AppRoutes; these move
// there when the UI is wired into the server.
package routes

const (
	Logout         = "/logout"
	Locale         = "/locale"
	DashboardLive  = "/dashboard/live"
	UpdateSettings = "/dashboard/settings"
	DeleteAccount  = "/dashboard/delete-account"
	Static         = "/static/"
)
