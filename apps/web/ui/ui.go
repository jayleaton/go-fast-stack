package ui

import (
	"context"
	"crypto/rand"
	"errors"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"
	"unicode"

	"github.com/a-h/templ"
	"github.com/starfederation/datastar-go/datastar"

	"github.com/jayleaton/go-fast-stack/apps/web/internal/apiclient"
	"github.com/jayleaton/go-fast-stack/apps/web/internal/config"
	"github.com/jayleaton/go-fast-stack/apps/web/ui/i18n"
	"github.com/jayleaton/go-fast-stack/apps/web/ui/routes"
	"github.com/jayleaton/go-fast-stack/apps/web/ui/static"
)

// UI serves the HTML pages and their Datastar actions.
type UI struct {
	auth     Auth
	gate     *apiclient.Gate
	api      apiclient.Client
	features config.Features
	appName  string
	secure   bool
	log      *slog.Logger
	now      func() time.Time
	interval time.Duration
	hub      *hub
	csrf     *http.CrossOriginProtection
}

// New builds the UI. Auth, API and Gate are required.
func New(deps Deps) *UI {
	if deps.Auth == nil || deps.API == nil || deps.Gate == nil {
		panic("ui.New: Auth, API and Gate are required")
	}
	ui := &UI{
		auth:     deps.Auth,
		gate:     deps.Gate,
		api:      deps.API,
		features: deps.Features,
		appName:  deps.AppName,
		secure:   deps.SecureCookies,
		log:      deps.Logger,
		now:      deps.Now,
		interval: deps.LiveInterval,
		hub:      newHub(),
		csrf:     http.NewCrossOriginProtection(),
	}
	if ui.appName == "" {
		ui.appName = config.DefaultAppName
	}
	if ui.log == nil {
		ui.log = slog.Default()
	}
	if ui.now == nil {
		ui.now = time.Now
	}
	if ui.interval <= 0 {
		ui.interval = time.Second
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

	// Static assets are the same for everyone and never read the session.
	mux.Handle("GET "+routes.Static, static.Handler())
	mux.Handle("GET /favicon.ico", http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		http.Redirect(writer, request, static.URL(static.FaviconFile), http.StatusMovedPermanently)
	}))

	page(apiclient.Public, "GET /{$}", ui.home)
	page(apiclient.Public, "GET "+config.AppRoutes.Why, ui.why)
	page(apiclient.Public, "GET "+config.AppRoutes.Login, ui.loginPage)
	action(apiclient.Public, "POST "+config.AppRoutes.Login, ui.login)
	page(apiclient.Public, "GET "+config.AppRoutes.Signup, ui.signupPage)
	action(apiclient.Public, "POST "+config.AppRoutes.Signup, ui.signup)
	page(apiclient.Public, "GET "+config.AppRoutes.ForgotPassword, ui.forgotPasswordPage)
	action(apiclient.Public, "POST "+config.AppRoutes.ForgotPassword, ui.forgotPassword)
	page(apiclient.Public, "GET "+config.AppRoutes.ResetPassword, ui.resetPasswordPage)
	action(apiclient.Public, "POST "+config.AppRoutes.ResetPassword, ui.resetPassword)
	page(apiclient.Public, "GET "+config.AppRoutes.VerifyEmail, ui.verifyEmailPage)
	action(apiclient.Public, "POST "+config.AppRoutes.VerifyEmail, ui.resendVerification)
	page(apiclient.Public, "GET "+config.AppRoutes.VerifyEmailSuccess, ui.verifyEmailSuccessPage)
	action(apiclient.Public, "POST "+config.AppRoutes.VerifyEmailSuccess, ui.verifyEmail)
	page(apiclient.SignedIn, "GET "+config.AppRoutes.Dashboard, ui.dashboard)
	page(apiclient.SignedIn, "GET "+routes.DashboardLive, ui.dashboardLive)
	page(apiclient.SignedIn, "GET "+config.AppRoutes.Account, ui.accountPage)
	action(apiclient.SignedIn, "POST "+routes.AccountProfile, ui.updateProfile)
	action(apiclient.SignedIn, "POST "+routes.AccountEmail, ui.requestEmailChange)
	action(apiclient.SignedIn, "POST "+routes.AccountPassword, ui.changePassword)
	action(apiclient.SignedIn, "POST "+routes.AccountRevokeSession, ui.revokeSession)
	action(apiclient.SignedIn, "POST "+routes.AccountRevokeOthers, ui.revokeOtherSessions)
	action(apiclient.SignedIn, "POST "+routes.AccountDelete, ui.deleteAccount)
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
	sse := datastar.NewSSE(writer, request)
	for _, component := range fragments {
		var opts []datastar.PatchElementOption
		if rf, ok := component.(replaced); ok {
			component, opts = rf.Component, []datastar.PatchElementOption{datastar.WithModeReplace()}
		}
		if err := sse.PatchElementTempl(component, opts...); err != nil {
			ui.log.WarnContext(request.Context(), "ui: patch", "path", request.URL.Path, "err", err)
			return
		}
	}
}

// replaced patches a fragment without morphing: the element is swapped, so
// typed input values (passwords) are cleared. Default patches morph and keep them.
type replaced struct{ templ.Component }

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

// errorMessage translates a service error; unknown errors are logged.
func (ui *UI) errorMessage(ctx context.Context, err error) string {
	for _, authError := range AuthErrors {
		if errors.Is(err, authError.Err) {
			return i18n.T(ctx, authError.Key)
		}
	}
	ui.log.ErrorContext(ctx, "ui: service error", "err", err)
	return i18n.T(ctx, "errors.somethingWentWrong")
}

// userFromService converts the service's user row; both describe the
// auth-owned "user" table.
func userFromService(session apiclient.User) User {
	return User(session)
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

func newTabID() string {
	return rand.Text()[:12]
}

// pathWithoutToken is the request's path and query minus an emailed link's
// token: the layout echoes the path (the language switcher's "next"), and a
// token may only appear in its page's own hidden form field.
func pathWithoutToken(link *url.URL) string {
	query := link.Query()
	if !query.Has("token") {
		return link.RequestURI()
	}
	query.Del("token")
	out := *link
	out.RawQuery = query.Encode()
	return out.RequestURI()
}
