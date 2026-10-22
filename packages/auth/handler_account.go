package auth

import (
	"net/http"
)

// account reads the signed-in caller for the account endpoints, answering 401
// when there is none. State-changing requests also pass the origin check.
func (a *Auth) account(w http.ResponseWriter, r *http.Request) (User, Session, bool) {
	if r.Method != http.MethodGet {
		if err := a.validateOrigin(r, false); err != nil {
			a.writeError(w, r, err)
			return User{}, Session{}, false
		}
	}
	user, sess, ok := a.userFrom(r.Context())
	if !ok {
		a.writeError(w, r, ErrUnauthorized)
	}
	return user, sess, ok
}

func (a *Auth) handleChangePassword(w http.ResponseWriter, r *http.Request) {
	user, sess, ok := a.account(w, r)
	if !ok {
		return
	}
	var in struct {
		CurrentPassword     string `json:"currentPassword"`
		NewPassword         string `json:"newPassword"`
		RevokeOtherSessions bool   `json:"revokeOtherSessions"`
	}
	if err := decode(r, &in); err != nil {
		a.writeError(w, r, err)
		return
	}
	if err := a.ChangePassword(r.Context(), user.ID, in.CurrentPassword, in.NewPassword, in.RevokeOtherSessions, sess.ID); err != nil {
		a.writeError(w, r, err)
		return
	}
	// Better Auth returns a new token when it rotates the session; this
	// package keeps the current session, so the token is null.
	writeJSON(w, http.StatusOK, map[string]any{"token": nil, "user": user})
}

func (a *Auth) handleChangeEmail(w http.ResponseWriter, r *http.Request) {
	user, _, ok := a.account(w, r)
	if !ok {
		return
	}
	var in struct {
		NewEmail    string `json:"newEmail"`
		CallbackURL string `json:"callbackURL"`
	}
	if err := decode(r, &in); err != nil {
		a.writeError(w, r, err)
		return
	}
	if err := a.ChangeEmail(r.Context(), user.ID, in.NewEmail, in.CallbackURL); err != nil {
		a.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"status": true})
}

func (a *Auth) handleListSessions(w http.ResponseWriter, r *http.Request) {
	user, _, ok := a.account(w, r)
	if !ok {
		return
	}
	sessions, err := a.ListSessions(r.Context(), user.ID)
	if err != nil {
		a.writeError(w, r, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, sessions)
}

// handleRevokeSession takes {"id"} (sessions from list-sessions carry no
// token) or, for Better Auth clients revoking their own token, {"token"}.
func (a *Auth) handleRevokeSession(w http.ResponseWriter, r *http.Request) {
	user, sess, ok := a.account(w, r)
	if !ok {
		return
	}
	var in struct {
		ID    string `json:"id"`
		Token string `json:"token"`
	}
	if err := decode(r, &in); err != nil {
		a.writeError(w, r, err)
		return
	}
	if err := a.revokeSession(r.Context(), user.ID, in.ID, in.Token); err != nil {
		a.writeError(w, r, err)
		return
	}
	if (in.ID != "" && in.ID == sess.ID) || (in.Token != "" && in.Token == sess.Token) {
		a.ClearSessionCookie(w)
	}
	writeJSON(w, http.StatusOK, map[string]bool{"status": true})
}

func (a *Auth) handleRevokeOtherSessions(w http.ResponseWriter, r *http.Request) {
	user, sess, ok := a.account(w, r)
	if !ok {
		return
	}
	if err := a.RevokeOtherSessions(r.Context(), user.ID, sess.ID); err != nil {
		a.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"status": true})
}
