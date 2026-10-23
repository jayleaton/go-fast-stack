package auth

import (
	"net/http"
)

// account reads the signed-in caller for the account endpoints, answering 401
// when there is none. State-changing requests also pass the origin check.
func (auth *Auth) account(writer http.ResponseWriter, request *http.Request) (User, Session, bool) {
	if request.Method != http.MethodGet {
		if err := auth.validateOrigin(request, false); err != nil {
			auth.writeError(writer, request, err)
			return User{}, Session{}, false
		}
	}
	user, sess, ok := auth.userFrom(request.Context())
	if !ok {
		auth.writeError(writer, request, ErrUnauthorized)
	}
	return user, sess, ok
}

func (auth *Auth) handleChangePassword(writer http.ResponseWriter, request *http.Request) {
	user, sess, ok := auth.account(writer, request)
	if !ok {
		return
	}
	var in struct {
		CurrentPassword     string `json:"currentPassword"`
		NewPassword         string `json:"newPassword"`
		RevokeOtherSessions bool   `json:"revokeOtherSessions"`
	}
	if err := decode(request, &in); err != nil {
		auth.writeError(writer, request, err)
		return
	}
	if err := auth.ChangePassword(request.Context(), user.ID, in.CurrentPassword, in.NewPassword, in.RevokeOtherSessions, sess.ID); err != nil {
		auth.writeError(writer, request, err)
		return
	}
	// Better Auth returns a new token when it rotates the session; this
	// package keeps the current session, so the token is null.
	writeJSON(writer, http.StatusOK, map[string]any{"token": nil, "user": user})
}

func (auth *Auth) handleChangeEmail(writer http.ResponseWriter, request *http.Request) {
	user, _, ok := auth.account(writer, request)
	if !ok {
		return
	}
	var in struct {
		NewEmail    string `json:"newEmail"`
		CallbackURL string `json:"callbackURL"`
	}
	if err := decode(request, &in); err != nil {
		auth.writeError(writer, request, err)
		return
	}
	if err := auth.ChangeEmail(request.Context(), user.ID, in.NewEmail, in.CallbackURL); err != nil {
		auth.writeError(writer, request, err)
		return
	}
	writeJSON(writer, http.StatusOK, map[string]bool{"status": true})
}

func (auth *Auth) handleListSessions(writer http.ResponseWriter, request *http.Request) {
	user, _, ok := auth.account(writer, request)
	if !ok {
		return
	}
	sessions, err := auth.ListSessions(request.Context(), user.ID)
	if err != nil {
		auth.writeError(writer, request, err)
		return
	}
	writer.Header().Set("Cache-Control", "no-store")
	writeJSON(writer, http.StatusOK, sessions)
}

// handleRevokeSession takes {"id"} (sessions from list-sessions carry no
// token) or, for Better Auth clients revoking their own token, {"token"}.
func (auth *Auth) handleRevokeSession(writer http.ResponseWriter, request *http.Request) {
	user, sess, ok := auth.account(writer, request)
	if !ok {
		return
	}
	var in struct {
		ID    string `json:"id"`
		Token string `json:"token"`
	}
	if err := decode(request, &in); err != nil {
		auth.writeError(writer, request, err)
		return
	}
	if err := auth.revokeSession(request.Context(), user.ID, in.ID, in.Token); err != nil {
		auth.writeError(writer, request, err)
		return
	}
	if (in.ID != "" && in.ID == sess.ID) || (in.Token != "" && in.Token == sess.Token) {
		auth.ClearSessionCookie(writer)
	}
	writeJSON(writer, http.StatusOK, map[string]bool{"status": true})
}

func (auth *Auth) handleRevokeOtherSessions(writer http.ResponseWriter, request *http.Request) {
	user, sess, ok := auth.account(writer, request)
	if !ok {
		return
	}
	if err := auth.RevokeOtherSessions(request.Context(), user.ID, sess.ID); err != nil {
		auth.writeError(writer, request, err)
		return
	}
	writeJSON(writer, http.StatusOK, map[string]bool{"status": true})
}
