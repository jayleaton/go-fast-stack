package ui

import (
	"errors"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/starfederation/datastar-go/datastar"

	"github.com/jayleaton/go-fast-stack/apps/web/internal/apiclient"
	"github.com/jayleaton/go-fast-stack/apps/web/internal/config"
	"github.com/jayleaton/go-fast-stack/apps/web/ui/i18n"
	"github.com/jayleaton/go-fast-stack/apps/web/ui/welcome"
	"github.com/jayleaton/go-fast-stack/packages/auth"
)

// resendCooldown matches the original's five minutes between resends.
const resendCooldown = 5 * time.Minute

func (ui *UI) home(writer http.ResponseWriter, request *http.Request, caller apiclient.Caller) {
	if caller.SignedIn() {
		http.Redirect(writer, request, config.AppRoutes.Dashboard, http.StatusSeeOther)
		return
	}
	if ui.features.TemplateWelcome {
		ui.render(writer, request, http.StatusOK, welcome.LandingPage())
		return
	}
	ui.render(writer, request, http.StatusOK, HomePage())
}

func (ui *UI) why(writer http.ResponseWriter, request *http.Request, _ apiclient.Caller) {
	if !ui.features.TemplateWelcome {
		http.NotFound(writer, request)
		return
	}
	ui.render(writer, request, http.StatusOK, welcome.WhyPage())
}

func (ui *UI) loginPage(writer http.ResponseWriter, request *http.Request, _ apiclient.Caller) {
	ui.render(writer, request, http.StatusOK, LoginPage(AuthForm{}))
}

func (ui *UI) login(writer http.ResponseWriter, request *http.Request, _ apiclient.Caller) {
	ctx := request.Context()
	in := SignInInput{
		Email:       strings.TrimSpace(request.PostFormValue("email")),
		Password:    request.PostFormValue("password"),
		CallbackURL: config.AppRoutes.VerifyEmailSuccess,
	}
	_, session, err := ui.auth.SignInEmail(ctx, in)
	if err != nil {
		msg := ui.errorMessage(ctx, err)
		if errors.Is(err, auth.ErrEmailNotVerified) {
			// Like the original: send a fresh link and say so.
			if err := ui.auth.SendVerificationEmail(ctx, in.Email, config.AppRoutes.VerifyEmailSuccess); err != nil {
				ui.log.WarnContext(ctx, "ui: resend verification on login", "err", err)
				msg = i18n.T(ctx, "pages.verifyEmail.checkEmail")
			}
		}
		ui.patch(writer, request, http.StatusUnprocessableEntity, LoginPage(AuthForm{Email: in.Email, Error: msg}), FormError(msg))
		return
	}
	ui.auth.SetSessionCookie(writer, session)
	ui.redirect(writer, request, config.AppRoutes.Dashboard)
}

func (ui *UI) signupPage(writer http.ResponseWriter, request *http.Request, _ apiclient.Caller) {
	ui.render(writer, request, http.StatusOK, SignupPage(AuthForm{}))
}

func (ui *UI) signup(writer http.ResponseWriter, request *http.Request, _ apiclient.Caller) {
	ctx := request.Context()
	in := SignUpInput{
		Name:        strings.TrimSpace(request.PostFormValue("name")),
		Email:       strings.TrimSpace(request.PostFormValue("email")),
		Password:    request.PostFormValue("password"),
		CallbackURL: config.AppRoutes.VerifyEmailSuccess,
	}
	_, session, err := ui.auth.SignUpEmail(ctx, in)
	if err != nil {
		msg := ui.errorMessage(ctx, err)
		ui.patch(writer, request, http.StatusUnprocessableEntity, SignupPage(AuthForm{Name: in.Name, Email: in.Email, Error: msg}), FormError(msg))
		return
	}
	if ui.features.EmailVerification || session.Token == "" {
		ui.redirect(writer, request, config.AppRoutes.VerifyEmail+"?"+url.Values{"email": {in.Email}}.Encode())
		return
	}
	ui.auth.SetSessionCookie(writer, session)
	ui.redirect(writer, request, config.AppRoutes.Dashboard)
}

func (ui *UI) forgotPasswordPage(writer http.ResponseWriter, request *http.Request, _ apiclient.Caller) {
	ui.render(writer, request, http.StatusOK, ForgotPasswordPage(AuthForm{}, false))
}

func (ui *UI) forgotPassword(writer http.ResponseWriter, request *http.Request, _ apiclient.Caller) {
	ctx := request.Context()
	email := strings.TrimSpace(request.PostFormValue("email"))
	if err := ui.auth.RequestPasswordReset(ctx, email, config.AppRoutes.ResetPassword); err != nil {
		ui.log.ErrorContext(ctx, "ui: request password reset", "err", err)
		msg := i18n.T(ctx, "common.error")
		ui.patch(writer, request, http.StatusUnprocessableEntity, ForgotPasswordPage(AuthForm{Email: email, Error: msg}, false), FormError(msg))
		return
	}
	ui.patch(writer, request, http.StatusOK, ForgotPasswordPage(AuthForm{}, true), ForgotPasswordSent())
}

func (ui *UI) resetPasswordPage(writer http.ResponseWriter, request *http.Request, _ apiclient.Caller) {
	ui.tokenPage(writer)
	ui.render(writer, request, http.StatusOK, ResetPasswordPage(request.URL.Query().Get("token"), AuthForm{}, false))
}

func (ui *UI) resetPassword(writer http.ResponseWriter, request *http.Request, _ apiclient.Caller) {
	ctx := request.Context()
	token := request.PostFormValue("token")
	password := request.PostFormValue("password")
	fail := func(key string) {
		msg := i18n.T(ctx, key)
		ui.patch(writer, request, http.StatusUnprocessableEntity, ResetPasswordPage(token, AuthForm{Error: msg}, false), FormError(msg))
	}
	if token == "" {
		fail("pages.resetPassword.invalidResetLink")
		return
	}
	if password != request.PostFormValue("confirmPassword") {
		fail("pages.resetPassword.passwordMismatch")
		return
	}
	if err := ui.auth.ResetPassword(ctx, token, password); err != nil {
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
			ui.log.ErrorContext(ctx, "ui: reset password", "err", err)
			fail("common.error")
		}
		return
	}
	ui.patch(writer, request, http.StatusOK, ResetPasswordPage(token, AuthForm{}, true), ResetPasswordDone())
}

func (ui *UI) verifyEmailPage(writer http.ResponseWriter, request *http.Request, _ apiclient.Caller) {
	ui.render(writer, request, http.StatusOK, VerifyEmailPage(request.URL.Query().Get("email"), ResendState{}))
}

func (ui *UI) resendVerification(writer http.ResponseWriter, request *http.Request, _ apiclient.Caller) {
	ctx := request.Context()
	email := strings.TrimSpace(request.PostFormValue("email"))
	state := ResendState{
		Message:         i18n.T(ctx, "pages.verifyEmail.verificationEmailSent"),
		CooldownSeconds: int(resendCooldown.Seconds()),
	}
	status := http.StatusOK
	if err := ui.auth.SendVerificationEmail(ctx, email, config.AppRoutes.VerifyEmailSuccess); err != nil {
		state = ResendState{Error: ui.errorMessage(ctx, err)}
		status = http.StatusUnprocessableEntity
	}
	ui.patch(writer, request, status, VerifyEmailPage(email, state), ResendBlock(email, state))
}

func (ui *UI) verifyEmailSuccessPage(writer http.ResponseWriter, request *http.Request, _ apiclient.Caller) {
	ui.tokenPage(writer)
	query := request.URL.Query()
	// auth's own link handler lands here with ?error=<code> when it failed.
	if code := query.Get("error"); code != "" {
		ui.render(writer, request, http.StatusOK, VerifyEmailSuccessPage("", "", VerifyEmailFailed(authCodeMessage(request.Context(), code))))
		return
	}
	ui.render(writer, request, http.StatusOK, VerifyEmailSuccessPage(query.Get("token"), query.Get("callbackURL"), nil))
}

// verifyEmail spends the token of a verification or change-email link (auth
// emails them to this page; the person confirmed with this POST) and signs
// the opener in, then continues to the link's callbackURL when it is a path
// on this site or a callback auth trusts, else the dashboard.
func (ui *UI) verifyEmail(writer http.ResponseWriter, request *http.Request, _ apiclient.Caller) {
	ctx := request.Context()
	token, callback := request.PostFormValue("token"), request.PostFormValue("callbackURL")
	if token == "" {
		result := VerifyEmailFailed(i18n.T(ctx, "pages.verifyEmail.invalidToken"))
		ui.patch(writer, request, http.StatusUnprocessableEntity, VerifyEmailSuccessPage(token, callback, result), result)
		return
	}
	result, status := VerifyEmailSucceeded(ui.verifyNext(callback)), http.StatusOK
	_, session, err := ui.auth.VerifyEmail(ctx, token)
	switch {
	case errors.Is(err, auth.ErrInvalidToken), errors.Is(err, auth.ErrTokenExpired):
		result, status = VerifyEmailFailed(i18n.T(ctx, "pages.verifyEmail.invalidToken")), http.StatusUnprocessableEntity
	case err != nil:
		result, status = VerifyEmailFailed(ui.errorMessage(ctx, err)), http.StatusUnprocessableEntity
	case session.Token != "":
		ui.auth.SetSessionCookie(writer, session)
	}
	ui.patch(writer, request, status, VerifyEmailSuccessPage(token, callback, result), result)
}

// verifyNext is where a verified link continues: its callbackURL when that
// is another page on this site or a callback auth trusts (the mobile app's
// deep link), else the dashboard.
func (ui *UI) verifyNext(callback string) string {
	if callback != "" && !strings.HasPrefix(callback, "/") && ui.auth.CheckCallbackURL(callback) == nil {
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
func (ui *UI) tokenPage(writer http.ResponseWriter) {
	if ui.secure {
		writer.Header().Set("Referrer-Policy", "no-referrer")
	}
}

func (ui *UI) dashboard(writer http.ResponseWriter, request *http.Request, _ apiclient.Caller) {
	writer.Header().Set("Cache-Control", "no-store")
	current, err := ui.api.Me(request.Context())
	if err != nil {
		ui.apiFailed(writer, request, err)
		return
	}
	ui.render(writer, request, http.StatusOK, DashboardPage(DashboardData{User: User(current.User), Tab: newTabID()}))
}

// dashboardLive is the server-push example: one SSE stream per open
// dashboard that patches the clock every tick and the account card whenever
// the user changes (from any tab or device served by this process), and
// sends the tab to the login page when its session is revoked elsewhere.
func (ui *UI) dashboardLive(writer http.ResponseWriter, request *http.Request, caller apiclient.Caller) {
	tab := request.URL.Query().Get("tab")
	events, cancel := ui.hub.subscribe(caller.User.ID)
	defer cancel()

	sse := datastar.NewSSE(writer, request)
	tick := time.NewTicker(ui.interval)
	defer tick.Stop()
	for {
		if err := sse.PatchElementTempl(LiveBody(ui.now(), caller.Session)); err != nil {
			return
		}
		select {
		case <-request.Context().Done():
			return
		case <-tick.C:
		case ev := <-events:
			if ev.Deleted {
				_ = sse.Redirect(config.AppRoutes.Home)
				return
			}
			if ev.revokes(caller.Session.ID) {
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

func (ui *UI) logout(writer http.ResponseWriter, request *http.Request, caller apiclient.Caller) {
	ctx := request.Context()
	if caller.SignedIn() {
		if err := ui.auth.SignOut(ctx, caller.Session.Token); err != nil {
			ui.log.WarnContext(ctx, "ui: sign out", "err", err)
		}
	}
	ui.auth.ClearSessionCookie(writer)
	ui.redirect(writer, request, config.AppRoutes.Login)
}

// setLocale is the language switcher: store the choice, go back to the page.
func (ui *UI) setLocale(writer http.ResponseWriter, request *http.Request, _ apiclient.Caller) {
	locale, ok := i18n.Supported(request.PostFormValue("locale"))
	if !ok {
		locale = i18n.DefaultLocale
	}
	i18n.SetCookie(writer, locale, ui.secure)
	http.Redirect(writer, request, safeNext(request.PostFormValue("next")), http.StatusSeeOther)
}
