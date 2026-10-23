package ui

import (
	"errors"
	"net/http"

	"github.com/jayleaton/go-fast-stack/apps/admin/internal/apiclient"
	"github.com/jayleaton/go-fast-stack/apps/admin/internal/config"
	"github.com/jayleaton/go-fast-stack/apps/admin/ui/i18n"
	"github.com/jayleaton/go-fast-stack/apps/admin/ui/view"
)

// handlerFunc is a page or Datastar action: it gets the caller the gate
// admitted (the zero Caller on a public route without a session).
type handlerFunc func(writer http.ResponseWriter, request *http.Request, caller apiclient.Caller)

// route is the one route handler of the admin UI, the counterpart of
// api.Route. The gate resolves the caller and enforces policy with the same
// policies as the API; only the answers differ: signed out goes to the login
// page (over SSE for Datastar), denied is a 403, a failed lookup a 500. An
// admitted request gets the translator and layout state on its context.
// Responses are never stored: secure sets no-store on every page.
func (ui *UI) route(policy apiclient.Policy, handler handlerFunc) http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		locale := i18n.Negotiate(request)
		writer.Header().Set("Content-Language", string(locale))
		writer.Header().Add("Vary", "Cookie, Accept-Language")
		ctx := i18n.WithTranslator(request.Context(), i18n.Translator{Locale: locale})
		request = request.WithContext(ctx)

		caller, request, err := ui.gate.Admit(request, policy)
		if err != nil {
			ui.apiFailed(writer, request, err)
			return
		}

		ctx = view.WithState(request.Context(), view.State{
			AppName:       ui.appName,
			Authenticated: caller.SignedIn(),
			Email:         caller.Admin.Email,
			Section:       section(request.URL.Path),
			Theme:         view.ThemeFrom(request),
			Path:          pathWithoutToken(request.URL),
		})
		handler(writer, request.WithContext(ctx), caller)
	})
}

// apiFailed answers a request the gate or the API refused outright, with
// the same outcome for a page's own policy and for an API call it makes:
// signed out goes to the login page (over SSE for Datastar), denied is a
// 403, anything else a logged 500.
func (ui *UI) apiFailed(writer http.ResponseWriter, request *http.Request, err error) {
	ctx := request.Context()
	switch {
	case errors.Is(err, apiclient.ErrSignedOut):
		ui.redirect(writer, request, config.AppRoutes.Login)
	case errors.Is(err, apiclient.ErrDenied):
		ui.log.InfoContext(ctx, "ui: access denied", "path", request.URL.Path, "err", err)
		http.Error(writer, i18n.T(ctx, "errors.accessDenied"), http.StatusForbidden)
	default:
		ui.log.ErrorContext(ctx, "ui: request failed", "path", request.URL.Path, "err", err)
		http.Error(writer, i18n.T(ctx, "errors.somethingWentWrong"), http.StatusInternalServerError)
	}
}
