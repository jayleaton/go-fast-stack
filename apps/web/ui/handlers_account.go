package ui

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/a-h/templ"

	"github.com/jayleaton/go-fast-stack/apps/web/internal/apiclient"
	"github.com/jayleaton/go-fast-stack/apps/web/internal/config"
	"github.com/jayleaton/go-fast-stack/apps/web/ui/i18n"
)

// accountData loads the Account page through the API; load failures show
// as a banner, an access refusal is returned for the caller to answer.
func (ui *UI) accountData(ctx context.Context, caller apiclient.Caller) (AccountData, error) {
	account := AccountData{User: caller.User}
	var err error
	for _, load := range []func() error{
		func() error { account.Sessions, err = ui.api.ListSessions(ctx); return err },
		func() error { account.Summary, err = ui.api.DataSummary(ctx); return err },
	} {
		if err := load(); err != nil {
			if errors.Is(err, apiclient.ErrSignedOut) || errors.Is(err, apiclient.ErrDenied) {
				return account, err
			}
			account.LoadError = ui.serviceMessage(ctx, err)
		}
	}
	return account, nil
}

func (ui *UI) accountPage(writer http.ResponseWriter, request *http.Request, caller apiclient.Caller) {
	account, err := ui.accountData(request.Context(), caller)
	if ui.refused(writer, request, err) {
		return
	}
	// The change-email link lands here; on failure auth adds ?error=<code>.
	if code := request.URL.Query().Get("error"); code != "" {
		account.LoadError = ui.authCodeMessage(request.Context(), code)
	} else {
		account.EmailChanged = request.URL.Query().Get("email-changed") == "1"
	}
	writer.Header().Set("Cache-Control", "no-store")
	ui.render(writer, request, http.StatusOK, AccountPage(account))
}

// accountResult answers an account form: Datastar gets the fragments, a
// plain post goes back to the page (on error with the message as a banner).
func (ui *UI) accountResult(writer http.ResponseWriter, request *http.Request, caller apiclient.Caller, errMsg string, fragments ...templ.Component) {
	if isDatastar(request) {
		ui.patch(writer, request, http.StatusOK, nil, fragments...)
		return
	}
	if errMsg == "" {
		http.Redirect(writer, request, config.AppRoutes.Account, http.StatusSeeOther)
		return
	}
	account, err := ui.accountData(request.Context(), caller)
	if ui.refused(writer, request, err) {
		return
	}
	account.LoadError = errMsg
	ui.render(writer, request, http.StatusUnprocessableEntity, AccountPage(account))
}

func (ui *UI) updateProfile(writer http.ResponseWriter, request *http.Request, caller apiclient.Caller) {
	ctx := request.Context()
	name := request.PostFormValue("name")
	in := apiclient.UpdateUserSettings{Name: &name, Image: apiclient.Nullable[string]{Set: true}}
	if img := strings.TrimSpace(request.PostFormValue("image")); img != "" {
		in.Image.Value = &img
	}
	updatedUser, err := ui.api.UpdateMe(ctx, in) // the API validates
	if err != nil {
		if ui.refused(writer, request, err) {
			return
		}
		msg := ui.serviceMessage(ctx, err)
		ui.accountResult(writer, request, caller, msg, StatusAlert(profileStatusID, Status{Error: msg}))
		return
	}
	updated := updatedUser.User
	ui.hub.publish(caller.User.ID, userEvent{User: updated})
	ui.accountResult(writer, request, caller, "", ProfileCard(updated, Status{Message: i18n.Translate(ctx, "pages.account.profile.saved")}))
}

func (ui *UI) requestEmailChange(writer http.ResponseWriter, request *http.Request, caller apiclient.Caller) {
	ctx := request.Context()
	in := apiclient.UpdateUserEmail{NewEmail: request.PostFormValue("newEmail"), CallbackURL: config.AppRoutes.EmailChanged}
	if err := ui.api.RequestEmailChange(ctx, in); err != nil {
		if ui.refused(writer, request, err) {
			return
		}
		msg := ui.serviceMessage(ctx, err)
		ui.accountResult(writer, request, caller, msg, StatusAlert(emailStatusID, Status{Error: msg}))
		return
	}
	ui.accountResult(writer, request, caller, "", EmailCard(caller.User.Email, strings.ToLower(strings.TrimSpace(in.NewEmail)), Status{}))
}

func (ui *UI) changePassword(writer http.ResponseWriter, request *http.Request, caller apiclient.Caller) {
	ctx := request.Context()
	in := apiclient.UpdateUserPassword{
		CurrentPassword:     request.PostFormValue("currentPassword"),
		NewPassword:         request.PostFormValue("newPassword"),
		RevokeOtherSessions: request.PostFormValue("revokeOtherSessions") == "on",
	}
	fail := func(msg string) {
		ui.accountResult(writer, request, caller, msg, StatusAlert(passwordStatusID, Status{Error: msg}))
	}
	if in.NewPassword != request.PostFormValue("confirmPassword") {
		fail(i18n.Translate(ctx, "pages.account.password.mismatch"))
		return
	}
	if err := ui.api.ChangePassword(ctx, in); err != nil {
		if ui.refused(writer, request, err) {
			return
		}
		fail(ui.serviceMessage(ctx, err))
		return
	}
	done := "pages.account.password.changed"
	if in.RevokeOtherSessions {
		done = "pages.account.password.changedRevoked"
		ui.hub.publish(caller.User.ID, userEvent{KeepSession: caller.Session.ID})
	}
	ui.accountResult(writer, request, caller, "",
		replaced{PasswordCard(caller.User.Email, Status{Message: i18n.Translate(ctx, done)})},
		ui.sessionsCard(ctx, caller, Status{}))
}

// sessionsCard re-lists the sessions with s as the outcome line.
func (ui *UI) sessionsCard(ctx context.Context, _ apiclient.Caller, status Status) templ.Component {
	sessions, err := ui.api.ListSessions(ctx)
	if err != nil && status.Error == "" {
		status = Status{Error: ui.serviceMessage(ctx, err)}
	}
	return SessionsCard(sessions, status)
}

func (ui *UI) revokeSession(writer http.ResponseWriter, request *http.Request, caller apiclient.Caller) {
	ctx := request.Context()
	id := request.PostFormValue("sessionId")
	if err := ui.api.RevokeSession(ctx, id); err != nil {
		if ui.refused(writer, request, err) {
			return
		}
		msg := ui.serviceMessage(ctx, err)
		ui.accountResult(writer, request, caller, msg, ui.sessionsCard(ctx, caller, Status{Error: msg}))
		return
	}
	ui.hub.publish(caller.User.ID, userEvent{RevokedSession: id})
	ui.accountResult(writer, request, caller, "", ui.sessionsCard(ctx, caller, Status{Message: i18n.Translate(ctx, "pages.account.sessions.revoked")}))
}

func (ui *UI) revokeOtherSessions(writer http.ResponseWriter, request *http.Request, caller apiclient.Caller) {
	ctx := request.Context()
	if err := ui.api.RevokeOtherSessions(ctx); err != nil {
		if ui.refused(writer, request, err) {
			return
		}
		msg := ui.serviceMessage(ctx, err)
		ui.accountResult(writer, request, caller, msg, ui.sessionsCard(ctx, caller, Status{Error: msg}))
		return
	}
	ui.hub.publish(caller.User.ID, userEvent{KeepSession: caller.Session.ID})
	ui.accountResult(writer, request, caller, "", ui.sessionsCard(ctx, caller, Status{Message: i18n.Translate(ctx, "pages.account.sessions.othersRevoked")}))
}

func (ui *UI) deleteAccount(writer http.ResponseWriter, request *http.Request, caller apiclient.Caller) {
	ctx := request.Context()
	if err := ui.api.DeleteMe(ctx); err != nil {
		if ui.refused(writer, request, err) {
			return
		}
		msg := ui.serviceMessage(ctx, err)
		ui.accountResult(writer, request, caller, msg, StatusAlert(deleteErrorID, Status{Error: msg}))
		return
	}
	// Sessions go with the user row; sign out anyway in case they do not cascade.
	signedOut, err := ui.api.SignOut(ctx)
	if err != nil {
		ui.log.WarnContext(ctx, "ui: sign out after delete", "err", err)
	}
	setCookies(writer, signedOut.Cookies)
	ui.hub.publish(caller.User.ID, userEvent{Deleted: true})
	ui.redirect(writer, request, config.AppRoutes.Home)
}
