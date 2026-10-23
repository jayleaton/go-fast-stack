package ui

import (
	"net/http"

	"github.com/jayleaton/go-fast-stack/apps/web/internal/apiclient"
)

// The one way a cookie from the API reaches the browser: a UI server action
// sets what an API result returned (the email and password flows'
// AuthResult.Cookies, the social flows' SocialStart / SocialOutcome
// Cookies), before it redirects or renders.

// setCookies sets cookies the API returned as data (the auth session and
// sign-in flow state): Path=/, HttpOnly, SameSite=Lax, Secure as the API says. A
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
