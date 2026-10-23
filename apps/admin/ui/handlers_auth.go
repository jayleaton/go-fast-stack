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

func (ui *UI) home(w http.ResponseWriter, r *http.Request, caller apiclient.Caller) {
	if caller.SignedIn() {
		http.Redirect(w, r, config.AppRoutes.Users, http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, config.AppRoutes.Login, http.StatusSeeOther)
}

func (ui *UI) loginPage(w http.ResponseWriter, r *http.Request, caller apiclient.Caller) {
	if caller.SignedIn() {
		http.Redirect(w, r, config.AppRoutes.Users, http.StatusSeeOther)
		return
	}
	ui.render(w, r, http.StatusOK, LoginPage(AuthForm{}))
}

func (ui *UI) login(w http.ResponseWriter, r *http.Request, _ apiclient.Caller) {
	ctx := r.Context()
	email := strings.TrimSpace(r.PostFormValue("email"))
	s, err := ui.auth.SignIn(ctx, email, r.PostFormValue("password"))
	if err != nil {
		if ui.challenge(w, r, err) {
			return
		}
		msg := ui.errorMessage(ctx, err)
		ui.patch(w, r, http.StatusUnprocessableEntity, LoginPage(AuthForm{Email: email, Error: msg}), uikit.Alert(formErrorID, uikit.Error, msg))
		return
	}
	ui.auth.SetSessionCookie(w, s)
	ui.redirect(w, r, config.AppRoutes.Users)
}

// challenge handles a *TwoFactorChallenge returned as the error of a
// sign-in: it stores the challenge and sends the browser to the second step
// (or to TOTP enrolment), and reports whether err was one.
func (ui *UI) challenge(w http.ResponseWriter, r *http.Request, err error) bool {
	var c *TwoFactorChallenge
	if !errors.As(err, &c) {
		return false
	}
	ui.auth.SetTwoFactorCookie(w, c)
	if c.SetupRequired {
		ui.redirect(w, r, config.AppRoutes.TwoFactorSetup)
	} else {
		ui.redirect(w, r, config.AppRoutes.TwoFactor)
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
		fail(i18n.T(ctx, mode.key("invalidLink")))
		return
	}
	if password != request.PostFormValue("confirmPassword") {
		fail(i18n.T(ctx, "pages.setPassword.passwordMismatch"))
		return
	}
	session, err := ui.auth.CompleteSetPassword(ctx, token, password)
	if err != nil {
		if ui.challenge(writer, request, err) {
			return
		}
		if errors.Is(err, ErrInvalidToken) {
			fail(i18n.T(ctx, mode.key("invalidLink"))) // the mode says where a new link comes from
			return
		}
		fail(ui.errorMessage(ctx, err))
		return
	}
	ui.auth.SetSessionCookie(writer, session)
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
	if err := ui.auth.RequestPasswordReset(ctx, email); err != nil {
		msg := ui.errorMessage(ctx, err)
		ui.patch(writer, request, http.StatusUnprocessableEntity, ForgotPasswordPage(AuthForm{Email: email, Error: msg}, ""), uikit.Alert(formErrorID, uikit.Error, msg))
		return
	}
	sent := i18n.T(ctx, "pages.forgotPassword.sent")
	ui.patch(writer, request, http.StatusOK, ForgotPasswordPage(AuthForm{}, sent), ForgotPasswordBody(AuthForm{}, sent))
}

func (ui *UI) twoFactorPage(w http.ResponseWriter, r *http.Request, _ apiclient.Caller) {
	ui.render(w, r, http.StatusOK, TwoFactorPage(TwoFactorForm{}))
}

// twoFactor is the second sign-in step, with a TOTP or a backup code.
func (ui *UI) twoFactor(w http.ResponseWriter, r *http.Request, _ apiclient.Caller) {
	ctx := r.Context()
	in := VerifyTwoFactorInput{
		Code:        strings.TrimSpace(r.PostFormValue("code")),
		Method:      MethodTOTP,
		TrustDevice: r.PostFormValue("trustDevice") != "",
	}
	if r.PostFormValue("method") == MethodBackupCode {
		in.Method = MethodBackupCode
	}
	s, trust, err := ui.auth.VerifyTwoFactor(ctx, in)
	if err != nil {
		f := TwoFactorForm{Backup: in.Method == MethodBackupCode, Trust: in.TrustDevice, Error: ui.errorMessage(ctx, err), Expired: errors.Is(err, ErrChallengeExpired)}
		ui.patch(w, r, http.StatusUnprocessableEntity, TwoFactorPage(f), TwoFactorError(f))
		return
	}
	ui.signIn(w, r, s, trust)
}

// signIn issues the session (and the trusted-device cookie) and goes to the
// dashboard.
func (ui *UI) signIn(w http.ResponseWriter, r *http.Request, s Session, trustToken string) {
	ui.auth.SetSessionCookie(w, s)
	if trustToken != "" {
		ui.auth.SetTrustDeviceCookie(w, trustToken)
	}
	ui.redirect(w, r, config.AppRoutes.Users)
}

// twoFactorSetupPage has three states: signed out with a SetupRequired
// challenge (enrol now, then sign in), signed in without 2FA (password
// first), signed in with 2FA (nothing to do).
func (ui *UI) twoFactorSetupPage(w http.ResponseWriter, r *http.Request, caller apiclient.Caller) {
	ctx := r.Context()
	if caller.SignedIn() {
		w.Header().Set("Cache-Control", "no-store")
		ui.render(w, r, http.StatusOK, TwoFactorSetupPage(SetupState{SignedIn: true, Enabled: caller.Admin.TwoFactorEnabled, Required: ui.requireTwoFactor}))
		return
	}
	w.Header().Set("Cache-Control", "no-store") // the page shows the secret
	setup, err := ui.auth.StartTwoFactorSetup(ctx)
	switch {
	case errors.Is(err, ErrChallengeExpired):
		ui.render(w, r, http.StatusOK, TwoFactorSetupPage(SetupState{Expired: true}))
	case err != nil:
		ui.render(w, r, http.StatusInternalServerError, TwoFactorSetupPage(SetupState{Expired: true, Error: ui.errorMessage(ctx, err)}))
	default:
		ui.render(w, r, http.StatusOK, TwoFactorSetupPage(SetupState{Required: true, Setup: &setup}))
	}
}

// enableTwoFactor is optional enrolment for a signed-in admin: check the
// password, then show the secret and the code form.
func (ui *UI) enableTwoFactor(w http.ResponseWriter, r *http.Request, caller apiclient.Caller) {
	ctx := r.Context()
	st := SetupState{SignedIn: true, Required: ui.requireTwoFactor}
	setup, err := ui.auth.EnableTwoFactor(ctx, caller.Admin.ID, r.PostFormValue("password"))
	if err != nil {
		st.Error = ui.errorMessage(ctx, err)
		ui.patch(w, r, http.StatusUnprocessableEntity, TwoFactorSetupPage(st), uikit.Alert(formErrorID, uikit.Error, st.Error))
		return
	}
	st.Setup = &setup
	ui.patch(w, r, http.StatusOK, TwoFactorSetupPage(st), SetupBody(st))
}

// twoFactorSetup checks the first code from the authenticator app. Signed
// in, it confirms optional enrolment; signed out, it finishes the
// SetupRequired challenge and signs in.
func (ui *UI) twoFactorSetup(w http.ResponseWriter, r *http.Request, caller apiclient.Caller) {
	ctx := r.Context()
	code := strings.TrimSpace(r.PostFormValue("code"))
	// A plain post re-renders the page, so the form carries the secret back
	// (the backup codes were shown once and are not).
	shown := &TOTPSetup{Secret: r.PostFormValue("secret"), URI: r.PostFormValue("uri")}
	signedIn := caller.SignedIn()
	st := SetupState{SignedIn: signedIn, Required: ui.requireTwoFactor || !signedIn, Setup: shown}

	var err error
	if signedIn {
		err = ui.auth.ConfirmTwoFactor(ctx, caller.Admin.ID, code)
	} else {
		var s Session
		var trust string
		s, trust, err = ui.auth.VerifyTwoFactor(ctx, VerifyTwoFactorInput{Code: code, Method: MethodTOTP})
		if err == nil {
			ui.signIn(w, r, s, trust)
			return
		}
	}
	if err != nil {
		if !signedIn && errors.Is(err, ErrChallengeExpired) {
			st = SetupState{Expired: true}
			ui.patch(w, r, http.StatusUnprocessableEntity, TwoFactorSetupPage(st), SetupBody(st))
			return
		}
		st.Error = ui.errorMessage(ctx, err)
		ui.patch(w, r, http.StatusUnprocessableEntity, TwoFactorSetupPage(st), uikit.Alert(formErrorID, uikit.Error, st.Error))
		return
	}
	st = SetupState{SignedIn: true, Done: true}
	ui.patch(w, r, http.StatusOK, TwoFactorSetupPage(st), SetupBody(st))
}

func (ui *UI) logout(w http.ResponseWriter, r *http.Request, caller apiclient.Caller) {
	ctx := r.Context()
	if caller.SignedIn() {
		if err := ui.auth.SignOut(ctx, caller.Session.Token); err != nil {
			ui.log.WarnContext(ctx, "ui: sign out", "err", err)
		}
	}
	ui.auth.ClearSessionCookie(w)
	ui.redirect(w, r, config.AppRoutes.Login)
}

// setLocale is the language switcher: store the choice, go back to the page.
func (ui *UI) setLocale(w http.ResponseWriter, r *http.Request, _ apiclient.Caller) {
	l, ok := i18n.Supported(r.PostFormValue("locale"))
	if !ok {
		l = i18n.DefaultLocale
	}
	i18n.SetCookie(w, l, ui.secure)
	http.Redirect(w, r, safeNext(r.PostFormValue("next")), http.StatusSeeOther)
}

// tokenPage marks a page that carries an emailed token (the invite link):
// never sent on as a Referer behind https. Opening it spends nothing; the
// POST does. (Over plain http the default strict-origin stays: a no-referrer
// page posts with Origin: null, and without Fetch Metadata the cross-origin
// protection could not tell it from a forged post.)
func (ui *UI) tokenPage(w http.ResponseWriter) {
	if ui.secure {
		w.Header().Set("Referrer-Policy", "no-referrer")
	}
}
