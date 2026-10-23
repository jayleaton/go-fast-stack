package auth

import (
	"errors"
	"net/http"
)

// writeChallenge answers a sign-in that needs a second factor the way Better
// Auth's twoFactor plugin does: 200 {"twoFactorRedirect": true, ...} and the
// two_factor cookie. It reports whether err was such a challenge.
func (auth *Auth) writeChallenge(writer http.ResponseWriter, err error) bool {
	challenge, ok := errors.AsType[*TwoFactorChallenge](err)
	if !ok {
		return false
	}
	auth.SetTwoFactorCookie(writer, challenge)
	writeJSON(writer, http.StatusOK, map[string]any{
		"twoFactorRedirect":      true,
		"twoFactorMethods":       challenge.Methods,
		"twoFactorSetupRequired": challenge.SetupRequired,
	})
	return true
}

// postBody passes the origin check and decodes the JSON body of a POST.
func (auth *Auth) postBody(writer http.ResponseWriter, request *http.Request, value any) bool {
	if err := auth.validateOrigin(request, false); err != nil {
		auth.writeError(writer, request, err)
		return false
	}
	if err := decode(request, value); err != nil {
		auth.writeError(writer, request, err)
		return false
	}
	return true
}

func (auth *Auth) handleSetPassword(writer http.ResponseWriter, request *http.Request) {
	var in struct {
		Token       string `json:"token"`
		NewPassword string `json:"newPassword"`
	}
	if !auth.postBody(writer, request, &in) {
		return
	}
	user, sess, err := auth.CompleteSetPassword(request.Context(), in.Token, in.NewPassword)
	if auth.writeChallenge(writer, err) {
		return
	}
	if err != nil {
		auth.writeError(writer, request, err)
		return
	}
	auth.signedIn(writer, sess)
	writeJSON(writer, http.StatusOK, map[string]any{"token": sess.Token, "user": user})
}

// handleTwoFactorEnable starts enrolment: signed in (password required), or
// for a SetupRequired sign-in challenge (the two_factor cookie).
func (auth *Auth) handleTwoFactorEnable(writer http.ResponseWriter, request *http.Request) {
	var in struct {
		Password string `json:"password"`
	}
	if !auth.postBody(writer, request, &in) {
		return
	}
	var setup TOTPSetup
	var err error
	if user, _, ok := auth.userFrom(request.Context()); ok {
		setup, err = auth.EnableTwoFactor(request.Context(), user.ID, in.Password)
	} else if auth.meta(request.Context()).twoFactorToken != "" {
		setup, err = auth.StartTwoFactorSetup(request.Context(), "")
	} else {
		err = ErrUnauthorized
	}
	if err != nil {
		auth.writeError(writer, request, err)
		return
	}
	writeJSON(writer, http.StatusOK, setup)
}

func (auth *Auth) handleTwoFactorURI(writer http.ResponseWriter, request *http.Request) {
	user, _, ok := auth.account(writer, request)
	if !ok {
		return
	}
	var in struct {
		Password string `json:"password"`
	}
	if err := decode(request, &in); err != nil {
		auth.writeError(writer, request, err)
		return
	}
	if err := auth.twoFactorOn(); err != nil {
		auth.writeError(writer, request, err)
		return
	}
	if err := auth.rateLimitUser(request.Context(), "two-factor", user.ID, RuleSignIn); err != nil {
		auth.writeError(writer, request, err)
		return
	}
	if err := auth.checkUserPassword(request.Context(), user.ID, in.Password); err != nil {
		auth.writeError(writer, request, err)
		return
	}
	uri, err := auth.totpURIFor(request.Context(), user)
	if err != nil {
		auth.writeError(writer, request, err)
		return
	}
	writeJSON(writer, http.StatusOK, map[string]string{"totpURI": uri})
}

// handleTwoFactorVerify completes a pending sign-in (two_factor cookie), or,
// for a signed-in user, confirms enrolment with a TOTP code.
func (auth *Auth) handleTwoFactorVerify(method string) http.HandlerFunc {
	return func(writer http.ResponseWriter, request *http.Request) {
		var in struct {
			Code        string `json:"code"`
			TrustDevice bool   `json:"trustDevice"`
		}
		if !auth.postBody(writer, request, &in) {
			return
		}
		ctx := request.Context()
		if user, sess, ok := auth.userFrom(ctx); ok && !auth.pendingFor(ctx, user.ID) {
			// Signed in: confirm enrolment. A stale or foreign two_factor
			// cookie must not get in the way.
			if method != TwoFactorMethodTOTP {
				auth.writeError(writer, request, ErrInvalidCode)
				return
			}
			if err := auth.ConfirmTwoFactor(ctx, user.ID, in.Code); err != nil {
				auth.writeError(writer, request, err)
				return
			}
			writeJSON(writer, http.StatusOK, map[string]any{"token": sess.Token, "user": user})
			return
		}
		res, err := auth.VerifyTwoFactor(ctx, VerifyTwoFactorInput{Code: in.Code, Method: method, TrustDevice: in.TrustDevice})
		if errors.Is(err, ErrInvalidTwoFactorCookie) {
			auth.clearTwoFactorCookie(writer)
		}
		if err != nil {
			auth.writeError(writer, request, err)
			return
		}
		auth.clearTwoFactorCookie(writer)
		auth.SetTrustDeviceCookie(writer, res.TrustToken)
		auth.signedIn(writer, res.Session)
		writeJSON(writer, http.StatusOK, map[string]any{"token": res.Session.Token, "user": res.User})
	}
}

func (auth *Auth) handleTwoFactorBackupCodes(writer http.ResponseWriter, request *http.Request) {
	user, _, ok := auth.account(writer, request)
	if !ok {
		return
	}
	var in struct {
		Password string `json:"password"`
	}
	if err := decode(request, &in); err != nil {
		auth.writeError(writer, request, err)
		return
	}
	codes, err := auth.GenerateBackupCodes(request.Context(), user.ID, in.Password)
	if err != nil {
		auth.writeError(writer, request, err)
		return
	}
	writeJSON(writer, http.StatusOK, map[string]any{"status": true, "backupCodes": codes})
}

func (auth *Auth) handleTwoFactorDisable(writer http.ResponseWriter, request *http.Request) {
	user, _, ok := auth.account(writer, request)
	if !ok {
		return
	}
	var in struct {
		Password string `json:"password"`
	}
	if err := decode(request, &in); err != nil {
		auth.writeError(writer, request, err)
		return
	}
	if err := auth.DisableTwoFactor(request.Context(), user.ID, in.Password); err != nil {
		auth.writeError(writer, request, err)
		return
	}
	auth.setSignedCookie(writer, auth.cookie(trustDeviceCookieName), "", 0)
	writeJSON(writer, http.StatusOK, map[string]bool{"status": true})
}
