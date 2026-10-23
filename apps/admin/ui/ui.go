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
	userFrom         func(ctx context.Context) (Admin, Session, bool)
	requireTwoFactor bool
	appName          string
	secure           bool
	log              *slog.Logger
	csrf             *http.CrossOriginProtection
}

// New builds the UI. Every port and UserFrom are required.
func New(d Deps) *UI {
	if d.Auth == nil || d.Users == nil || d.Admins == nil || d.Audit == nil || d.Analytics == nil || d.UserFrom == nil {
		panic("ui.New: Auth, Users, Admins, Audit, Analytics and UserFrom are required")
	}
	u := &UI{
		auth:             d.Auth,
		users:            d.Users,
		admins:           d.Admins,
		audit:            d.Audit,
		analytics:        d.Analytics,
		userFrom:         d.UserFrom,
		requireTwoFactor: d.RequireTwoFactor,
		appName:          d.AppName,
		secure:           d.SecureCookies,
		log:              d.Logger,
		csrf:             http.NewCrossOriginProtection(),
	}
	if u.appName == "" {
		u.appName = view.DefaultAppName
	}
	if u.log == nil {
		u.log = slog.Default()
	}
	return u
}

// Mux is what Routes registers on (*http.ServeMux).
type Mux interface {
	Handle(pattern string, handler http.Handler)
}

// Routes mounts every page, action and static asset on mux. The auth
// middleware must wrap mux so Deps.UserFrom sees the session. Mount Handler
// rather than Routes in production: it adds the security headers.
func (u *UI) Routes(mux Mux) {
	page := func(pattern string, h http.HandlerFunc) { mux.Handle(pattern, u.wrap(h)) }
	// Actions also pass Go's cross-origin protection (Sec-Fetch-Site / Origin).
	action := func(pattern string, h http.HandlerFunc) { mux.Handle(pattern, u.csrf.Handler(u.wrap(h))) }
	p := config.AppRoutes

	mux.Handle("GET "+routes.Static, static.Handler())
	mux.Handle("GET /favicon.ico", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, static.URL(static.FaviconFile), http.StatusMovedPermanently)
	}))

	page("GET /{$}", u.home)
	page("GET "+p.Login, u.loginPage)
	action("POST "+p.Login, u.login)
	page("GET "+p.SetPassword, u.setPasswordPage)
	action("POST "+p.SetPassword, u.setPassword)
	page("GET "+p.ForgotPassword, u.forgotPasswordPage)
	action("POST "+p.ForgotPassword, u.forgotPassword)
	page("GET "+p.ResetPassword, u.resetPasswordPage)
	action("POST "+p.ResetPassword, u.resetPassword)
	page("GET "+p.TwoFactor, u.twoFactorPage)
	action("POST "+p.TwoFactor, u.twoFactor)
	page("GET "+p.TwoFactorSetup, u.twoFactorSetupPage)
	action("POST "+p.TwoFactorSetup, u.twoFactorSetup)
	action("POST "+routes.EnableTwoFactor, u.enableTwoFactor)

	page("GET "+p.Users, u.usersPage)
	page("GET "+p.UserByID, u.userPage)
	action("POST "+routes.UserBan, u.banUser)
	action("POST "+routes.UserUnban, u.unbanUser)
	action("POST "+routes.UserRole, u.setRole)
	action("POST "+routes.UserSessions, u.revokeSessions)
	action("POST "+routes.UserSession, u.revokeSession)
	page("GET "+p.Admins, u.adminsPage)
	action("POST "+routes.AdminInvite, u.inviteAdmin)
	action("POST "+routes.AdminDisable, u.disableAdmin)
	action("POST "+routes.AdminEnable, u.enableAdmin)
	action("POST "+routes.AdminRemove, u.removeAdmin)
	action("POST "+routes.AdminPasswordReset, u.sendAdminPasswordReset)
	action("POST "+routes.AdminResetTwoFactor, u.resetAdminTwoFactor)
	page("GET "+p.Audit, u.auditPage)
	page("GET "+p.Analytics, u.analyticsPage)

	action("POST "+routes.Logout, u.logout)
	action("POST "+routes.Locale, u.setLocale)
}

// Handler returns the UI for server.Deps.UI: Routes behind the browser
// security headers (secure), with a bare 404 for anything unrouted.
func (u *UI) Handler() http.Handler {
	mux := http.NewServeMux()
	u.Routes(mux)
	return secure(strict(mux), u.secure)
}

// wrap puts the translator and layout state on the request context.
func (u *UI) wrap(h http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		loc := i18n.Negotiate(r)
		admin, _, authed := u.userFrom(r.Context())
		ctx := i18n.WithTranslator(r.Context(), i18n.Translator{Locale: loc})
		ctx = view.WithState(ctx, view.State{
			AppName:       u.appName,
			Authenticated: authed,
			Email:         admin.Email,
			Section:       section(r.URL.Path),
			Theme:         view.ThemeFrom(r),
			Path:          pathWithoutToken(r.URL),
		})
		w.Header().Set("Content-Language", string(loc))
		w.Header().Add("Vary", "Cookie, Accept-Language")
		h(w, r.WithContext(ctx))
	})
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
func isDatastar(r *http.Request) bool {
	return r.Header.Get("Datastar-Request") == "true"
}

func (u *UI) render(w http.ResponseWriter, r *http.Request, status int, c templ.Component) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	if err := c.Render(r.Context(), w); err != nil {
		u.log.ErrorContext(r.Context(), "ui: render", "path", r.URL.Path, "err", err)
	}
}

// patch answers a Datastar action with fragments patched by id, or a plain
// form post with the full page.
func (u *UI) patch(w http.ResponseWriter, r *http.Request, status int, page templ.Component, fragments ...templ.Component) {
	if !isDatastar(r) {
		u.render(w, r, status, page)
		return
	}
	u.patchOnly(w, r, fragments...)
}

// patchOnly streams fragments to a Datastar action.
func (u *UI) patchOnly(w http.ResponseWriter, r *http.Request, fragments ...templ.Component) {
	sse := datastar.NewSSE(w, r)
	for _, f := range fragments {
		if err := sse.PatchElementTempl(f); err != nil {
			u.log.WarnContext(r.Context(), "ui: patch", "path", r.URL.Path, "err", err)
			return
		}
	}
}

// redirect navigates the browser: via SSE for Datastar, 303 otherwise.
// Set cookies before calling it.
func (u *UI) redirect(w http.ResponseWriter, r *http.Request, to string) {
	if !isDatastar(r) {
		http.Redirect(w, r, to, http.StatusSeeOther)
		return
	}
	if err := datastar.NewSSE(w, r).Redirect(to); err != nil {
		u.log.WarnContext(r.Context(), "ui: redirect", "to", to, "err", err)
	}
}

// signedIn returns the admin of a page request, or redirects to the login
// page. Signed-in pages are never cached.
func (u *UI) signedIn(w http.ResponseWriter, r *http.Request) (services.Actor, bool) {
	a, s, ok := u.userFrom(r.Context())
	if !ok {
		http.Redirect(w, r, config.AppRoutes.Login, http.StatusSeeOther)
		return services.Actor{}, false
	}
	w.Header().Set("Cache-Control", "no-store")
	return actor(a, s), true
}

// signedInAction is signedIn for actions: Datastar requests are redirected
// over SSE.
func (u *UI) signedInAction(w http.ResponseWriter, r *http.Request) (services.Actor, bool) {
	a, s, ok := u.userFrom(r.Context())
	if !ok {
		u.redirect(w, r, config.AppRoutes.Login)
		return services.Actor{}, false
	}
	return actor(a, s), true
}

// needsTwoFactor sends the admin to enrolment when a service refused them
// for lacking 2FA, and reports whether it did.
func (u *UI) needsTwoFactor(w http.ResponseWriter, r *http.Request, err error) bool {
	if !errors.Is(err, services.ErrTwoFactorRequired) {
		return false
	}
	u.redirect(w, r, config.AppRoutes.TwoFactorSetup)
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
func (u *UI) errorMessage(ctx context.Context, err error) string {
	if errors.Is(err, services.ErrAuditUnavailable) {
		u.log.ErrorContext(ctx, "ui: action refused", "err", err)
	}
	for _, e := range errorKeys {
		if errors.Is(err, e.err) {
			return i18n.T(ctx, e.key)
		}
	}
	var verr *validate.Error
	if errors.As(err, &verr) || services.KindOf(err) == services.KindInvalid {
		return i18n.T(ctx, "errors.invalidInput")
	}
	u.log.ErrorContext(ctx, "ui: error", "err", err)
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

// actor is the signed-in admin as the services see it.
func actor(a Admin, s Session) services.Actor {
	return services.Actor{AdminID: a.ID, Email: a.Email, SessionID: s.ID, TwoFactorEnabled: a.TwoFactorEnabled}
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
func pathWithoutToken(u *url.URL) string {
	q := u.Query()
	if !q.Has("token") {
		return u.RequestURI()
	}
	q.Del("token")
	out := *u
	out.RawQuery = q.Encode()
	return out.RequestURI()
}
