package ui

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"unicode"

	"github.com/a-h/templ"
	"github.com/starfederation/datastar-go/datastar"

	"github.com/jayleaton/go-fast-stack/apps/admin/internal/access"
	"github.com/jayleaton/go-fast-stack/apps/admin/internal/config"
	"github.com/jayleaton/go-fast-stack/apps/admin/internal/services"
	"github.com/jayleaton/go-fast-stack/apps/admin/internal/validate"
	"github.com/jayleaton/go-fast-stack/apps/admin/ui/i18n"
	"github.com/jayleaton/go-fast-stack/apps/admin/ui/routes"
	"github.com/jayleaton/go-fast-stack/apps/admin/ui/static"
	"github.com/jayleaton/go-fast-stack/apps/admin/ui/view"
)

// UI serves the admin pages and their Datastar actions.
type UI struct {
	auth             Auth
	users            Users
	admins           Admins
	audit            Audit
	analytics        Analytics
	gate             *access.Gate
	requireTwoFactor bool
	appName          string
	secure           bool
	log              *slog.Logger
	csrf             *http.CrossOriginProtection
}

// New builds the UI. Every port and Gate are required.
func New(deps Deps) *UI {
	if deps.Auth == nil || deps.Users == nil || deps.Admins == nil || deps.Audit == nil || deps.Analytics == nil || deps.Gate == nil {
		panic("ui.New: Auth, Users, Admins, Audit, Analytics and Gate are required")
	}
	ui := &UI{
		auth:             deps.Auth,
		users:            deps.Users,
		admins:           deps.Admins,
		audit:            deps.Audit,
		analytics:        deps.Analytics,
		gate:             deps.Gate,
		requireTwoFactor: deps.RequireTwoFactor,
		appName:          deps.AppName,
		secure:           deps.SecureCookies,
		log:              deps.Logger,
		csrf:             http.NewCrossOriginProtection(),
	}
	if ui.appName == "" {
		ui.appName = view.DefaultAppName
	}
	if ui.log == nil {
		ui.log = slog.Default()
	}
	return ui
}

// Mux is what Routes registers on (*http.ServeMux).
type Mux interface {
	Handle(pattern string, handler http.Handler)
}

// Routes mounts every page, action and static asset on mux. Each page and
// action declares its access policy on its line (access.Public,
// access.SignedIn, or your own); the gate in Deps enforces it. The auth
// middleware must wrap mux so the gate sees the session. Mount Handler
// rather than Routes in production: it adds the security headers.
func (ui *UI) Routes(mux Mux) {
	page := func(policy access.Policy, pattern string, handler handlerFunc) {
		mux.Handle(pattern, ui.route(policy, handler))
	}
	// Actions also pass Go's cross-origin protection (Sec-Fetch-Site / Origin).
	action := func(policy access.Policy, pattern string, handler handlerFunc) {
		mux.Handle(pattern, ui.csrf.Handler(ui.route(policy, handler)))
	}
	pages := config.AppRoutes

	// Static assets are the same for everyone and never read the session.
	mux.Handle("GET "+routes.Static, static.Handler())
	mux.Handle("GET /favicon.ico", http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		http.Redirect(writer, request, static.URL(static.FaviconFile), http.StatusMovedPermanently)
	}))

	page(access.Public, "GET /{$}", ui.home)
	page(access.Public, "GET "+pages.Login, ui.loginPage)
	action(access.Public, "POST "+pages.Login, ui.login)
	page(access.Public, "GET "+pages.SetPassword, ui.setPasswordPage)
	action(access.Public, "POST "+pages.SetPassword, ui.setPassword)
	page(access.Public, "GET "+pages.ForgotPassword, ui.forgotPasswordPage)
	action(access.Public, "POST "+pages.ForgotPassword, ui.forgotPassword)
	page(access.Public, "GET "+pages.ResetPassword, ui.resetPasswordPage)
	action(access.Public, "POST "+pages.ResetPassword, ui.resetPassword)
	page(access.Public, "GET "+pages.TwoFactor, ui.twoFactorPage)
	action(access.Public, "POST "+pages.TwoFactor, ui.twoFactor)
	// Signed in (optional enrolment) or with a SetupRequired challenge.
	page(access.Public, "GET "+pages.TwoFactorSetup, ui.twoFactorSetupPage)
	action(access.Public, "POST "+pages.TwoFactorSetup, ui.twoFactorSetup)
	action(access.SignedIn, "POST "+routes.EnableTwoFactor, ui.enableTwoFactor)

	page(access.SignedIn, "GET "+pages.Users, ui.usersPage)
	page(access.SignedIn, "GET "+pages.UserByID, ui.userPage)
	action(access.SignedIn, "POST "+routes.UserBan, ui.banUser)
	action(access.SignedIn, "POST "+routes.UserUnban, ui.unbanUser)
	action(access.SignedIn, "POST "+routes.UserRole, ui.setRole)
	action(access.SignedIn, "POST "+routes.UserSessions, ui.revokeSessions)
	action(access.SignedIn, "POST "+routes.UserSession, ui.revokeSession)
	page(access.SignedIn, "GET "+pages.Admins, ui.adminsPage)
	action(access.SignedIn, "POST "+routes.AdminInvite, ui.inviteAdmin)
	action(access.SignedIn, "POST "+routes.AdminDisable, ui.disableAdmin)
	action(access.SignedIn, "POST "+routes.AdminEnable, ui.enableAdmin)
	action(access.SignedIn, "POST "+routes.AdminRemove, ui.removeAdmin)
	action(access.SignedIn, "POST "+routes.AdminPasswordReset, ui.sendAdminPasswordReset)
	action(access.SignedIn, "POST "+routes.AdminResetTwoFactor, ui.resetAdminTwoFactor)
	page(access.SignedIn, "GET "+pages.Audit, ui.auditPage)
	page(access.SignedIn, "GET "+pages.Analytics, ui.analyticsPage)

	action(access.Public, "POST "+routes.Logout, ui.logout)
	action(access.Public, "POST "+routes.Locale, ui.setLocale)
}

// Handler returns the UI for server.Deps.UI: Routes behind the browser
// security headers (secure), with a bare 404 for anything unrouted.
func (ui *UI) Handler() http.Handler {
	mux := http.NewServeMux()
	ui.Routes(mux)
	return secure(strict(mux), ui.secure)
}

// section is the nav entry a path belongs to.
func section(path string) string {
	under := func(root string) bool { return path == root || strings.HasPrefix(path, root+"/") }
	switch {
	case under(config.AppRoutes.Users):
		return view.SectionUsers
	case under(config.AppRoutes.Admins):
		return view.SectionAdmins
	case under(config.AppRoutes.Audit):
		return view.SectionAudit
	case under(config.AppRoutes.Analytics):
		return view.SectionAnalytics
	}
	return ""
}

// isDatastar reports whether a Datastar action sent the request; otherwise
// it is a plain form post and gets a full page or a redirect back.
func isDatastar(request *http.Request) bool {
	return request.Header.Get("Datastar-Request") == "true"
}

func (ui *UI) render(writer http.ResponseWriter, request *http.Request, status int, component templ.Component) {
	writer.Header().Set("Content-Type", "text/html; charset=utf-8")
	writer.WriteHeader(status)
	if err := component.Render(request.Context(), writer); err != nil {
		ui.log.ErrorContext(request.Context(), "ui: render", "path", request.URL.Path, "err", err)
	}
}

// patch answers a Datastar action with fragments patched by id, or a plain
// form post with the full page.
func (ui *UI) patch(writer http.ResponseWriter, request *http.Request, status int, page templ.Component, fragments ...templ.Component) {
	if !isDatastar(request) {
		ui.render(writer, request, status, page)
		return
	}
	ui.patchOnly(writer, request, fragments...)
}

// patchOnly streams fragments to a Datastar action.
func (ui *UI) patchOnly(writer http.ResponseWriter, request *http.Request, fragments ...templ.Component) {
	sse := datastar.NewSSE(writer, request)
	for _, fragment := range fragments {
		if err := sse.PatchElementTempl(fragment); err != nil {
			ui.log.WarnContext(request.Context(), "ui: patch", "path", request.URL.Path, "err", err)
			return
		}
	}
}

// redirect navigates the browser: via SSE for Datastar, 303 otherwise.
// Set cookies before calling it.
func (ui *UI) redirect(writer http.ResponseWriter, request *http.Request, to string) {
	if !isDatastar(request) {
		http.Redirect(writer, request, to, http.StatusSeeOther)
		return
	}
	if err := datastar.NewSSE(writer, request).Redirect(to); err != nil {
		ui.log.WarnContext(request.Context(), "ui: redirect", "to", to, "err", err)
	}
}

// needsTwoFactor sends the admin to enrolment when a service refused them
// for lacking 2FA, and reports whether it did.
func (ui *UI) needsTwoFactor(writer http.ResponseWriter, request *http.Request, err error) bool {
	if !errors.Is(err, services.ErrTwoFactorRequired) {
		return false
	}
	ui.redirect(writer, request, config.AppRoutes.TwoFactorSetup)
	return true
}

// errorKeys maps the auth port's and the services' errors to message keys,
// most specific first. Validation errors (and any other KindInvalid) show
// errors.invalidInput; anything else errors.somethingWentWrong, logged.
var errorKeys = []struct {
	err error
	key string
}{
	{ErrInvalidCredentials, "errors.invalidCredentials"},
	{ErrInvalidCode, "errors.invalidCode"},
	{ErrInvalidToken, "errors.invalidToken"},
	{ErrPasswordPolicy, "errors.passwordPolicy"},
	{ErrInvalidPassword, "errors.invalidPassword"},
	{ErrChallengeExpired, "errors.challengeExpired"},
	{ErrTooManyRequests, "errors.tooManyRequests"},
	{services.ErrUserNotFound, "errors.userNotFound"},
	{services.ErrSessionNotFound, "errors.sessionNotFound"},
	{services.ErrInvalidRole, "errors.invalidRole"},
	{services.ErrRoleUnchanged, "errors.roleUnchanged"},
	{services.ErrUserNotBanned, "errors.userNotBanned"},
	{services.ErrAdminExists, "errors.adminExists"},
	{services.ErrAdminNotFound, "errors.adminNotFound"},
	{services.ErrAdminSelf, "errors.adminSelf"},
	{services.ErrLastAdmin, "errors.lastAdmin"},
	{services.ErrAdminDisabled, "errors.adminDisabled"},
	{services.ErrAdminNotDisabled, "errors.adminNotDisabled"},
	{services.ErrAdminPending, "errors.adminPending"},
	{services.ErrAdminNoTwoFactor, "errors.adminNoTwoFactor"},
	{services.ErrAdminSelfReset, "errors.adminSelfReset"},
	{services.ErrTwoFactorRequired, "errors.twoFactorRequired"},
	{services.ErrAuditUnavailable, "errors.auditUnavailable"},
	{services.ErrForbidden, "errors.forbidden"},
}

// errorMessage translates an auth, service or validation error; unknown
// errors are logged, and so is a refusal for an unavailable audit log (its
// wrapped cause is the outage).
func (ui *UI) errorMessage(ctx context.Context, err error) string {
	if errors.Is(err, services.ErrAuditUnavailable) {
		ui.log.ErrorContext(ctx, "ui: action refused", "err", err)
	}
	for _, entry := range errorKeys {
		if errors.Is(err, entry.err) {
			return i18n.T(ctx, entry.key)
		}
	}
	var verr *validate.Error
	if errors.As(err, &verr) || services.KindOf(err) == services.KindInvalid {
		return i18n.T(ctx, "errors.invalidInput")
	}
	ui.log.ErrorContext(ctx, "ui: error", "err", err)
	return i18n.T(ctx, "errors.somethingWentWrong")
}

// statusOf is the HTTP status of a page that failed with err.
func statusOf(err error) int {
	var verr *validate.Error
	if errors.As(err, &verr) {
		return http.StatusBadRequest
	}
	switch services.KindOf(err) {
	case services.KindInvalid:
		return http.StatusBadRequest
	case services.KindForbidden:
		return http.StatusForbidden
	case services.KindNotFound:
		return http.StatusNotFound
	case services.KindConflict:
		return http.StatusConflict
	}
	return http.StatusInternalServerError
}

// safeNext keeps redirects on this site.
func safeNext(next string) string {
	// Browsers drop tabs and newlines and treat \ as /, so "/\t/evil.com" would
	// leave the site: refuse control characters and backslashes outright.
	if !strings.HasPrefix(next, "/") || strings.HasPrefix(next, "//") || strings.ContainsRune(next, '\\') || strings.IndexFunc(next, unicode.IsControl) >= 0 {
		return config.AppRoutes.Home
	}
	return next
}

// pathWithoutToken is the request's path and query minus an emailed link's
// token: the layout echoes the path (the language switcher's "next"), and a
// token may only appear in its page's own hidden form field.
func pathWithoutToken(target *url.URL) string {
	query := target.Query()
	if !query.Has("token") {
		return target.RequestURI()
	}
	query.Del("token")
	out := *target
	out.RawQuery = query.Encode()
	return out.RequestURI()
}
