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
func (auth *Auth) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST "+BasePath+"/sign-up/email", auth.handleSignUp)
	mux.HandleFunc("POST "+BasePath+"/sign-in/email", auth.handleSignIn)
	mux.HandleFunc("POST "+BasePath+"/sign-out", auth.handleSignOut)
	mux.HandleFunc("GET "+BasePath+"/get-session", auth.handleGetSession)
	mux.HandleFunc("GET "+BasePath+"/verify-email", auth.handleVerifyEmail)
	mux.HandleFunc("POST "+BasePath+"/send-verification-email", auth.handleSendVerificationEmail)
	mux.HandleFunc("POST "+BasePath+"/request-password-reset", auth.handleRequestPasswordReset)
	mux.HandleFunc("GET "+BasePath+"/reset-password/{token}", auth.handleResetPasswordCallback)
	mux.HandleFunc("POST "+BasePath+"/reset-password", auth.handleResetPassword)
	mux.HandleFunc("POST "+BasePath+"/change-password", auth.handleChangePassword)
	mux.HandleFunc("POST "+BasePath+"/change-email", auth.handleChangeEmail)
	mux.HandleFunc("GET "+BasePath+"/list-sessions", auth.handleListSessions)
	mux.HandleFunc("POST "+BasePath+"/revoke-session", auth.handleRevokeSession)
	mux.HandleFunc("POST "+BasePath+"/revoke-other-sessions", auth.handleRevokeOtherSessions)
	mux.HandleFunc("POST "+BasePath+"/set-password", auth.handleSetPassword)
	mux.HandleFunc("POST "+BasePath+"/two-factor/enable", auth.handleTwoFactorEnable)
	mux.HandleFunc("POST "+BasePath+"/two-factor/get-totp-uri", auth.handleTwoFactorURI)
	mux.HandleFunc("POST "+BasePath+"/two-factor/verify-totp", auth.handleTwoFactorVerify(TwoFactorMethodTOTP))
	mux.HandleFunc("POST "+BasePath+"/two-factor/verify-backup-code", auth.handleTwoFactorVerify(TwoFactorMethodBackup))
	mux.HandleFunc("POST "+BasePath+"/two-factor/generate-backup-codes", auth.handleTwoFactorBackupCodes)
	mux.HandleFunc("POST "+BasePath+"/two-factor/disable", auth.handleTwoFactorDisable)
	mux.HandleFunc("GET "+BasePath+"/social-providers", auth.handleSocialProviders)
	mux.HandleFunc("POST "+BasePath+"/sign-in/social", auth.handleSignInSocial)
	mux.HandleFunc("POST "+BasePath+"/sign-in/handoff", auth.handleSignInHandoff)
	if auth.cfg.SocialCallbackPath == "" {
		callback := func(writer http.ResponseWriter, request *http.Request) {
			auth.SocialCallback(writer, request, request.PathValue("provider"))
		}
		mux.HandleFunc("GET "+BasePath+"/callback/{provider}", callback)
		mux.HandleFunc("POST "+BasePath+"/callback/{provider}", callback)
	}
	mux.HandleFunc("GET "+BasePath+"/ok", func(writer http.ResponseWriter, _ *http.Request) {
		writeJSON(writer, http.StatusOK, map[string]bool{"ok": true})
	})
	return auth.Middleware(mux)
}

var (
	errBadBody   = &Error{http.StatusBadRequest, "INVALID_REQUEST_BODY", "Invalid request body"}
	errMediaType = &Error{http.StatusUnsupportedMediaType, "UNSUPPORTED_MEDIA_TYPE", "Content-Type must be application/json"}
	errInternal  = &Error{http.StatusInternalServerError, "INTERNAL_SERVER_ERROR", "Internal server error"}
)

func writeJSON(writer http.ResponseWriter, status int, value any) {
	writer.Header().Set("Content-Type", "application/json")
	writer.WriteHeader(status)
	_ = json.NewEncoder(writer).Encode(value)
}

// writeError renders err as {"code","message"}; non-auth errors are logged and
// hidden behind a 500.
func (auth *Auth) writeError(writer http.ResponseWriter, request *http.Request, err error) {
	if rl, ok := errors.AsType[*RateLimitError](err); ok {
		writer.Header().Set("X-Retry-After", strconv.Itoa(int(math.Ceil(rl.RetryAfter.Seconds()))))
	}
	authErr := asError(err)
	if authErr == nil {
		auth.log.ErrorContext(request.Context(), "auth: request failed", "path", request.URL.Path, "err", err)
		authErr = errInternal
	}
	writeJSON(writer, authErr.Status, map[string]string{"code": authErr.Code, "message": authErr.Message})
}

const maxJSONBody = 64 << 10 // 64 KiB cap on JSON request bodies

// decode reads a JSON body (at most maxJSONBody) into v. JSON-only bodies also
// force browsers into a CORS preflight for cross-site requests.
func decode(request *http.Request, value any) error {
	mt, _, err := mime.ParseMediaType(request.Header.Get("Content-Type"))
	if err != nil || mt != "application/json" {
		return errMediaType
	}
	if err := json.NewDecoder(http.MaxBytesReader(nil, request.Body, maxJSONBody)).Decode(value); err != nil {
		return errBadBody
	}
	return nil
}

// signedIn writes the session cookie and, like the bearer plugin, exposes the
// signed token in the set-auth-token header for clients without cookies.
func (auth *Auth) signedIn(writer http.ResponseWriter, session Session) {
	auth.SetSessionCookie(writer, session)
	writer.Header().Set("Set-Auth-Token", auth.signValue(session.Token))
	writer.Header().Add("Access-Control-Expose-Headers", "set-auth-token")
}

func (auth *Auth) handleSignUp(writer http.ResponseWriter, request *http.Request) {
	var in SignUpInput
	if err := auth.checkFormCSRF(request); err != nil {
		auth.writeError(writer, request, err)
		return
	}
	if err := decode(request, &in); err != nil {
		auth.writeError(writer, request, err)
		return
	}
	user, sess, err := auth.SignUpEmail(request.Context(), in)
	if auth.writeChallenge(writer, err) {
		return
	}
	if err != nil {
		auth.writeError(writer, request, err)
		return
	}
	var token *string
	if sess.Token != "" {
		auth.signedIn(writer, sess)
		token = &sess.Token
	}
	writeJSON(writer, http.StatusOK, map[string]any{"token": token, "user": user})
}

func (auth *Auth) handleSignIn(writer http.ResponseWriter, request *http.Request) {
	var in SignInInput
	if err := auth.checkFormCSRF(request); err != nil {
		auth.writeError(writer, request, err)
		return
	}
	if err := decode(request, &in); err != nil {
		auth.writeError(writer, request, err)
		return
	}
	user, sess, err := auth.SignInEmail(request.Context(), in)
	if auth.writeChallenge(writer, err) {
		return
	}
	if err != nil {
		auth.writeError(writer, request, err)
		return
	}
	auth.signedIn(writer, sess)
	body := map[string]any{"redirect": in.CallbackURL != "", "token": sess.Token, "user": user}
	if in.CallbackURL != "" {
		writer.Header().Set("Location", in.CallbackURL)
		body["url"] = in.CallbackURL
	}
	writeJSON(writer, http.StatusOK, body)
}

func (auth *Auth) handleSignOut(writer http.ResponseWriter, request *http.Request) {
	if err := auth.validateOrigin(request, false); err != nil {
		auth.writeError(writer, request, err)
		return
	}
	// Revoke the presented session and, when a bearer token and a cookie
	// both came in, the cookie's session too.
	tokens := []string{auth.meta(request.Context()).token}
	if cookie, err := request.Cookie(auth.cookieName()); err == nil {
		if signed, ok := auth.verifySigned(cookie.Value); ok && isTokenString(signed) {
			tokens = append(tokens, signed)
		}
	}
	for _, token := range tokens {
		if err := auth.SignOut(request.Context(), token); err != nil {
			auth.writeError(writer, request, err)
			return
		}
	}
	auth.ClearSessionCookie(writer)
	writeJSON(writer, http.StatusOK, map[string]bool{"success": true})
}

func (auth *Auth) handleGetSession(writer http.ResponseWriter, request *http.Request) {
	writer.Header().Set("Cache-Control", "no-store")
	writer.Header().Set("Pragma", "no-cache")
	user, sess, ok := auth.userFrom(request.Context())
	if !ok {
		writeJSON(writer, http.StatusOK, nil)
		return
	}
	writeJSON(writer, http.StatusOK, map[string]any{"session": sess, "user": user})
}

// withQuery sets one query parameter on a redirect target (absolute or root-relative).
func (auth *Auth) withQuery(target, key, value string) string {
	link, err := url.Parse(target)
	if err != nil {
		return target
	}
	values := link.Query()
	values.Set(key, value)
	link.RawQuery = values.Encode()
	return link.String()
}

func (auth *Auth) handleVerifyEmail(writer http.ResponseWriter, request *http.Request) {
	callbackURL := request.URL.Query().Get("callbackURL")
	if err := auth.checkCallbackURL(callbackURL, ErrInvalidCallbackURL); err != nil {
		auth.writeError(writer, request, err)
		return
	}
	if auth.cfg.VerifyEmailPath != "" {
		// A GET never spends the token (scanners prefetch links): links
		// emailed before VerifyEmailPath was set go to the confirm page.
		http.Redirect(writer, request, auth.verificationLink(request.URL.Query().Get("token"), callbackURL), http.StatusFound)
		return
	}
	user, sess, changed, err := auth.verifyEmail(request.Context(), request.URL.Query().Get("token"))
	if err != nil {
		authErr := asError(err)
		if authErr == nil {
			auth.writeError(writer, request, err)
			return
		}
		if callbackURL != "" {
			http.Redirect(writer, request, auth.withQuery(callbackURL, "error", authErr.Code), http.StatusFound) //nolint:gosec // callbackURL passed checkCallbackURL
			return
		}
		writeJSON(writer, http.StatusUnauthorized, map[string]string{"code": authErr.Code, "message": authErr.Message})
		return
	}
	if sess.Token != "" {
		auth.signedIn(writer, sess)
	}
	if callbackURL != "" {
		http.Redirect(writer, request, callbackURL, http.StatusFound) //nolint:gosec // callbackURL passed checkCallbackURL
		return
	}
	if changed {
		writeJSON(writer, http.StatusOK, map[string]any{"status": true, "user": user})
		return
	}
	writeJSON(writer, http.StatusOK, map[string]any{"status": true, "user": nil})
}

func (auth *Auth) handleSendVerificationEmail(writer http.ResponseWriter, request *http.Request) {
	var in struct {
		Email       string `json:"email"`
		CallbackURL string `json:"callbackURL"`
	}
	if err := auth.validateOrigin(request, false); err != nil {
		auth.writeError(writer, request, err)
		return
	}
	if err := decode(request, &in); err != nil {
		auth.writeError(writer, request, err)
		return
	}
	if err := auth.SendVerificationEmail(request.Context(), in.Email, in.CallbackURL); err != nil {
		auth.writeError(writer, request, err)
		return
	}
	writeJSON(writer, http.StatusOK, map[string]bool{"status": true})
}

func (auth *Auth) handleRequestPasswordReset(writer http.ResponseWriter, request *http.Request) {
	var in struct {
		Email      string `json:"email"`
		RedirectTo string `json:"redirectTo"`
	}
	if err := auth.validateOrigin(request, false); err != nil {
		auth.writeError(writer, request, err)
		return
	}
	if err := decode(request, &in); err != nil {
		auth.writeError(writer, request, err)
		return
	}
	if err := auth.RequestPasswordReset(request.Context(), in.Email, in.RedirectTo); err != nil {
		auth.writeError(writer, request, err)
		return
	}
	writeJSON(writer, http.StatusOK, map[string]any{
		"status":  true,
		"message": "If this email exists in our system, check your email for the reset link",
	})
}

// handleResetPasswordCallback is the link in the reset email: it checks the
// token and redirects to callbackURL with ?token= (or ?error=INVALID_TOKEN).
func (auth *Auth) handleResetPasswordCallback(writer http.ResponseWriter, request *http.Request) {
	callbackURL := request.URL.Query().Get("callbackURL")
	if err := auth.checkCallbackURL(callbackURL, ErrInvalidCallbackURL); err != nil {
		auth.writeError(writer, request, err)
		return
	}
	token := request.PathValue("token")
	valid, err := auth.resetTokenValid(request.Context(), token)
	if err != nil {
		auth.writeError(writer, request, err)
		return
	}
	if callbackURL == "" {
		auth.writeError(writer, request, ErrInvalidToken)
		return
	}
	if !valid {
		http.Redirect(writer, request, auth.withQuery(callbackURL, "error", ErrInvalidToken.Code), http.StatusFound) //nolint:gosec // callbackURL passed checkCallbackURL
		return
	}
	http.Redirect(writer, request, auth.withQuery(callbackURL, "token", token), http.StatusFound) //nolint:gosec // callbackURL passed checkCallbackURL
}

func (auth *Auth) handleResetPassword(writer http.ResponseWriter, request *http.Request) {
	var in struct {
		NewPassword string `json:"newPassword"`
		Token       string `json:"token"`
	}
	if err := auth.validateOrigin(request, false); err != nil {
		auth.writeError(writer, request, err)
		return
	}
	if err := decode(request, &in); err != nil {
		auth.writeError(writer, request, err)
		return
	}
	if in.Token == "" {
		in.Token = request.URL.Query().Get("token")
	}
	if err := auth.ResetPassword(request.Context(), in.Token, in.NewPassword); err != nil {
		auth.writeError(writer, request, err)
		return
	}
	writeJSON(writer, http.StatusOK, map[string]bool{"status": true})
}
