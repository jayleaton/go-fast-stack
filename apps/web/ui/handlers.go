package ui

import (
	"errors"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/starfederation/datastar-go/datastar"

	"github.com/jayleaton/go-fast-stack/apps/web/internal/config"
	"github.com/jayleaton/go-fast-stack/apps/web/ui/i18n"
	"github.com/jayleaton/go-fast-stack/apps/web/ui/welcome"
	"github.com/jayleaton/go-fast-stack/packages/auth"
)

// resendCooldown matches the original's five minutes between resends.
const resendCooldown = 5 * time.Minute

func (u *UI) home(w http.ResponseWriter, r *http.Request) {
	if _, _, ok := u.userFrom(r.Context()); ok {
		http.Redirect(w, r, config.AppRoutes.Dashboard, http.StatusSeeOther)
		return
	}
	if u.features.TemplateWelcome {
		u.render(w, r, http.StatusOK, welcome.LandingPage())
		return
	}
	u.render(w, r, http.StatusOK, HomePage())
}

func (u *UI) why(w http.ResponseWriter, r *http.Request) {
	if !u.features.TemplateWelcome {
		http.NotFound(w, r)
		return
	}
	u.render(w, r, http.StatusOK, welcome.WhyPage())
}

func (u *UI) loginPage(w http.ResponseWriter, r *http.Request) {
	u.render(w, r, http.StatusOK, LoginPage(AuthForm{}))
}

func (u *UI) login(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	in := SignInInput{
		Email:       strings.TrimSpace(r.PostFormValue("email")),
		Password:    r.PostFormValue("password"),
		CallbackURL: config.AppRoutes.VerifyEmailSuccess,
	}
	_, s, err := u.auth.SignInEmail(ctx, in)
	if err != nil {
		msg := u.errorMessage(ctx, err)
		if errors.Is(err, auth.ErrEmailNotVerified) {
			// Like the original: send a fresh link and say so.
			if err := u.auth.SendVerificationEmail(ctx, in.Email, config.AppRoutes.VerifyEmailSuccess); err != nil {
				u.log.WarnContext(ctx, "ui: resend verification on login", "err", err)
				msg = i18n.T(ctx, "pages.verifyEmail.checkEmail")
			}
		}
		u.patch(w, r, http.StatusUnprocessableEntity, LoginPage(AuthForm{Email: in.Email, Error: msg}), FormError(msg))
		return
	}
	u.auth.SetSessionCookie(w, s)
	u.redirect(w, r, config.AppRoutes.Dashboard)
}

func (u *UI) signupPage(w http.ResponseWriter, r *http.Request) {
	u.render(w, r, http.StatusOK, SignupPage(AuthForm{}))
}

func (u *UI) signup(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	in := SignUpInput{
		Name:        strings.TrimSpace(r.PostFormValue("name")),
		Email:       strings.TrimSpace(r.PostFormValue("email")),
		Password:    r.PostFormValue("password"),
		CallbackURL: config.AppRoutes.VerifyEmailSuccess,
	}
	_, s, err := u.auth.SignUpEmail(ctx, in)
	if err != nil {
		msg := u.errorMessage(ctx, err)
		u.patch(w, r, http.StatusUnprocessableEntity, SignupPage(AuthForm{Name: in.Name, Email: in.Email, Error: msg}), FormError(msg))
		return
	}
	if u.features.EmailVerification || s.Token == "" {
		u.redirect(w, r, config.AppRoutes.VerifyEmail+"?"+url.Values{"email": {in.Email}}.Encode())
		return
	}
	u.auth.SetSessionCookie(w, s)
	u.redirect(w, r, config.AppRoutes.Dashboard)
}

func (u *UI) forgotPasswordPage(w http.ResponseWriter, r *http.Request) {
	u.render(w, r, http.StatusOK, ForgotPasswordPage(AuthForm{}, false))
}

func (u *UI) forgotPassword(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	email := strings.TrimSpace(r.PostFormValue("email"))
	if err := u.auth.RequestPasswordReset(ctx, email, config.AppRoutes.ResetPassword); err != nil {
		u.log.ErrorContext(ctx, "ui: request password reset", "err", err)
		msg := i18n.T(ctx, "common.error")
		u.patch(w, r, http.StatusUnprocessableEntity, ForgotPasswordPage(AuthForm{Email: email, Error: msg}, false), FormError(msg))
		return
	}
	u.patch(w, r, http.StatusOK, ForgotPasswordPage(AuthForm{}, true), ForgotPasswordSent())
}

func (u *UI) resetPasswordPage(w http.ResponseWriter, r *http.Request) {
	u.tokenPage(w)
	u.render(w, r, http.StatusOK, ResetPasswordPage(r.URL.Query().Get("token"), AuthForm{}, false))
}

func (u *UI) resetPassword(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	token := r.PostFormValue("token")
	password := r.PostFormValue("password")
	fail := func(key string) {
		msg := i18n.T(ctx, key)
		u.patch(w, r, http.StatusUnprocessableEntity, ResetPasswordPage(token, AuthForm{Error: msg}, false), FormError(msg))
	}
	if token == "" {
		fail("pages.resetPassword.invalidResetLink")
		return
	}
	if password != r.PostFormValue("confirmPassword") {
		fail("pages.resetPassword.passwordMismatch")
		return
	}
	if err := u.auth.ResetPassword(ctx, token, password); err != nil {
		switch {
		case errors.Is(err, auth.ErrInvalidToken), errors.Is(err, auth.ErrTokenExpired):
			fail("pages.resetPassword.invalidResetLink")
		case errors.Is(err, auth.ErrPasswordTooShort):
			fail("pages.resetPassword.passwordTooShort")
		case errors.Is(err, auth.ErrPasswordTooLong):
			fail("errors.passwordTooLong")
		case errors.Is(err, auth.ErrTooManyRequests):
			fail("errors.tooManyRequests")
		default:
			u.log.ErrorContext(ctx, "ui: reset password", "err", err)
			fail("common.error")
		}
		return
	}
	u.patch(w, r, http.StatusOK, ResetPasswordPage(token, AuthForm{}, true), ResetPasswordDone())
}

func (u *UI) verifyEmailPage(w http.ResponseWriter, r *http.Request) {
	u.render(w, r, http.StatusOK, VerifyEmailPage(r.URL.Query().Get("email"), ResendState{}))
}

func (u *UI) resendVerification(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	email := strings.TrimSpace(r.PostFormValue("email"))
	state := ResendState{
		Message:         i18n.T(ctx, "pages.verifyEmail.verificationEmailSent"),
		CooldownSeconds: int(resendCooldown.Seconds()),
	}
	status := http.StatusOK
	if err := u.auth.SendVerificationEmail(ctx, email, config.AppRoutes.VerifyEmailSuccess); err != nil {
		state = ResendState{Error: u.errorMessage(ctx, err)}
		status = http.StatusUnprocessableEntity
	}
	u.patch(w, r, status, VerifyEmailPage(email, state), ResendBlock(email, state))
}

func (u *UI) verifyEmailSuccessPage(w http.ResponseWriter, r *http.Request) {
	u.tokenPage(w)
	q := r.URL.Query()
	// auth's own link handler lands here with ?error=<code> when it failed.
	if code := q.Get("error"); code != "" {
		u.render(w, r, http.StatusOK, VerifyEmailSuccessPage("", "", VerifyEmailFailed(authCodeMessage(r.Context(), code))))
		return
	}
	u.render(w, r, http.StatusOK, VerifyEmailSuccessPage(q.Get("token"), q.Get("callbackURL"), nil))
}

// verifyEmail spends the token of a verification or change-email link (auth
// emails them to this page; the person confirmed with this POST) and signs
// the opener in, then continues to the link's callbackURL when it is a path
// on this site or a callback auth trusts, else the dashboard.
func (u *UI) verifyEmail(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	token, callback := r.PostFormValue("token"), r.PostFormValue("callbackURL")
	if token == "" {
		result := VerifyEmailFailed(i18n.T(ctx, "pages.verifyEmail.invalidToken"))
		u.patch(w, r, http.StatusUnprocessableEntity, VerifyEmailSuccessPage(token, callback, result), result)
		return
	}
	result, status := VerifyEmailSucceeded(u.verifyNext(callback)), http.StatusOK
	_, s, err := u.auth.VerifyEmail(ctx, token)
	switch {
	case errors.Is(err, auth.ErrInvalidToken), errors.Is(err, auth.ErrTokenExpired):
		result, status = VerifyEmailFailed(i18n.T(ctx, "pages.verifyEmail.invalidToken")), http.StatusUnprocessableEntity
	case err != nil:
		result, status = VerifyEmailFailed(u.errorMessage(ctx, err)), http.StatusUnprocessableEntity
	case s.Token != "":
		u.auth.SetSessionCookie(w, s)
	}
	u.patch(w, r, status, VerifyEmailSuccessPage(token, callback, result), result)
}

// verifyNext is where a verified link continues: its callbackURL when that
// is another page on this site or a callback auth trusts (the mobile app's
// deep link), else the dashboard.
func (u *UI) verifyNext(callback string) string {
	if callback != "" && !strings.HasPrefix(callback, "/") && u.auth.CheckCallbackURL(callback) == nil {
		return callback
	}
	next := safeNext(callback)
	if next == config.AppRoutes.Home || strings.HasPrefix(next, config.AppRoutes.VerifyEmailSuccess) {
		return config.AppRoutes.Dashboard
	}
	return next
}

// tokenPage marks a page that carries an emailed token: never sent on as a
// Referer behind https. (Over plain http the default strict-origin stays: a
// no-referrer page posts with Origin: null, and without Fetch Metadata the
// cross-origin protection could not tell it from a forged post.)
func (u *UI) tokenPage(w http.ResponseWriter) {
	if u.secure {
		w.Header().Set("Referrer-Policy", "no-referrer")
	}
}

func (u *UI) dashboard(w http.ResponseWriter, r *http.Request) {
	user, _, ok := u.userFrom(r.Context())
	if !ok {
		http.Redirect(w, r, config.AppRoutes.Login, http.StatusSeeOther)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	u.render(w, r, http.StatusOK, DashboardPage(DashboardData{User: user, Tab: newTabID()}))
}

// dashboardLive is the server-push example: one SSE stream per open
// dashboard that patches the clock every tick and the account card whenever
// the user changes (from any tab or device served by this process), and
// sends the tab to the login page when its session is revoked elsewhere.
func (u *UI) dashboardLive(w http.ResponseWriter, r *http.Request) {
	user, session, ok := u.userFrom(r.Context())
	if !ok {
		u.redirect(w, r, config.AppRoutes.Login)
		return
	}
	tab := r.URL.Query().Get("tab")
	events, cancel := u.hub.subscribe(user.ID)
	defer cancel()

	sse := datastar.NewSSE(w, r)
	tick := time.NewTicker(u.interval)
	defer tick.Stop()
	for {
		if err := sse.PatchElementTempl(LiveBody(u.now(), session)); err != nil {
			return
		}
		select {
		case <-r.Context().Done():
			return
		case <-tick.C:
		case ev := <-events:
			if ev.Deleted {
				_ = sse.Redirect(config.AppRoutes.Home)
				return
			}
			if ev.revokes(session.ID) {
				_ = sse.Redirect(config.AppRoutes.Login)
				return
			}
			if ev.User.ID == "" {
				continue
			}
			if err := sse.PatchElementTempl(AccountCard(ev.User)); err != nil {
				return
			}
			if ev.Tab != tab {
				if err := sse.PatchElementTempl(LiveNotice()); err != nil {
					return
				}
			}
		}
	}
}

func (u *UI) logout(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if _, s, ok := u.userFrom(ctx); ok {
		if err := u.auth.SignOut(ctx, s.Token); err != nil {
			u.log.WarnContext(ctx, "ui: sign out", "err", err)
		}
	}
	u.auth.ClearSessionCookie(w)
	u.redirect(w, r, config.AppRoutes.Login)
}

// setLocale is the language switcher: store the choice, go back to the page.
func (u *UI) setLocale(w http.ResponseWriter, r *http.Request) {
	l, ok := i18n.Supported(r.PostFormValue("locale"))
	if !ok {
		l = i18n.DefaultLocale
	}
	i18n.SetCookie(w, l, u.secure)
	http.Redirect(w, r, safeNext(r.PostFormValue("next")), http.StatusSeeOther)
}
