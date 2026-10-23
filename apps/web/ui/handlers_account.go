package ui

import (
	"context"
	"net/http"
	"strings"

	"github.com/a-h/templ"

	"github.com/jayleaton/go-fast-stack/apps/web/internal/access"
	"github.com/jayleaton/go-fast-stack/apps/web/internal/config"
	"github.com/jayleaton/go-fast-stack/apps/web/internal/services"
	"github.com/jayleaton/go-fast-stack/apps/web/internal/validate"
	"github.com/jayleaton/go-fast-stack/apps/web/ui/i18n"
)

// accountData loads the Account page; load failures show as a banner.
func (ui *UI) accountData(ctx context.Context, caller access.Caller) AccountData {
	account := AccountData{User: User(caller.User)}
	actor := caller.Actor()
	var err error
	if account.Sessions, err = ui.users.ListSessions(ctx, actor, caller.User.ID); err != nil {
		account.LoadError = ui.serviceMessage(ctx, err)
	}
	if account.Summary, err = ui.users.DataSummary(ctx, actor, caller.User.ID); err != nil {
		account.LoadError = ui.serviceMessage(ctx, err)
	}
	return account
}

func (ui *UI) accountPage(writer http.ResponseWriter, request *http.Request, caller access.Caller) {
	account := ui.accountData(request.Context(), caller)
	// The change-email link lands here; on failure auth adds ?error=<code>.
	if code := request.URL.Query().Get("error"); code != "" {
		account.LoadError = authCodeMessage(request.Context(), code)
	} else {
		account.EmailChanged = request.URL.Query().Get("email-changed") == "1"
	}
	writer.Header().Set("Cache-Control", "no-store")
	ui.render(writer, request, http.StatusOK, AccountPage(account))
}

// accountResult answers an account form: Datastar gets the fragments, a
// plain post goes back to the page (on error with the message as a banner).
func (ui *UI) accountResult(writer http.ResponseWriter, request *http.Request, caller access.Caller, errMsg string, fragments ...templ.Component) {
	if isDatastar(request) {
		ui.patch(writer, request, http.StatusOK, nil, fragments...)
		return
	}
	if errMsg == "" {
		http.Redirect(writer, request, config.AppRoutes.Account, http.StatusSeeOther)
		return
	}
	account := ui.accountData(request.Context(), caller)
	account.LoadError = errMsg
	ui.render(writer, request, http.StatusUnprocessableEntity, AccountPage(account))
}

func (ui *UI) updateProfile(writer http.ResponseWriter, request *http.Request, caller access.Caller) {
	ctx := request.Context()
	name := request.PostFormValue("name")
	in := validate.UpdateUserSettings{Name: &name, Image: validate.Nullable[string]{Set: true}}
	if img := strings.TrimSpace(request.PostFormValue("image")); img != "" {
		in.Image.Value = &img
	}
	err := in.Validate()
	var saved services.User
	if err == nil {
		saved, err = ui.users.UpdateProfile(ctx, caller.Actor(), caller.User.ID, in)
	}
	if err != nil {
		msg := ui.serviceMessage(ctx, err)
		ui.accountResult(writer, request, caller, msg, StatusAlert(profileStatusID, Status{Error: msg}))
		return
	}
	updated := userFromService(saved)
	ui.hub.publish(caller.User.ID, userEvent{User: updated})
	ui.accountResult(writer, request, caller, "", ProfileCard(updated, Status{Message: i18n.T(ctx, "pages.account.profile.saved")}))
}

func (ui *UI) requestEmailChange(writer http.ResponseWriter, request *http.Request, caller access.Caller) {
	ctx := request.Context()
	in := validate.UpdateUserEmail{NewEmail: request.PostFormValue("newEmail"), CallbackURL: config.AppRoutes.EmailChanged}
	err := in.Validate()
	if err == nil {
		err = ui.users.RequestEmailChange(ctx, caller.Actor(), caller.User.ID, in)
	}
	if err != nil {
		msg := ui.serviceMessage(ctx, err)
		ui.accountResult(writer, request, caller, msg, StatusAlert(emailStatusID, Status{Error: msg}))
		return
	}
	ui.accountResult(writer, request, caller, "", EmailCard(caller.User.Email, in.NewEmail, Status{}))
}

func (ui *UI) changePassword(writer http.ResponseWriter, request *http.Request, caller access.Caller) {
	ctx := request.Context()
	in := validate.UpdateUserPassword{
		CurrentPassword:     request.PostFormValue("currentPassword"),
		NewPassword:         request.PostFormValue("newPassword"),
		RevokeOtherSessions: request.PostFormValue("revokeOtherSessions") == "on",
	}
	fail := func(msg string) {
		ui.accountResult(writer, request, caller, msg, StatusAlert(passwordStatusID, Status{Error: msg}))
	}
	if in.NewPassword != request.PostFormValue("confirmPassword") {
		fail(i18n.T(ctx, "pages.account.password.mismatch"))
		return
	}
	err := in.Validate()
	if err == nil {
		err = ui.users.ChangePassword(ctx, caller.Actor(), caller.User.ID, in)
	}
	if err != nil {
		fail(ui.serviceMessage(ctx, err))
		return
	}
	done := "pages.account.password.changed"
	if in.RevokeOtherSessions {
		done = "pages.account.password.changedRevoked"
		ui.hub.publish(caller.User.ID, userEvent{KeepSession: caller.Session.ID})
	}
	ui.accountResult(writer, request, caller, "",
		replaced{PasswordCard(caller.User.Email, Status{Message: i18n.T(ctx, done)})},
		ui.sessionsCard(ctx, caller, Status{}))
}

// sessionsCard re-lists the sessions with s as the outcome line.
func (ui *UI) sessionsCard(ctx context.Context, caller access.Caller, status Status) templ.Component {
	sessions, err := ui.users.ListSessions(ctx, caller.Actor(), caller.User.ID)
	if err != nil && status.Error == "" {
		status = Status{Error: ui.serviceMessage(ctx, err)}
	}
	return SessionsCard(sessions, status)
}

func (ui *UI) revokeSession(writer http.ResponseWriter, request *http.Request, caller access.Caller) {
	ctx := request.Context()
	id := request.PostFormValue("sessionId")
	if err := ui.users.RevokeSession(ctx, caller.Actor(), caller.User.ID, id); err != nil {
		msg := ui.serviceMessage(ctx, err)
		ui.accountResult(writer, request, caller, msg, ui.sessionsCard(ctx, caller, Status{Error: msg}))
		return
	}
	ui.hub.publish(caller.User.ID, userEvent{RevokedSession: id})
	ui.accountResult(writer, request, caller, "", ui.sessionsCard(ctx, caller, Status{Message: i18n.T(ctx, "pages.account.sessions.revoked")}))
}

func (ui *UI) revokeOtherSessions(writer http.ResponseWriter, request *http.Request, caller access.Caller) {
	ctx := request.Context()
	if err := ui.users.RevokeOtherSessions(ctx, caller.Actor(), caller.User.ID); err != nil {
		msg := ui.serviceMessage(ctx, err)
		ui.accountResult(writer, request, caller, msg, ui.sessionsCard(ctx, caller, Status{Error: msg}))
		return
	}
	ui.hub.publish(caller.User.ID, userEvent{KeepSession: caller.Session.ID})
	ui.accountResult(writer, request, caller, "", ui.sessionsCard(ctx, caller, Status{Message: i18n.T(ctx, "pages.account.sessions.othersRevoked")}))
}

func (ui *UI) deleteAccount(writer http.ResponseWriter, request *http.Request, caller access.Caller) {
	ctx := request.Context()
	if err := ui.users.DeleteAccount(ctx, caller.Actor(), caller.User.ID); err != nil {
		msg := ui.serviceMessage(ctx, err)
		ui.accountResult(writer, request, caller, msg, StatusAlert(deleteErrorID, Status{Error: msg}))
		return
	}
	// Sessions go with the user row; sign out anyway in case they do not cascade.
	if err := ui.auth.SignOut(ctx, caller.Session.Token); err != nil {
		ui.log.WarnContext(ctx, "ui: sign out after delete", "err", err)
	}
	ui.auth.ClearSessionCookie(writer)
	ui.hub.publish(caller.User.ID, userEvent{Deleted: true})
	ui.redirect(writer, request, config.AppRoutes.Home)
}
