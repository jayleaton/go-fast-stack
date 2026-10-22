package ui

import (
	"context"
	"net/http"
	"strings"

	"github.com/a-h/templ"

	"github.com/jayleaton/ai-patterns-go/apps/web/internal/config"
	"github.com/jayleaton/ai-patterns-go/apps/web/internal/services"
	"github.com/jayleaton/ai-patterns-go/apps/web/internal/validate"
	"github.com/jayleaton/ai-patterns-go/apps/web/ui/i18n"
	"github.com/jayleaton/ai-patterns-go/apps/web/ui/view"
)

// signedIn returns the caller, or sends the browser to the login page.
func (u *UI) signedIn(w http.ResponseWriter, r *http.Request) (User, Session, bool) {
	user, session, ok := u.userFrom(r.Context())
	if !ok {
		u.redirect(w, r, config.AppRoutes.Login)
	}
	return user, session, ok
}

// accountData loads the Account page; load failures show as a banner.
func (u *UI) accountData(ctx context.Context, user User, session Session) AccountData {
	d := AccountData{User: user}
	a := actor(user, session)
	var err error
	if d.Sessions, err = u.users.ListSessions(ctx, a, user.ID); err != nil {
		d.LoadError = u.serviceMessage(ctx, err)
	}
	if d.Summary, err = u.users.DataSummary(ctx, a, user.ID); err != nil {
		d.LoadError = u.serviceMessage(ctx, err)
	}
	return d
}

func (u *UI) accountPage(w http.ResponseWriter, r *http.Request) {
	user, session, ok := u.signedIn(w, r)
	if !ok {
		return
	}
	d := u.accountData(r.Context(), user, session)
	d.EmailChanged = r.URL.Query().Get("email-changed") == "1"
	w.Header().Set("Cache-Control", "no-store")
	u.render(w, r, http.StatusOK, AccountPage(d))
}

// accountResult answers an account form: Datastar gets the fragments, a
// plain post goes back to the page (on error with the message as a banner).
func (u *UI) accountResult(w http.ResponseWriter, r *http.Request, user User, session Session, errMsg string, fragments ...templ.Component) {
	if isDatastar(r) {
		u.patch(w, r, http.StatusOK, nil, fragments...)
		return
	}
	if errMsg == "" {
		http.Redirect(w, r, config.AppRoutes.Account, http.StatusSeeOther)
		return
	}
	d := u.accountData(r.Context(), user, session)
	d.LoadError = errMsg
	u.render(w, r, http.StatusUnprocessableEntity, AccountPage(d))
}

func (u *UI) updateProfile(w http.ResponseWriter, r *http.Request) {
	user, session, ok := u.signedIn(w, r)
	if !ok {
		return
	}
	ctx := r.Context()
	name := r.PostFormValue("name")
	in := validate.UpdateUserSettings{Name: &name, Image: validate.Nullable[string]{Set: true}}
	if img := strings.TrimSpace(r.PostFormValue("image")); img != "" {
		in.Image.Value = &img
	}
	err := in.Validate()
	var saved services.User
	if err == nil {
		saved, err = u.users.UpdateProfile(ctx, actor(user, session), user.ID, in)
	}
	if err != nil {
		msg := u.serviceMessage(ctx, err)
		u.accountResult(w, r, user, session, msg, StatusAlert(profileStatusID, Status{Error: msg}))
		return
	}
	updated := userFromService(saved)
	u.hub.publish(user.ID, userEvent{User: updated})
	u.accountResult(w, r, user, session, "", ProfileCard(updated, Status{Message: i18n.T(ctx, "pages.account.profile.saved")}))
}

func (u *UI) requestEmailChange(w http.ResponseWriter, r *http.Request) {
	user, session, ok := u.signedIn(w, r)
	if !ok {
		return
	}
	ctx := r.Context()
	in := validate.UpdateUserEmail{NewEmail: r.PostFormValue("newEmail"), CallbackURL: config.AppRoutes.EmailChanged}
	err := in.Validate()
	if err == nil {
		err = u.users.RequestEmailChange(ctx, actor(user, session), user.ID, in)
	}
	if err != nil {
		msg := u.serviceMessage(ctx, err)
		u.accountResult(w, r, user, session, msg, StatusAlert(emailStatusID, Status{Error: msg}))
		return
	}
	u.accountResult(w, r, user, session, "", EmailCard(user.Email, in.NewEmail, Status{}))
}

func (u *UI) changePassword(w http.ResponseWriter, r *http.Request) {
	user, session, ok := u.signedIn(w, r)
	if !ok {
		return
	}
	ctx := r.Context()
	in := validate.UpdateUserPassword{
		CurrentPassword:     r.PostFormValue("currentPassword"),
		NewPassword:         r.PostFormValue("newPassword"),
		RevokeOtherSessions: r.PostFormValue("revokeOtherSessions") == "on",
	}
	fail := func(msg string) {
		u.accountResult(w, r, user, session, msg, StatusAlert(passwordStatusID, Status{Error: msg}))
	}
	if in.NewPassword != r.PostFormValue("confirmPassword") {
		fail(i18n.T(ctx, "pages.account.password.mismatch"))
		return
	}
	err := in.Validate()
	if err == nil {
		err = u.users.ChangePassword(ctx, actor(user, session), user.ID, in)
	}
	if err != nil {
		fail(u.serviceMessage(ctx, err))
		return
	}
	done := "pages.account.password.changed"
	if in.RevokeOtherSessions {
		done = "pages.account.password.changedRevoked"
		u.hub.publish(user.ID, userEvent{KeepSession: session.ID})
	}
	u.accountResult(w, r, user, session, "",
		PasswordCard(user.Email, Status{Message: i18n.T(ctx, done)}),
		u.sessionsCard(ctx, user, session, Status{}))
}

// sessionsCard re-lists the sessions with s as the outcome line.
func (u *UI) sessionsCard(ctx context.Context, user User, session Session, s Status) templ.Component {
	sessions, err := u.users.ListSessions(ctx, actor(user, session), user.ID)
	if err != nil && s.Error == "" {
		s = Status{Error: u.serviceMessage(ctx, err)}
	}
	return SessionsCard(sessions, s)
}

func (u *UI) revokeSession(w http.ResponseWriter, r *http.Request) {
	user, session, ok := u.signedIn(w, r)
	if !ok {
		return
	}
	ctx := r.Context()
	id := r.PostFormValue("sessionId")
	if err := u.users.RevokeSession(ctx, actor(user, session), user.ID, id); err != nil {
		msg := u.serviceMessage(ctx, err)
		u.accountResult(w, r, user, session, msg, u.sessionsCard(ctx, user, session, Status{Error: msg}))
		return
	}
	u.hub.publish(user.ID, userEvent{RevokedSession: id})
	u.accountResult(w, r, user, session, "", u.sessionsCard(ctx, user, session, Status{Message: i18n.T(ctx, "pages.account.sessions.revoked")}))
}

func (u *UI) revokeOtherSessions(w http.ResponseWriter, r *http.Request) {
	user, session, ok := u.signedIn(w, r)
	if !ok {
		return
	}
	ctx := r.Context()
	if err := u.users.RevokeOtherSessions(ctx, actor(user, session), user.ID); err != nil {
		msg := u.serviceMessage(ctx, err)
		u.accountResult(w, r, user, session, msg, u.sessionsCard(ctx, user, session, Status{Error: msg}))
		return
	}
	u.hub.publish(user.ID, userEvent{KeepSession: session.ID})
	u.accountResult(w, r, user, session, "", u.sessionsCard(ctx, user, session, Status{Message: i18n.T(ctx, "pages.account.sessions.othersRevoked")}))
}

func (u *UI) deleteAccount(w http.ResponseWriter, r *http.Request) {
	user, session, ok := u.signedIn(w, r)
	if !ok {
		return
	}
	ctx := r.Context()
	if err := u.users.DeleteAccount(ctx, actor(user, session), user.ID); err != nil {
		msg := u.serviceMessage(ctx, err)
		u.accountResult(w, r, user, session, msg, view.ErrorAlert(deleteErrorID, msg))
		return
	}
	// Sessions go with the user row; sign out anyway in case they do not cascade.
	if err := u.auth.SignOut(ctx, session.Token); err != nil {
		u.log.WarnContext(ctx, "ui: sign out after delete", "err", err)
	}
	u.auth.ClearSessionCookie(w)
	u.hub.publish(user.ID, userEvent{Deleted: true})
	u.redirect(w, r, config.AppRoutes.Home)
}
