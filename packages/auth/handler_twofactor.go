package auth

import (
	"errors"
	"net/http"
)

// writeChallenge answers a sign-in that needs a second factor the way Better
// Auth's twoFactor plugin does: 200 {"twoFactorRedirect": true, ...} and the
// two_factor cookie. It reports whether err was such a challenge.
func (a *Auth) writeChallenge(w http.ResponseWriter, err error) bool {
	c, ok := errors.AsType[*TwoFactorChallenge](err)
	if !ok {
		return false
	}
	a.SetTwoFactorCookie(w, c)
	writeJSON(w, http.StatusOK, map[string]any{
		"twoFactorRedirect":      true,
		"twoFactorMethods":       c.Methods,
		"twoFactorSetupRequired": c.SetupRequired,
	})
	return true
}

// postBody passes the origin check and decodes the JSON body of a POST.
func (a *Auth) postBody(w http.ResponseWriter, r *http.Request, v any) bool {
	if err := a.validateOrigin(r, false); err != nil {
		a.writeError(w, r, err)
		return false
	}
	if err := decode(r, v); err != nil {
		a.writeError(w, r, err)
		return false
	}
	return true
}

func (a *Auth) handleSetPassword(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Token       string `json:"token"`
		NewPassword string `json:"newPassword"`
	}
	if !a.postBody(w, r, &in) {
		return
	}
	user, sess, err := a.CompleteSetPassword(r.Context(), in.Token, in.NewPassword)
	if a.writeChallenge(w, err) {
		return
	}
	if err != nil {
		a.writeError(w, r, err)
		return
	}
	a.signedIn(w, sess)
	writeJSON(w, http.StatusOK, map[string]any{"token": sess.Token, "user": user})
}

// handleTwoFactorEnable starts enrolment: signed in (password required), or
// for a SetupRequired sign-in challenge (the two_factor cookie).
func (a *Auth) handleTwoFactorEnable(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Password string `json:"password"`
	}
	if !a.postBody(w, r, &in) {
		return
	}
	var setup TOTPSetup
	var err error
	if user, _, ok := a.userFrom(r.Context()); ok {
		setup, err = a.EnableTwoFactor(r.Context(), user.ID, in.Password)
	} else if a.meta(r.Context()).twoFactorToken != "" {
		setup, err = a.StartTwoFactorSetup(r.Context(), "")
	} else {
		err = ErrUnauthorized
	}
	if err != nil {
		a.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, setup)
}

func (a *Auth) handleTwoFactorURI(w http.ResponseWriter, r *http.Request) {
	user, _, ok := a.account(w, r)
	if !ok {
		return
	}
	var in struct {
		Password string `json:"password"`
	}
	if err := decode(r, &in); err != nil {
		a.writeError(w, r, err)
		return
	}
	if err := a.twoFactorOn(); err != nil {
		a.writeError(w, r, err)
		return
	}
	if err := a.rateLimitUser(r.Context(), "two-factor", user.ID, RuleSignIn); err != nil {
		a.writeError(w, r, err)
		return
	}
	if err := a.checkUserPassword(r.Context(), user.ID, in.Password); err != nil {
		a.writeError(w, r, err)
		return
	}
	uri, err := a.totpURIFor(r.Context(), user)
	if err != nil {
		a.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"totpURI": uri})
}

// handleTwoFactorVerify completes a pending sign-in (two_factor cookie), or,
// for a signed-in user, confirms enrolment with a TOTP code.
func (a *Auth) handleTwoFactorVerify(method string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Code        string `json:"code"`
			TrustDevice bool   `json:"trustDevice"`
		}
		if !a.postBody(w, r, &in) {
			return
		}
		ctx := r.Context()
		if user, sess, ok := a.userFrom(ctx); ok && !a.pendingFor(ctx, user.ID) {
			// Signed in: confirm enrolment. A stale or foreign two_factor
			// cookie must not get in the way.
			if method != TwoFactorMethodTOTP {
				a.writeError(w, r, ErrInvalidCode)
				return
			}
			if err := a.ConfirmTwoFactor(ctx, user.ID, in.Code); err != nil {
				a.writeError(w, r, err)
				return
			}
			writeJSON(w, http.StatusOK, map[string]any{"token": sess.Token, "user": user})
			return
		}
		res, err := a.VerifyTwoFactor(ctx, VerifyTwoFactorInput{Code: in.Code, Method: method, TrustDevice: in.TrustDevice})
		if errors.Is(err, ErrInvalidTwoFactorCookie) {
			a.clearTwoFactorCookie(w)
		}
		if err != nil {
			a.writeError(w, r, err)
			return
		}
		a.clearTwoFactorCookie(w)
		a.SetTrustDeviceCookie(w, res.TrustToken)
		a.signedIn(w, res.Session)
		writeJSON(w, http.StatusOK, map[string]any{"token": res.Session.Token, "user": res.User})
	}
}

func (a *Auth) handleTwoFactorBackupCodes(w http.ResponseWriter, r *http.Request) {
	user, _, ok := a.account(w, r)
	if !ok {
		return
	}
	var in struct {
		Password string `json:"password"`
	}
	if err := decode(r, &in); err != nil {
		a.writeError(w, r, err)
		return
	}
	codes, err := a.GenerateBackupCodes(r.Context(), user.ID, in.Password)
	if err != nil {
		a.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": true, "backupCodes": codes})
}

func (a *Auth) handleTwoFactorDisable(w http.ResponseWriter, r *http.Request) {
	user, _, ok := a.account(w, r)
	if !ok {
		return
	}
	var in struct {
		Password string `json:"password"`
	}
	if err := decode(r, &in); err != nil {
		a.writeError(w, r, err)
		return
	}
	if err := a.DisableTwoFactor(r.Context(), user.ID, in.Password); err != nil {
		a.writeError(w, r, err)
		return
	}
	a.setSignedCookie(w, a.cookie(trustDeviceCookieName), "", 0)
	writeJSON(w, http.StatusOK, map[string]bool{"status": true})
}
