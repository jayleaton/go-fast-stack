package auth

import (
	"encoding/json"
	"errors"
	"math"
	"mime"
	"net/http"
	"net/url"
	"strconv"
)

// Handler serves the Better Auth JSON endpoints under BasePath. Mount it
// without stripping the prefix: mux.Handle("/api/auth/", a.Handler()).
// Responses use Better Auth's shapes ({"code","message"} on error), not the
// app envelope, so Better Auth clients (Expo) work unchanged.
func (a *Auth) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST "+BasePath+"/sign-up/email", a.handleSignUp)
	mux.HandleFunc("POST "+BasePath+"/sign-in/email", a.handleSignIn)
	mux.HandleFunc("POST "+BasePath+"/sign-out", a.handleSignOut)
	mux.HandleFunc("GET "+BasePath+"/get-session", a.handleGetSession)
	mux.HandleFunc("GET "+BasePath+"/verify-email", a.handleVerifyEmail)
	mux.HandleFunc("POST "+BasePath+"/send-verification-email", a.handleSendVerificationEmail)
	mux.HandleFunc("POST "+BasePath+"/request-password-reset", a.handleRequestPasswordReset)
	mux.HandleFunc("GET "+BasePath+"/reset-password/{token}", a.handleResetPasswordCallback)
	mux.HandleFunc("POST "+BasePath+"/reset-password", a.handleResetPassword)
	mux.HandleFunc("POST "+BasePath+"/change-password", a.handleChangePassword)
	mux.HandleFunc("POST "+BasePath+"/change-email", a.handleChangeEmail)
	mux.HandleFunc("GET "+BasePath+"/list-sessions", a.handleListSessions)
	mux.HandleFunc("POST "+BasePath+"/revoke-session", a.handleRevokeSession)
	mux.HandleFunc("POST "+BasePath+"/revoke-other-sessions", a.handleRevokeOtherSessions)
	mux.HandleFunc("GET "+BasePath+"/ok", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
	})
	return a.Middleware(mux)
}

var (
	errBadBody   = &Error{http.StatusBadRequest, "INVALID_REQUEST_BODY", "Invalid request body"}
	errMediaType = &Error{http.StatusUnsupportedMediaType, "UNSUPPORTED_MEDIA_TYPE", "Content-Type must be application/json"}
	errInternal  = &Error{http.StatusInternalServerError, "INTERNAL_SERVER_ERROR", "Internal server error"}
)

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// writeError renders err as {"code","message"}; non-auth errors are logged and
// hidden behind a 500.
func (a *Auth) writeError(w http.ResponseWriter, r *http.Request, err error) {
	if rl, ok := errors.AsType[*RateLimitError](err); ok {
		w.Header().Set("X-Retry-After", strconv.Itoa(int(math.Ceil(rl.RetryAfter.Seconds()))))
	}
	e := asError(err)
	if e == nil {
		a.log.ErrorContext(r.Context(), "auth: request failed", "path", r.URL.Path, "err", err)
		e = errInternal
	}
	writeJSON(w, e.Status, map[string]string{"code": e.Code, "message": e.Message})
}

// decode reads a JSON body (at most 64 KiB) into v. JSON-only bodies also
// force browsers into a CORS preflight for cross-site requests.
func decode(r *http.Request, v any) error {
	mt, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mt != "application/json" {
		return errMediaType
	}
	if err := json.NewDecoder(http.MaxBytesReader(nil, r.Body, 64<<10)).Decode(v); err != nil {
		return errBadBody
	}
	return nil
}

// signedIn writes the session cookie and, like the bearer plugin, exposes the
// signed token in the set-auth-token header for clients without cookies.
func (a *Auth) signedIn(w http.ResponseWriter, s Session) {
	a.SetSessionCookie(w, s)
	w.Header().Set("Set-Auth-Token", a.signValue(s.Token))
	w.Header().Add("Access-Control-Expose-Headers", "set-auth-token")
}

func (a *Auth) handleSignUp(w http.ResponseWriter, r *http.Request) {
	var in SignUpInput
	if err := a.checkFormCSRF(r); err != nil {
		a.writeError(w, r, err)
		return
	}
	if err := decode(r, &in); err != nil {
		a.writeError(w, r, err)
		return
	}
	user, sess, err := a.SignUpEmail(r.Context(), in)
	if err != nil {
		a.writeError(w, r, err)
		return
	}
	var token *string
	if sess.Token != "" {
		a.signedIn(w, sess)
		token = &sess.Token
	}
	writeJSON(w, http.StatusOK, map[string]any{"token": token, "user": user})
}

func (a *Auth) handleSignIn(w http.ResponseWriter, r *http.Request) {
	var in SignInInput
	if err := a.checkFormCSRF(r); err != nil {
		a.writeError(w, r, err)
		return
	}
	if err := decode(r, &in); err != nil {
		a.writeError(w, r, err)
		return
	}
	user, sess, err := a.SignInEmail(r.Context(), in)
	if err != nil {
		a.writeError(w, r, err)
		return
	}
	a.signedIn(w, sess)
	body := map[string]any{"redirect": in.CallbackURL != "", "token": sess.Token, "user": user}
	if in.CallbackURL != "" {
		w.Header().Set("Location", in.CallbackURL)
		body["url"] = in.CallbackURL
	}
	writeJSON(w, http.StatusOK, body)
}

func (a *Auth) handleSignOut(w http.ResponseWriter, r *http.Request) {
	if err := a.validateOrigin(r, false); err != nil {
		a.writeError(w, r, err)
		return
	}
	// Revoke the presented session and, when a bearer token and a cookie
	// both came in, the cookie's session too.
	tokens := []string{metaFrom(r.Context()).token}
	if c, err := r.Cookie(a.cookieName()); err == nil {
		if t, ok := a.verifySigned(c.Value); ok && isTokenString(t) {
			tokens = append(tokens, t)
		}
	}
	for _, t := range tokens {
		if err := a.SignOut(r.Context(), t); err != nil {
			a.writeError(w, r, err)
			return
		}
	}
	a.ClearSessionCookie(w)
	writeJSON(w, http.StatusOK, map[string]bool{"success": true})
}

func (a *Auth) handleGetSession(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Pragma", "no-cache")
	user, sess, ok := UserFrom(r.Context())
	if !ok {
		writeJSON(w, http.StatusOK, nil)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"session": sess, "user": user})
}

// withQuery sets one query parameter on a redirect target (absolute or root-relative).
func (a *Auth) withQuery(target, key, value string) string {
	u, err := url.Parse(target)
	if err != nil {
		return target
	}
	q := u.Query()
	q.Set(key, value)
	u.RawQuery = q.Encode()
	return u.String()
}

func (a *Auth) handleVerifyEmail(w http.ResponseWriter, r *http.Request) {
	callbackURL := r.URL.Query().Get("callbackURL")
	if err := a.checkCallbackURL(callbackURL, ErrInvalidCallbackURL); err != nil {
		a.writeError(w, r, err)
		return
	}
	user, sess, changed, err := a.verifyEmail(r.Context(), r.URL.Query().Get("token"))
	if err != nil {
		e := asError(err)
		if e == nil {
			a.writeError(w, r, err)
			return
		}
		if callbackURL != "" {
			http.Redirect(w, r, a.withQuery(callbackURL, "error", e.Code), http.StatusFound) //nolint:gosec // callbackURL passed checkCallbackURL
			return
		}
		writeJSON(w, http.StatusUnauthorized, map[string]string{"code": e.Code, "message": e.Message})
		return
	}
	if sess.Token != "" {
		a.signedIn(w, sess)
	}
	if callbackURL != "" {
		http.Redirect(w, r, callbackURL, http.StatusFound) //nolint:gosec // callbackURL passed checkCallbackURL
		return
	}
	if changed {
		writeJSON(w, http.StatusOK, map[string]any{"status": true, "user": user})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": true, "user": nil})
}

func (a *Auth) handleSendVerificationEmail(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Email       string `json:"email"`
		CallbackURL string `json:"callbackURL"`
	}
	if err := a.validateOrigin(r, false); err != nil {
		a.writeError(w, r, err)
		return
	}
	if err := decode(r, &in); err != nil {
		a.writeError(w, r, err)
		return
	}
	if err := a.SendVerificationEmail(r.Context(), in.Email, in.CallbackURL); err != nil {
		a.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"status": true})
}

func (a *Auth) handleRequestPasswordReset(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Email      string `json:"email"`
		RedirectTo string `json:"redirectTo"`
	}
	if err := a.validateOrigin(r, false); err != nil {
		a.writeError(w, r, err)
		return
	}
	if err := decode(r, &in); err != nil {
		a.writeError(w, r, err)
		return
	}
	if err := a.RequestPasswordReset(r.Context(), in.Email, in.RedirectTo); err != nil {
		a.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"status":  true,
		"message": "If this email exists in our system, check your email for the reset link",
	})
}

// handleResetPasswordCallback is the link in the reset email: it checks the
// token and redirects to callbackURL with ?token= (or ?error=INVALID_TOKEN).
func (a *Auth) handleResetPasswordCallback(w http.ResponseWriter, r *http.Request) {
	callbackURL := r.URL.Query().Get("callbackURL")
	if err := a.checkCallbackURL(callbackURL, ErrInvalidCallbackURL); err != nil {
		a.writeError(w, r, err)
		return
	}
	token := r.PathValue("token")
	valid, err := a.resetTokenValid(r.Context(), token)
	if err != nil {
		a.writeError(w, r, err)
		return
	}
	if callbackURL == "" {
		a.writeError(w, r, ErrInvalidToken)
		return
	}
	if !valid {
		http.Redirect(w, r, a.withQuery(callbackURL, "error", ErrInvalidToken.Code), http.StatusFound) //nolint:gosec // callbackURL passed checkCallbackURL
		return
	}
	http.Redirect(w, r, a.withQuery(callbackURL, "token", token), http.StatusFound) //nolint:gosec // callbackURL passed checkCallbackURL
}

func (a *Auth) handleResetPassword(w http.ResponseWriter, r *http.Request) {
	var in struct {
		NewPassword string `json:"newPassword"`
		Token       string `json:"token"`
	}
	if err := a.validateOrigin(r, false); err != nil {
		a.writeError(w, r, err)
		return
	}
	if err := decode(r, &in); err != nil {
		a.writeError(w, r, err)
		return
	}
	if in.Token == "" {
		in.Token = r.URL.Query().Get("token")
	}
	if err := a.ResetPassword(r.Context(), in.Token, in.NewPassword); err != nil {
		a.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"status": true})
}
