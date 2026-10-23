package ui

import (
	"context"
	"errors"
	"net/http"
	"net/url"

	"github.com/jayleaton/go-fast-stack/apps/web/internal/apiclient"
	"github.com/jayleaton/go-fast-stack/apps/web/internal/config"
)

// maxCallbackForm caps Apple's form_post to the callback page (64 KiB).
const maxCallbackForm = 64 << 10

// callbackFields are the provider callback parameters the API reads.
// Apple's front-channel id_token is not one of them, so it goes no further.
var callbackFields = []string{"code", "state", "error", "user"}

// socialProviders lists the configured Google / Apple providers for the
// buttons: none when the API cannot say (email sign-in still works).
func (ui *UI) socialProviders(ctx context.Context) []string {
	providers, err := ui.api.SocialProviders(ctx)
	if err != nil {
		ui.log.WarnContext(ctx, "ui: listing sign-in providers", "err", err)
		return nil
	}
	return providers
}

// socialSignIn starts Google / Apple sign-in: the API returns the provider
// URL and the state cookie that binds the flow to this browser; the action
// sets the cookie and answers with a Datastar redirect to the provider (a
// plain form POST would be refused by the CSP: form-action 'self' also
// covers the redirect after a submit). The provider sends the browser back
// to socialCallback.
func (ui *UI) socialSignIn(writer http.ResponseWriter, request *http.Request, _ apiclient.Caller) {
	ctx := request.Context()
	start, err := ui.api.StartSocialSignIn(ctx, request.PathValue("provider"), apiclient.StartSocialSignIn{CallbackURL: config.AppRoutes.Dashboard})
	if err != nil {
		if ui.refused(writer, request, err) {
			return
		}
		code := "failed"
		switch {
		case errors.Is(err, apiclient.ErrRateLimited):
			code = "too_many_requests"
		case !errors.Is(err, apiclient.ErrProviderNotFound):
			ui.log.ErrorContext(ctx, "ui: starting social sign-in", "err", err)
		}
		ui.redirect(writer, request, config.AppRoutes.Login+"?"+url.Values{"error": {code}}.Encode())
		return
	}
	setCookies(writer, start.Cookies)
	ui.redirect(writer, request, start.URL)
}

// socialCallback is where Google and Apple send the browser back (GET, or
// Apple's cross-site form POST). It hands the callback parameters and the
// browser's cookies to the API, sets the cookies it returns (the session,
// clearing the state) and follows its redirect: the page the sign-in
// started for, the login page with ?error=, or the mobile app's deep link.
func (ui *UI) socialCallback(writer http.ResponseWriter, request *http.Request, _ apiclient.Caller) {
	ctx := request.Context()
	input := apiclient.FinishSocialSignIn{Params: map[string]string{}, Cookies: map[string]string{}}
	params := request.URL.Query()
	if request.Method == http.MethodPost {
		request.Body = http.MaxBytesReader(writer, request.Body, maxCallbackForm)
		if err := request.ParseForm(); err != nil {
			http.Redirect(writer, request, config.AppRoutes.Login+"?"+url.Values{"error": {"invalid_callback_request"}}.Encode(), http.StatusFound)
			return
		}
		input.FormPost, params = true, request.Form
	}
	for _, field := range callbackFields {
		if value := params.Get(field); value != "" {
			input.Params[field] = value
		}
	}
	for _, cookie := range request.Cookies() {
		input.Cookies[cookie.Name] = cookie.Value
	}
	outcome, err := ui.api.FinishSocialSignIn(ctx, request.PathValue("provider"), input)
	if err != nil {
		if ui.refused(writer, request, err) {
			return
		}
		ui.log.ErrorContext(ctx, "ui: finishing social sign-in", "err", err)
		http.Redirect(writer, request, config.AppRoutes.Login+"?"+url.Values{"error": {"failed"}}.Encode(), http.StatusFound)
		return
	}
	setCookies(writer, outcome.Cookies)
	http.Redirect(writer, request, outcome.Redirect, http.StatusFound) //nolint:gosec // G710: the API's redirect is a callback it checked when the flow started, or its own error page
}

// setCookies sets cookies the API returned as data (the auth session and
// flow state): Path=/, HttpOnly, SameSite=Lax, Secure as the API says. A
// negative MaxAge deletes the cookie.
func setCookies(writer http.ResponseWriter, cookies []apiclient.Cookie) {
	for _, cookie := range cookies {
		http.SetCookie(writer, &http.Cookie{ //nolint:gosec // G124: Secure follows the API (PUBLIC_URL's scheme) so http dev servers work
			Name:     cookie.Name,
			Value:    cookie.Value,
			Path:     "/",
			MaxAge:   cookie.MaxAge,
			Secure:   cookie.Secure,
			HttpOnly: true,
			SameSite: http.SameSiteLaxMode,
		})
	}
}
