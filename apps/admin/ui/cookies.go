package ui

import (
	"net/http"

	"github.com/jayleaton/go-fast-stack/apps/admin/internal/apiclient"
)

// setCookies sets the cookies an API sign-in flow returned as data (the
// session, the two-factor challenge, the trusted device): Path=/, HttpOnly,
// SameSite=Lax, Secure as the API says; a negative MaxAge deletes the
// cookie. It is the only way a cookie from the API reaches the browser;
// call it before redirecting or rendering.
func setCookies(writer http.ResponseWriter, cookies []apiclient.Cookie) {
	for _, cookie := range cookies {
		http.SetCookie(writer, &http.Cookie{ //nolint:gosec // G124: Secure follows the API (ADMIN_PUBLIC_URL's scheme) so http dev servers work
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
