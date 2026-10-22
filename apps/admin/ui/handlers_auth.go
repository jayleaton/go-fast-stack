package ui

import (
	"errors"
	"net/http"
	"strings"

	"github.com/jayleaton/ai-patterns-go/apps/admin/internal/config"
	"github.com/jayleaton/ai-patterns-go/apps/admin/ui/i18n"
	"github.com/jayleaton/ai-patterns-go/packages/uikit"
)

func (u *UI) home(w http.ResponseWriter, r *http.Request) {
	if _, _, ok := u.userFrom(r.Context()); ok {
		http.Redirect(w, r, config.AppRoutes.Users, http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, config.AppRoutes.Login, http.StatusSeeOther)
}

func (u *UI) loginPage(w http.ResponseWriter, r *http.Request) {
	if _, _, ok := u.userFrom(r.Context()); ok {
		http.Redirect(w, r, config.AppRoutes.Users, http.StatusSeeOther)
		return
	}
	u.render(w, r, http.StatusOK, LoginPage(AuthForm{}))
}

func (u *UI) login(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	email := strings.TrimSpace(r.PostFormValue("email"))
	s, err := u.auth.SignIn(ctx, email, r.PostFormValue("password"))
	if err != nil {
		if u.challenge(w, r, err) {
			return
		}
		msg := u.errorMessage(ctx, err)
		u.patch(w, r, http.StatusUnprocessableEntity, LoginPage(AuthForm{Email: email, Error: msg}), uikit.Alert(formErrorID, uikit.Error, msg))
		return
	}
	u.auth.SetSessionCookie(w, s)
	u.redirect(w, r, config.AppRoutes.Users)
}

// challenge handles a *TwoFactorChallenge returned as the error of a
// sign-in: it stores the challenge and sends the browser to the second step
// (or to TOTP enrolment), and reports whether err was one.
func (u *UI) challenge(w http.ResponseWriter, r *http.Request, err error) bool {
	var c *TwoFactorChallenge
	if !errors.As(err, &c) {
		return false
	}
	u.auth.SetTwoFactorCookie(w, c)
	if c.SetupRequired {
		u.redirect(w, r, config.AppRoutes.TwoFactorSetup)
	} else {
		u.redirect(w, r, config.AppRoutes.TwoFactor)
	}
	return true
}

func (u *UI) setPasswordPage(w http.ResponseWriter, r *http.Request) {
	u.tokenPage(w)
	u.render(w, r, http.StatusOK, SetPasswordPage(r.URL.Query().Get("token"), AuthForm{}))
}

// setPassword finishes an invite: the admin picks a password and is signed
// in, or continues into 2FA.
func (u *UI) setPassword(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	token := r.PostFormValue("token")
	password := r.PostFormValue("password")
	fail := func(msg string) {
		u.patch(w, r, http.StatusUnprocessableEntity, SetPasswordPage(token, AuthForm{Error: msg}), uikit.Alert(formErrorID, uikit.Error, msg))
	}
	if token == "" {
		fail(i18n.T(ctx, "pages.setPassword.invalidLink"))
		return
	}
	if password != r.PostFormValue("confirmPassword") {
		fail(i18n.T(ctx, "pages.setPassword.passwordMismatch"))
		return
	}
	s, err := u.auth.CompleteSetPassword(ctx, token, password)
	if err != nil {
		if u.challenge(w, r, err) {
			return
		}
		fail(u.errorMessage(ctx, err))
		return
	}
	u.auth.SetSessionCookie(w, s)
	u.redirect(w, r, config.AppRoutes.Users)
}

func (u *UI) twoFactorPage(w http.ResponseWriter, r *http.Request) {
	u.render(w, r, http.StatusOK, TwoFactorPage(TwoFactorForm{}))
}

// twoFactor is the second sign-in step, with a TOTP or a backup code.
func (u *UI) twoFactor(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	in := VerifyTwoFactorInput{
		Code:        strings.TrimSpace(r.PostFormValue("code")),
		Method:      MethodTOTP,
		TrustDevice: r.PostFormValue("trustDevice") != "",
	}
	if r.PostFormValue("method") == MethodBackupCode {
		in.Method = MethodBackupCode
	}
	s, trust, err := u.auth.VerifyTwoFactor(ctx, in)
	if err != nil {
		f := TwoFactorForm{Backup: in.Method == MethodBackupCode, Trust: in.TrustDevice, Error: u.errorMessage(ctx, err), Expired: errors.Is(err, ErrChallengeExpired)}
		u.patch(w, r, http.StatusUnprocessableEntity, TwoFactorPage(f), TwoFactorError(f))
		return
	}
	u.signIn(w, r, s, trust)
}

// signIn issues the session (and the trusted-device cookie) and goes to the
// dashboard.
func (u *UI) signIn(w http.ResponseWriter, r *http.Request, s Session, trustToken string) {
	u.auth.SetSessionCookie(w, s)
	if trustToken != "" {
		u.auth.SetTrustDeviceCookie(w, trustToken)
	}
	u.redirect(w, r, config.AppRoutes.Users)
}

// twoFactorSetupPage has three states: signed out with a SetupRequired
// challenge (enrol now, then sign in), signed in without 2FA (password
// first), signed in with 2FA (nothing to do).
func (u *UI) twoFactorSetupPage(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if a, _, ok := u.userFrom(ctx); ok {
		w.Header().Set("Cache-Control", "no-store")
		u.render(w, r, http.StatusOK, TwoFactorSetupPage(SetupState{SignedIn: true, Enabled: a.TwoFactorEnabled, Required: u.requireTwoFactor}))
		return
	}
	w.Header().Set("Cache-Control", "no-store") // the page shows the secret
	setup, err := u.auth.StartTwoFactorSetup(ctx)
	switch {
	case errors.Is(err, ErrChallengeExpired):
		u.render(w, r, http.StatusOK, TwoFactorSetupPage(SetupState{Expired: true}))
	case err != nil:
		u.render(w, r, http.StatusInternalServerError, TwoFactorSetupPage(SetupState{Expired: true, Error: u.errorMessage(ctx, err)}))
	default:
		u.render(w, r, http.StatusOK, TwoFactorSetupPage(SetupState{Required: true, Setup: &setup}))
	}
}

// enableTwoFactor is optional enrolment for a signed-in admin: check the
// password, then show the secret and the code form.
func (u *UI) enableTwoFactor(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	a, _, ok := u.userFrom(ctx)
	if !ok {
		u.redirect(w, r, config.AppRoutes.Login)
		return
	}
	st := SetupState{SignedIn: true, Required: u.requireTwoFactor}
	setup, err := u.auth.EnableTwoFactor(ctx, a.ID, r.PostFormValue("password"))
	if err != nil {
		st.Error = u.errorMessage(ctx, err)
		u.patch(w, r, http.StatusUnprocessableEntity, TwoFactorSetupPage(st), uikit.Alert(formErrorID, uikit.Error, st.Error))
		return
	}
	st.Setup = &setup
	u.patch(w, r, http.StatusOK, TwoFactorSetupPage(st), SetupBody(st))
}

// twoFactorSetup checks the first code from the authenticator app. Signed
// in, it confirms optional enrolment; signed out, it finishes the
// SetupRequired challenge and signs in.
func (u *UI) twoFactorSetup(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	code := strings.TrimSpace(r.PostFormValue("code"))
	// A plain post re-renders the page, so the form carries the secret back
	// (the backup codes were shown once and are not).
	shown := &TOTPSetup{Secret: r.PostFormValue("secret"), URI: r.PostFormValue("uri")}
	a, _, signedIn := u.userFrom(ctx)
	st := SetupState{SignedIn: signedIn, Required: u.requireTwoFactor || !signedIn, Setup: shown}

	var err error
	if signedIn {
		err = u.auth.ConfirmTwoFactor(ctx, a.ID, code)
	} else {
		var s Session
		var trust string
		s, trust, err = u.auth.VerifyTwoFactor(ctx, VerifyTwoFactorInput{Code: code, Method: MethodTOTP})
		if err == nil {
			u.signIn(w, r, s, trust)
			return
		}
	}
	if err != nil {
		if !signedIn && errors.Is(err, ErrChallengeExpired) {
			st = SetupState{Expired: true}
			u.patch(w, r, http.StatusUnprocessableEntity, TwoFactorSetupPage(st), SetupBody(st))
			return
		}
		st.Error = u.errorMessage(ctx, err)
		u.patch(w, r, http.StatusUnprocessableEntity, TwoFactorSetupPage(st), uikit.Alert(formErrorID, uikit.Error, st.Error))
		return
	}
	st = SetupState{SignedIn: true, Done: true}
	u.patch(w, r, http.StatusOK, TwoFactorSetupPage(st), SetupBody(st))
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

// tokenPage marks a page that carries an emailed token (the invite link):
// never sent on as a Referer behind https. Opening it spends nothing; the
// POST does. (Over plain http the default strict-origin stays: a no-referrer
// page posts with Origin: null, and without Fetch Metadata the cross-origin
// protection could not tell it from a forged post.)
func (u *UI) tokenPage(w http.ResponseWriter) {
	if u.secure {
		w.Header().Set("Referrer-Policy", "no-referrer")
	}
}
