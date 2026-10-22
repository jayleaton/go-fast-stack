package ui

import (
	"context"
	"crypto/rand"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/a-h/templ"
	"github.com/starfederation/datastar-go/datastar"

	"github.com/jayleaton/ai-patterns-go/apps/web/ui/i18n"
	"github.com/jayleaton/ai-patterns-go/apps/web/ui/routes"
	"github.com/jayleaton/ai-patterns-go/apps/web/ui/static"
	"github.com/jayleaton/ai-patterns-go/apps/web/ui/view"
)

// UI serves the HTML pages and their Datastar actions.
type UI struct {
	auth     Auth
	users    Users
	features Features
	secure   bool
	log      *slog.Logger
	now      func() time.Time
	interval time.Duration
	hub      *hub
	csrf     *http.CrossOriginProtection
}

// New builds the UI. Auth and Users are required.
func New(d Deps) *UI {
	if d.Auth == nil || d.Users == nil {
		panic("ui.New: Auth and Users are required")
	}
	u := &UI{
		auth:     d.Auth,
		users:    d.Users,
		features: d.Features,
		secure:   d.SecureCookies,
		log:      d.Logger,
		now:      d.Now,
		interval: d.LiveInterval,
		hub:      newHub(),
		csrf:     http.NewCrossOriginProtection(),
	}
	if u.log == nil {
		u.log = slog.Default()
	}
	if u.now == nil {
		u.now = time.Now
	}
	if u.interval <= 0 {
		u.interval = time.Second
	}
	return u
}

// Routes mounts every page, action and static asset on mux. The auth
// middleware must wrap mux so Auth.UserFrom sees the session.
func (u *UI) Routes(mux *http.ServeMux) {
	page := func(pattern string, h http.HandlerFunc) { mux.Handle(pattern, u.wrap(h)) }
	// Actions also pass Go's cross-origin protection (Sec-Fetch-Site / Origin).
	action := func(pattern string, h http.HandlerFunc) { mux.Handle(pattern, u.csrf.Handler(u.wrap(h))) }

	mux.Handle("GET "+routes.Static, static.Handler())
	mux.HandleFunc("GET /favicon.ico", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, static.URL(static.FaviconFile), http.StatusMovedPermanently)
	})

	page("GET /{$}", u.home)
	page("GET "+routes.Why, u.why)
	page("GET "+routes.Login, u.loginPage)
	action("POST "+routes.Login, u.login)
	page("GET "+routes.Signup, u.signupPage)
	action("POST "+routes.Signup, u.signup)
	page("GET "+routes.ForgotPassword, u.forgotPasswordPage)
	action("POST "+routes.ForgotPassword, u.forgotPassword)
	page("GET "+routes.ResetPassword, u.resetPasswordPage)
	action("POST "+routes.ResetPassword, u.resetPassword)
	page("GET "+routes.VerifyEmail, u.verifyEmailPage)
	action("POST "+routes.VerifyEmail, u.resendVerification)
	page("GET "+routes.VerifyEmailSuccess, u.verifyEmailSuccessPage)
	action("POST "+routes.VerifyEmailSuccess, u.verifyEmail)
	page("GET "+routes.Dashboard, u.dashboard)
	page("GET "+routes.DashboardLive, u.dashboardLive)
	action("POST "+routes.UpdateSettings, u.updateSettings)
	action("POST "+routes.DeleteAccount, u.deleteAccount)
	action("POST "+routes.Logout, u.logout)
	action("POST "+routes.Locale, u.setLocale)
}

// wrap puts the translator and layout state on the request context.
func (u *UI) wrap(h http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		loc := i18n.Negotiate(r)
		_, _, authed := u.auth.UserFrom(r.Context())
		ctx := i18n.WithTranslator(r.Context(), i18n.Translator{Locale: loc})
		ctx = view.WithState(ctx, view.State{
			Authenticated: authed,
			Theme:         view.ThemeFrom(r),
			Path:          r.URL.RequestURI(),
		})
		w.Header().Set("Content-Language", string(loc))
		w.Header().Add("Vary", "Cookie, Accept-Language")
		h(w, r.WithContext(ctx))
	})
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

// errorMessage translates a service error; unknown errors are logged.
func (u *UI) errorMessage(ctx context.Context, err error) string {
	for _, e := range errorKeys {
		if errors.Is(err, e.err) {
			return i18n.T(ctx, e.key)
		}
	}
	u.log.ErrorContext(ctx, "ui: service error", "err", err)
	return i18n.T(ctx, "errors.somethingWentWrong")
}

// safeNext keeps redirects on this site.
func safeNext(next string) string {
	if !strings.HasPrefix(next, "/") || strings.HasPrefix(next, "//") || strings.HasPrefix(next, `/\`) {
		return routes.Home
	}
	return next
}

func newTabID() string {
	return rand.Text()[:12]
}
