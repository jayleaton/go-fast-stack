package ui

import (
	"errors"
	"net/http"
	"strings"

	"github.com/jayleaton/go-fast-stack/apps/admin/internal/apiclient"
	"github.com/jayleaton/go-fast-stack/apps/admin/internal/config"
	"github.com/jayleaton/go-fast-stack/apps/admin/ui/i18n"
	"github.com/jayleaton/go-fast-stack/packages/uikit"
)

func (ui *UI) home(writer http.ResponseWriter, request *http.Request, caller apiclient.Caller) {
	if caller.SignedIn() {
		http.Redirect(writer, request, config.AppRoutes.Users, http.StatusSeeOther)
		return
	}
	http.Redirect(writer, request, config.AppRoutes.Login, http.StatusSeeOther)
}

func (ui *UI) loginPage(writer http.ResponseWriter, request *http.Request, caller apiclient.Caller) {
	if caller.SignedIn() {
		http.Redirect(writer, request, config.AppRoutes.Users, http.StatusSeeOther)
		return
	}
	ui.render(writer, request, http.StatusOK, LoginPage(AuthForm{}))
}

func (ui *UI) login(writer http.ResponseWriter, request *http.Request, _ apiclient.Caller) {
	ctx := request.Context()
	email := strings.TrimSpace(request.PostFormValue("email"))
	result, err := ui.api.SignIn(ctx, apiclient.SignIn{Email: email, Password: request.PostFormValue("password")})
	if err != nil {
		if ui.challenge(writer, request, err) {
			return
		}
		msg := ui.errorMessage(ctx, err)
		ui.patch(writer, request, http.StatusUnprocessableEntity, LoginPage(AuthForm{Email: email, Error: msg}), uikit.Alert(formErrorID, uikit.Error, msg))
		return
	}
	setCookies(writer, result.Cookies)
	ui.redirect(writer, request, config.AppRoutes.Users)
}

// challenge handles a *TwoFactorChallenge returned as the error of a
// sign-in: it stores the challenge and sends the browser to the second step
// (or to TOTP enrolment), and reports whether err was one.
func (ui *UI) challenge(writer http.ResponseWriter, request *http.Request, err error) bool {
	var challenge *TwoFactorChallenge
	if !errors.As(err, &challenge) {
		return false
	}
	setCookies(writer, challenge.Cookies)
	if challenge.SetupRequired {
		ui.redirect(writer, request, config.AppRoutes.TwoFactorSetup)
	} else {
		ui.redirect(writer, request, config.AppRoutes.TwoFactor)
	}
	return true
}

func (ui *UI) setPasswordPage(writer http.ResponseWriter, request *http.Request, _ apiclient.Caller) {
	ui.tokenPage(writer)
	ui.render(writer, request, http.StatusOK, SetPasswordPage(inviteMode, request.URL.Query().Get("token"), AuthForm{}))
}

func (ui *UI) resetPasswordPage(writer http.ResponseWriter, request *http.Request, _ apiclient.Caller) {
	ui.tokenPage(writer)
	ui.render(writer, request, http.StatusOK, SetPasswordPage(resetMode, request.URL.Query().Get("token"), AuthForm{}))
}

// setPassword finishes an invite: the admin picks a password and is signed
// in, or continues into 2FA.
func (ui *UI) setPassword(writer http.ResponseWriter, request *http.Request, _ apiclient.Caller) {
	ui.choosePassword(writer, request, inviteMode)
}

// resetPassword spends a reset link the same way (invite and reset links are
// the same kind of token): every other session ends, and sign-in still goes
// through 2FA.
func (ui *UI) resetPassword(writer http.ResponseWriter, request *http.Request, _ apiclient.Caller) {
	ui.choosePassword(writer, request, resetMode)
}

func (ui *UI) choosePassword(writer http.ResponseWriter, request *http.Request, mode passwordMode) {
	ctx := request.Context()
	token := request.PostFormValue("token")
	password := request.PostFormValue("password")
	fail := func(msg string) {
		ui.patch(writer, request, http.StatusUnprocessableEntity, SetPasswordPage(mode, token, AuthForm{Error: msg}), uikit.Alert(formErrorID, uikit.Error, msg))
	}
	if token == "" {
		fail(i18n.Translate(ctx, mode.key("invalidLink")))
		return
	}
	if password != request.PostFormValue("confirmPassword") {
		fail(i18n.Translate(ctx, "pages.setPassword.passwordMismatch"))
		return
	}
	result, err := ui.api.SetPassword(ctx, apiclient.SetPassword{Token: token, Password: password})
	if err != nil {
		if ui.challenge(writer, request, err) {
			return
		}
		if errors.Is(err, ErrInvalidToken) {
			fail(i18n.Translate(ctx, mode.key("invalidLink"))) // the mode says where a new link comes from
			return
		}
		fail(ui.errorMessage(ctx, err))
		return
	}
	setCookies(writer, result.Cookies)
	ui.redirect(writer, request, config.AppRoutes.Users)
}

func (ui *UI) forgotPasswordPage(writer http.ResponseWriter, request *http.Request, caller apiclient.Caller) {
	if caller.SignedIn() {
		http.Redirect(writer, request, config.AppRoutes.Users, http.StatusSeeOther)
		return
	}
	ui.render(writer, request, http.StatusOK, ForgotPasswordPage(AuthForm{}, ""))
}

// forgotPassword asks for a reset link. The answer is the same whether or
// not the email belongs to an admin; only rate limiting is reported.
func (ui *UI) forgotPassword(writer http.ResponseWriter, request *http.Request, _ apiclient.Caller) {
	ctx := request.Context()
	email := strings.TrimSpace(request.PostFormValue("email"))
	if err := ui.api.RequestPasswordReset(ctx, apiclient.CreatePasswordReset{Email: email}); err != nil {
		msg := ui.errorMessage(ctx, err)
		ui.patch(writer, request, http.StatusUnprocessableEntity, ForgotPasswordPage(AuthForm{Email: email, Error: msg}, ""), uikit.Alert(formErrorID, uikit.Error, msg))
		return
	}
	sent := i18n.Translate(ctx, "pages.forgotPassword.sent")
	ui.patch(writer, request, http.StatusOK, ForgotPasswordPage(AuthForm{}, sent), ForgotPasswordBody(AuthForm{}, sent))
}

func (ui *UI) twoFactorPage(writer http.ResponseWriter, request *http.Request, _ apiclient.Caller) {
	ui.render(writer, request, http.StatusOK, TwoFactorPage(TwoFactorForm{}))
}

// twoFactor is the second sign-in step, with a TOTP or a backup code.
func (ui *UI) twoFactor(writer http.ResponseWriter, request *http.Request, _ apiclient.Caller) {
	ctx := request.Context()
	in := apiclient.VerifyTwoFactor{
		Code:        strings.TrimSpace(request.PostFormValue("code")),
		Method:      MethodTOTP,
		TrustDevice: request.PostFormValue("trustDevice") != "",
	}
	if request.PostFormValue("method") == MethodBackupCode {
		in.Method = MethodBackupCode
	}
	result, err := ui.api.VerifyTwoFactor(ctx, in)
	if err != nil {
		form := TwoFactorForm{Backup: in.Method == MethodBackupCode, Trust: in.TrustDevice, Error: ui.errorMessage(ctx, err), Expired: errors.Is(err, ErrChallengeExpired)}
		ui.patch(writer, request, http.StatusUnprocessableEntity, TwoFactorPage(form), TwoFactorError(form))
		return
	}
	ui.signIn(writer, request, result)
}

// signIn sets the cookies a finished sign-in returned (the session, and the
// trusted device when asked for) and goes to the dashboard.
func (ui *UI) signIn(writer http.ResponseWriter, request *http.Request, result apiclient.AuthResult) {
	setCookies(writer, result.Cookies)
	ui.redirect(writer, request, config.AppRoutes.Users)
}

// twoFactorSetupPage has three states: signed out with a SetupRequired
// challenge (enrol now, then sign in), signed in without 2FA (password
// first), signed in with 2FA (nothing to do).
func (ui *UI) twoFactorSetupPage(writer http.ResponseWriter, request *http.Request, caller apiclient.Caller) {
	ctx := request.Context()
	if caller.SignedIn() {
		writer.Header().Set("Cache-Control", "no-store")
		ui.render(writer, request, http.StatusOK, TwoFactorSetupPage(SetupState{SignedIn: true, Enabled: caller.Admin.TwoFactorEnabled, Required: ui.requireTwoFactor}))
		return
	}
	writer.Header().Set("Cache-Control", "no-store") // the page shows the secret
	setup, err := ui.api.StartTwoFactorSetup(ctx)
	switch {
	case errors.Is(err, ErrChallengeExpired):
		ui.render(writer, request, http.StatusOK, TwoFactorSetupPage(SetupState{Expired: true}))
	case err != nil:
		ui.render(writer, request, http.StatusInternalServerError, TwoFactorSetupPage(SetupState{Expired: true, Error: ui.errorMessage(ctx, err)}))
	default:
		ui.render(writer, request, http.StatusOK, TwoFactorSetupPage(SetupState{Required: true, Setup: &setup}))
	}
}

// enableTwoFactor is optional enrolment for a signed-in admin: check the
// password, then show the secret and the code form.
func (ui *UI) enableTwoFactor(writer http.ResponseWriter, request *http.Request, caller apiclient.Caller) {
	ctx := request.Context()
	st := SetupState{SignedIn: true, Required: ui.requireTwoFactor}
	setup, err := ui.api.EnableTwoFactor(ctx, apiclient.EnableTwoFactor{Password: request.PostFormValue("password")})
	if err != nil {
		st.Error = ui.errorMessage(ctx, err)
		ui.patch(writer, request, http.StatusUnprocessableEntity, TwoFactorSetupPage(st), uikit.Alert(formErrorID, uikit.Error, st.Error))
		return
	}
	st.Setup = &setup
	ui.patch(writer, request, http.StatusOK, TwoFactorSetupPage(st), SetupBody(st))
}

// twoFactorSetup checks the first code from the authenticator app. Signed
// in, it confirms optional enrolment; signed out, it finishes the
// SetupRequired challenge and signs in.
func (ui *UI) twoFactorSetup(writer http.ResponseWriter, request *http.Request, caller apiclient.Caller) {
	ctx := request.Context()
	code := strings.TrimSpace(request.PostFormValue("code"))
	// A plain post re-renders the page, so the form carries the secret back
	// (the backup codes were shown once and are not).
	shown := &TOTPSetup{Secret: request.PostFormValue("secret"), URI: request.PostFormValue("uri")}
	signedIn := caller.SignedIn()
	st := SetupState{SignedIn: signedIn, Required: ui.requireTwoFactor || !signedIn, Setup: shown}

	var err error
	if signedIn {
		err = ui.api.ConfirmTwoFactor(ctx, apiclient.ConfirmTwoFactor{Code: code})
	} else {
		var result apiclient.AuthResult
		result, err = ui.api.VerifyTwoFactor(ctx, apiclient.VerifyTwoFactor{Code: code, Method: MethodTOTP})
		if err == nil {
			ui.signIn(writer, request, result)
			return
		}
	}
	if err != nil {
		if !signedIn && errors.Is(err, ErrChallengeExpired) {
			st = SetupState{Expired: true}
			ui.patch(writer, request, http.StatusUnprocessableEntity, TwoFactorSetupPage(st), SetupBody(st))
			return
		}
		st.Error = ui.errorMessage(ctx, err)
		ui.patch(writer, request, http.StatusUnprocessableEntity, TwoFactorSetupPage(st), uikit.Alert(formErrorID, uikit.Error, st.Error))
		return
	}
	st = SetupState{SignedIn: true, Done: true}
	ui.patch(writer, request, http.StatusOK, TwoFactorSetupPage(st), SetupBody(st))
}

func (ui *UI) logout(writer http.ResponseWriter, request *http.Request, _ apiclient.Caller) {
	ctx := request.Context()
	// Signed out or not: SignOut ends the session if there is one and
	// always returns the cookie that clears it.
	signedOut, err := ui.api.SignOut(ctx)
	if err != nil {
		ui.log.WarnContext(ctx, "ui: sign out", "err", err)
	}
	setCookies(writer, signedOut.Cookies)
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

// tokenPage marks a page that carries an emailed token (the invite link):
// never sent on as a Referer behind https. Opening it spends nothing; the
// POST does. (Over plain http the default strict-origin stays: a no-referrer
// page posts with Origin: null, and without Fetch Metadata the cross-origin
// protection could not tell it from a forged post.)
func (ui *UI) tokenPage(writer http.ResponseWriter) {
	if ui.secure {
		writer.Header().Set("Referrer-Policy", "no-referrer")
	}
}
