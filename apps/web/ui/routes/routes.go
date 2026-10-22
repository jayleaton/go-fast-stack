// Package routes is the single place for web UI paths (the Go AppRoutes).
// Navigate and post through these constants, never through string literals.
// feat/foundation moves this file into apps/web/internal/config at integration.
package routes

const (
	Home               = "/"
	Why                = "/why"
	Login              = "/login"
	Signup             = "/signup"
	Dashboard          = "/dashboard"
	ForgotPassword     = "/auth/forgot-password"
	ResetPassword      = "/auth/reset-password"
	VerifyEmail        = "/auth/verify-email"
	VerifyEmailSuccess = "/auth/verify-email/success"

	// HTML transport actions (Datastar posts and their no-JS form fallbacks).
	// The page paths above also accept POST for their own form.
	Logout         = "/logout"
	Locale         = "/locale"
	DashboardLive  = "/dashboard/live"
	UpdateSettings = "/dashboard/settings"
	DeleteAccount  = "/dashboard/delete-account"
	Static         = "/static/"
)
