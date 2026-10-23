package ui

import (
	"context"
	"net/http"
	"strconv"
	"strings"

	"github.com/jayleaton/go-fast-stack/apps/admin/internal/apiclient"
	"github.com/jayleaton/go-fast-stack/apps/admin/internal/config"
	"github.com/jayleaton/go-fast-stack/apps/admin/ui/i18n"
)

func (ui *UI) usersPage(writer http.ResponseWriter, request *http.Request, _ apiclient.Caller) {
	ctx := request.Context()
	query := request.URL.Query()
	data := UsersData{Filters: userFilters(query)}
	data.Sort = data.Filters.sortLinks()
	var input apiclient.ListUsers
	err := input.DecodeQuery(query) // shape only: the API validates
	var page apiclient.UserPage
	if err == nil {
		page, err = ui.api.ListUsers(ctx, input)
	}
	if ui.handled(writer, request, err) {
		return
	}
	if err != nil {
		data.Error = ui.errorMessage(ctx, err)
		ui.render(writer, request, statusOf(err), UsersPage(data))
		return
	}
	data.Users = page.Users
	data.Pager = pager(page.Offset, page.Limit, len(page.Users), page.Total, data.Filters.url)
	ui.render(writer, request, http.StatusOK, UsersPage(data))
}

func (ui *UI) userPage(writer http.ResponseWriter, request *http.Request, _ apiclient.Caller) {
	ctx := request.Context()
	data, err := ui.api.GetUser(ctx, request.PathValue("id"))
	if ui.handled(writer, request, err) {
		return
	}
	if err != nil {
		ui.render(writer, request, statusOf(err), UserPage(UserData{Missing: true, Status: Status{Error: true, Message: ui.errorMessage(ctx, err)}}))
		return
	}
	ui.render(writer, request, http.StatusOK, UserPage(UserData{Detail: data}))
}

func (ui *UI) banUser(writer http.ResponseWriter, request *http.Request, caller apiclient.Caller) {
	ui.userAction(writer, request, caller, "pages.user.status.banned", func(ctx context.Context, id string) error {
		input, err := banInput(request.PostFormValue("banReason"), request.PostFormValue("banExpiresIn"))
		if err != nil {
			return err
		}
		return ui.api.BanUser(ctx, id, input)
	})
}

func (ui *UI) unbanUser(writer http.ResponseWriter, request *http.Request, caller apiclient.Caller) {
	ui.userAction(writer, request, caller, "pages.user.status.unbanned", func(ctx context.Context, id string) error {
		return ui.api.UnbanUser(ctx, id)
	})
}

func (ui *UI) setRole(writer http.ResponseWriter, request *http.Request, caller apiclient.Caller) {
	ui.userAction(writer, request, caller, "pages.user.status.roleSet", func(ctx context.Context, id string) error {
		return ui.api.SetRole(ctx, id, apiclient.SetUserRole{Role: request.PostFormValue("role")})
	})
}

func (ui *UI) revokeSessions(writer http.ResponseWriter, request *http.Request, caller apiclient.Caller) {
	ui.userAction(writer, request, caller, "pages.user.status.sessionsRevoked", func(ctx context.Context, id string) error {
		return ui.api.RevokeSessions(ctx, id)
	})
}

func (ui *UI) revokeSession(writer http.ResponseWriter, request *http.Request, caller apiclient.Caller) {
	ui.userAction(writer, request, caller, "pages.user.status.sessionRevoked", func(ctx context.Context, id string) error {
		return ui.api.RevokeSession(ctx, id, request.PathValue("sessionId"))
	})
}

// userAction runs one mutation on the user in the path. A Datastar request
// gets the refreshed profile, ban and sessions fragments plus a status alert
// (or just an error alert); a plain post gets a 303 back to the user page, or
// the full page with the error.
func (ui *UI) userAction(writer http.ResponseWriter, request *http.Request, _ apiclient.Caller, doneKey string, do func(ctx context.Context, id string) error) {
	ctx := request.Context()
	id := request.PathValue("id")
	err := do(ctx, id)
	if ui.handled(writer, request, err) {
		return
	}
	if err != nil {
		st := Status{Error: true, Message: ui.errorMessage(ctx, err)}
		if isDatastar(request) {
			ui.patchOnly(writer, request, UserStatus(st))
			return
		}
		data, gerr := ui.api.GetUser(ctx, id)
		ui.render(writer, request, http.StatusUnprocessableEntity, UserPage(UserData{Detail: data, Missing: gerr != nil, Status: st}))
		return
	}
	if !isDatastar(request) {
		http.Redirect(writer, request, config.UserPage(id), http.StatusSeeOther)
		return
	}
	st := Status{Message: i18n.T(ctx, doneKey)}
	data, err := ui.api.GetUser(ctx, id)
	if err != nil {
		ui.log.WarnContext(ctx, "ui: reload user after action", "err", err)
		ui.patchOnly(writer, request, UserStatus(st))
		return
	}
	ui.patchOnly(writer, request, UserProfile(data.User), UserBan(data.User), UserSessions(data.User, data.Sessions), UserStatus(st))
}

// banInput reads the ban form: the reason and the expiry select in seconds
// ("0" or empty: permanent). Only the number is parsed here; the API
// validates the input.
func banInput(reason, expiresIn string) (apiclient.BanUser, error) {
	in := apiclient.BanUser{BanReason: reason}
	if value := strings.TrimSpace(expiresIn); value != "" && value != "0" {
		seconds, err := strconv.ParseInt(value, 10, 64)
		if err != nil {
			return in, apiclient.ValidationFields{"banExpiresIn": "must be a whole number"}.Err()
		}
		in.BanExpiresIn = &seconds
	}
	return in, nil
}
