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

	"github.com/jayleaton/go-fast-stack/apps/admin/internal/apiclient"
	"github.com/jayleaton/go-fast-stack/apps/admin/internal/config"
	"github.com/jayleaton/go-fast-stack/apps/admin/ui/i18n"
	"github.com/jayleaton/go-fast-stack/apps/admin/ui/routes"
	"github.com/jayleaton/go-fast-stack/apps/admin/ui/static"
	"github.com/jayleaton/go-fast-stack/apps/admin/ui/view"
)

// UI serves the admin pages and their Datastar actions.
type UI struct {
	api              apiclient.Client
	gate             *apiclient.Gate
	requireTwoFactor bool
	appName          string
	secure           bool
	log              *slog.Logger
	csrf             *http.CrossOriginProtection
}

// New builds the UI. Auth, API and Gate are required.
func New(deps Deps) *UI {
	if deps.API == nil || deps.Gate == nil {
		panic("ui.New: API and Gate are required")
	}
	ui := &UI{
		api:              deps.API,
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
// action declares its access policy on its line (apiclient.Public,
// apiclient.SignedIn, or your own); the gate in Deps enforces it. The auth
// middleware must wrap mux so the gate sees the session. Mount Handler
// rather than Routes in production: it adds the security headers.
func (ui *UI) Routes(mux Mux) {
	page := func(policy apiclient.Policy, pattern string, handler handlerFunc) {
		mux.Handle(pattern, ui.route(policy, handler))
	}
	// Actions also pass Go's cross-origin protection (Sec-Fetch-Site / Origin).
	action := func(policy apiclient.Policy, pattern string, handler handlerFunc) {
		mux.Handle(pattern, ui.csrf.Handler(ui.route(policy, handler)))
	}
	pages := config.AppRoutes

	// Static assets are the same for everyone and never read the session.
	mux.Handle("GET "+routes.Static, static.Handler())
	mux.Handle("GET /favicon.ico", http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		http.Redirect(writer, request, static.URL(static.FaviconFile), http.StatusMovedPermanently)
	}))

	page(apiclient.Public, "GET /{$}", ui.home)
	page(apiclient.Public, "GET "+pages.Login, ui.loginPage)
	action(apiclient.Public, "POST "+pages.Login, ui.login)
	page(apiclient.Public, "GET "+pages.SetPassword, ui.setPasswordPage)
	action(apiclient.Public, "POST "+pages.SetPassword, ui.setPassword)
	page(apiclient.Public, "GET "+pages.ForgotPassword, ui.forgotPasswordPage)
	action(apiclient.Public, "POST "+pages.ForgotPassword, ui.forgotPassword)
	page(apiclient.Public, "GET "+pages.ResetPassword, ui.resetPasswordPage)
	action(apiclient.Public, "POST "+pages.ResetPassword, ui.resetPassword)
	page(apiclient.Public, "GET "+pages.TwoFactor, ui.twoFactorPage)
	action(apiclient.Public, "POST "+pages.TwoFactor, ui.twoFactor)
	// Signed in (optional enrolment) or with a SetupRequired challenge.
	page(apiclient.Public, "GET "+pages.TwoFactorSetup, ui.twoFactorSetupPage)
	action(apiclient.Public, "POST "+pages.TwoFactorSetup, ui.twoFactorSetup)
	action(apiclient.SignedIn, "POST "+routes.EnableTwoFactor, ui.enableTwoFactor)

	page(apiclient.SignedIn, "GET "+pages.Users, ui.usersPage)
	page(apiclient.SignedIn, "GET "+pages.UserByID, ui.userPage)
	action(apiclient.SignedIn, "POST "+routes.UserBan, ui.banUser)
	action(apiclient.SignedIn, "POST "+routes.UserUnban, ui.unbanUser)
	action(apiclient.SignedIn, "POST "+routes.UserRole, ui.setRole)
	action(apiclient.SignedIn, "POST "+routes.UserSessions, ui.revokeSessions)
	action(apiclient.SignedIn, "POST "+routes.UserSession, ui.revokeSession)
	page(apiclient.SignedIn, "GET "+pages.Admins, ui.adminsPage)
	action(apiclient.SignedIn, "POST "+routes.AdminInvite, ui.inviteAdmin)
	action(apiclient.SignedIn, "POST "+routes.AdminDisable, ui.disableAdmin)
	action(apiclient.SignedIn, "POST "+routes.AdminEnable, ui.enableAdmin)
	action(apiclient.SignedIn, "POST "+routes.AdminRemove, ui.removeAdmin)
	action(apiclient.SignedIn, "POST "+routes.AdminPasswordReset, ui.sendAdminPasswordReset)
	action(apiclient.SignedIn, "POST "+routes.AdminResetTwoFactor, ui.resetAdminTwoFactor)
	page(apiclient.SignedIn, "GET "+pages.Audit, ui.auditPage)
	page(apiclient.SignedIn, "GET "+pages.Analytics, ui.analyticsPage)

	action(apiclient.Public, "POST "+routes.Logout, ui.logout)
	action(apiclient.Public, "POST "+routes.Locale, ui.setLocale)
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

// handled answers an API error that is not a form message and reports
// whether it did: an access refusal (signed out: the login page, denied:
// 403, as for the page's own policy) or a refusal for lacking 2FA (the
// enrolment page). Handlers check it after every API call.
func (ui *UI) handled(writer http.ResponseWriter, request *http.Request, err error) bool {
	switch {
	case errors.Is(err, apiclient.ErrSignedOut), errors.Is(err, apiclient.ErrDenied):
		ui.apiFailed(writer, request, err)
	case errors.Is(err, apiclient.ErrTwoFactorRequired):
		ui.redirect(writer, request, config.AppRoutes.TwoFactorSetup)
	default:
		return false
	}
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
	{apiclient.ErrUserNotFound, "errors.userNotFound"},
	{apiclient.ErrSessionNotFound, "errors.sessionNotFound"},
	{apiclient.ErrInvalidRole, "errors.invalidRole"},
	{apiclient.ErrRoleUnchanged, "errors.roleUnchanged"},
	{apiclient.ErrUserNotBanned, "errors.userNotBanned"},
	{apiclient.ErrAdminExists, "errors.adminExists"},
	{apiclient.ErrAdminNotFound, "errors.adminNotFound"},
	{apiclient.ErrAdminSelf, "errors.adminSelf"},
	{apiclient.ErrLastAdmin, "errors.lastAdmin"},
	{apiclient.ErrAdminDisabled, "errors.adminDisabled"},
	{apiclient.ErrAdminNotDisabled, "errors.adminNotDisabled"},
	{apiclient.ErrAdminPending, "errors.adminPending"},
	{apiclient.ErrAdminNoTwoFactor, "errors.adminNoTwoFactor"},
	{apiclient.ErrAdminSelfReset, "errors.adminSelfReset"},
	{apiclient.ErrTwoFactorRequired, "errors.twoFactorRequired"},
	{apiclient.ErrAuditUnavailable, "errors.auditUnavailable"},
	{apiclient.ErrForbidden, "errors.forbidden"},
}

// errorMessage translates an auth, service or validation error; unknown
// errors are logged, and so is a refusal for an unavailable audit log (its
// wrapped cause is the outage).
func (ui *UI) errorMessage(ctx context.Context, err error) string {
	if errors.Is(err, apiclient.ErrAuditUnavailable) {
		ui.log.ErrorContext(ctx, "ui: action refused", "err", err)
	}
	for _, entry := range errorKeys {
		if errors.Is(err, entry.err) {
			return i18n.Translate(ctx, entry.key)
		}
	}
	var verr *apiclient.ValidationError
	if errors.As(err, &verr) || apiclient.KindOf(err) == apiclient.KindInvalid {
		return i18n.Translate(ctx, "errors.invalidInput")
	}
	ui.log.ErrorContext(ctx, "ui: error", "err", err)
	return i18n.Translate(ctx, "errors.somethingWentWrong")
}

// statusOf is the HTTP status of a page that failed with err.
func statusOf(err error) int {
	var verr *apiclient.ValidationError
	if errors.As(err, &verr) {
		return http.StatusBadRequest
	}
	switch apiclient.KindOf(err) {
	case apiclient.KindInvalid:
		return http.StatusBadRequest
	case apiclient.KindForbidden:
		return http.StatusForbidden
	case apiclient.KindNotFound:
		return http.StatusNotFound
	case apiclient.KindConflict:
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
