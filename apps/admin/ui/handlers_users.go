package ui

import (
	"context"
	"net/http"
	"strconv"
	"strings"

	"github.com/jayleaton/go-fast-stack/apps/admin/internal/access"
	"github.com/jayleaton/go-fast-stack/apps/admin/internal/config"
	"github.com/jayleaton/go-fast-stack/apps/admin/internal/services"
	"github.com/jayleaton/go-fast-stack/apps/admin/internal/validate"
	"github.com/jayleaton/go-fast-stack/apps/admin/ui/i18n"
)

func (ui *UI) usersPage(writer http.ResponseWriter, request *http.Request, caller access.Caller) {
	actor := caller.Actor()
	ctx := request.Context()
	query := request.URL.Query()
	data := UsersData{Filters: userFilters(query)}
	data.Sort = data.Filters.sortLinks()
	in, err := validate.DecodeQuery[validate.ListUsers](query)
	var page services.UserPage
	if err == nil {
		page, err = ui.users.ListUsers(ctx, actor, in)
	}
	if ui.needsTwoFactor(writer, request, err) {
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

func (ui *UI) userPage(writer http.ResponseWriter, request *http.Request, caller access.Caller) {
	actor := caller.Actor()
	ctx := request.Context()
	data, err := ui.users.GetUser(ctx, actor, request.PathValue("id"))
	if ui.needsTwoFactor(writer, request, err) {
		return
	}
	if err != nil {
		ui.render(writer, request, statusOf(err), UserPage(UserData{Missing: true, Status: Status{Error: true, Message: ui.errorMessage(ctx, err)}}))
		return
	}
	ui.render(writer, request, http.StatusOK, UserPage(UserData{Detail: data}))
}

func (ui *UI) banUser(writer http.ResponseWriter, request *http.Request, caller access.Caller) {
	ui.userAction(writer, request, caller, "pages.user.status.banned", func(ctx context.Context, actor services.Actor, id string) error {
		in, err := banInput(request.PostFormValue("banReason"), request.PostFormValue("banExpiresIn"))
		if err != nil {
			return err
		}
		return ui.users.BanUser(ctx, actor, id, in)
	})
}

func (ui *UI) unbanUser(writer http.ResponseWriter, request *http.Request, caller access.Caller) {
	ui.userAction(writer, request, caller, "pages.user.status.unbanned", func(ctx context.Context, actor services.Actor, id string) error {
		return ui.users.UnbanUser(ctx, actor, id)
	})
}

func (ui *UI) setRole(writer http.ResponseWriter, request *http.Request, caller access.Caller) {
	ui.userAction(writer, request, caller, "pages.user.status.roleSet", func(ctx context.Context, actor services.Actor, id string) error {
		in := validate.SetUserRole{Role: request.PostFormValue("role")}
		if err := in.Validate(); err != nil {
			return err
		}
		return ui.users.SetRole(ctx, actor, id, in)
	})
}

func (ui *UI) revokeSessions(writer http.ResponseWriter, request *http.Request, caller access.Caller) {
	ui.userAction(writer, request, caller, "pages.user.status.sessionsRevoked", func(ctx context.Context, actor services.Actor, id string) error {
		return ui.users.RevokeSessions(ctx, actor, id)
	})
}

func (ui *UI) revokeSession(writer http.ResponseWriter, request *http.Request, caller access.Caller) {
	ui.userAction(writer, request, caller, "pages.user.status.sessionRevoked", func(ctx context.Context, actor services.Actor, id string) error {
		return ui.users.RevokeSession(ctx, actor, id, request.PathValue("sessionId"))
	})
}

// userAction runs one mutation on the user in the path. A Datastar request
// gets the refreshed profile, ban and sessions fragments plus a status alert
// (or just an error alert); a plain post gets a 303 back to the user page, or
// the full page with the error.
func (ui *UI) userAction(writer http.ResponseWriter, request *http.Request, caller access.Caller, doneKey string, do func(ctx context.Context, actor services.Actor, id string) error) {
	actor := caller.Actor()
	ctx := request.Context()
	id := request.PathValue("id")
	err := do(ctx, actor, id)
	if ui.needsTwoFactor(writer, request, err) {
		return
	}
	if err != nil {
		st := Status{Error: true, Message: ui.errorMessage(ctx, err)}
		if isDatastar(request) {
			ui.patchOnly(writer, request, UserStatus(st))
			return
		}
		data, gerr := ui.users.GetUser(ctx, actor, id)
		ui.render(writer, request, http.StatusUnprocessableEntity, UserPage(UserData{Detail: data, Missing: gerr != nil, Status: st}))
		return
	}
	if !isDatastar(request) {
		http.Redirect(writer, request, config.UserPage(id), http.StatusSeeOther)
		return
	}
	st := Status{Message: i18n.T(ctx, doneKey)}
	data, err := ui.users.GetUser(ctx, actor, id)
	if err != nil {
		ui.log.WarnContext(ctx, "ui: reload user after action", "err", err)
		ui.patchOnly(writer, request, UserStatus(st))
		return
	}
	ui.patchOnly(writer, request, UserProfile(data.User), UserBan(data.User), UserSessions(data.User, data.Sessions), UserStatus(st))
}

// banInput reads the ban form: the reason and the expiry select in seconds
// ("0" or empty: permanent).
func banInput(reason, expiresIn string) (validate.BanUser, error) {
	in := validate.BanUser{BanReason: reason}
	if value := strings.TrimSpace(expiresIn); value != "" && value != "0" {
		seconds, err := strconv.ParseInt(value, 10, 64)
		if err != nil {
			return in, validate.Fields{"banExpiresIn": "must be a whole number"}.Err()
		}
		in.BanExpiresIn = &seconds
	}
	return in, in.Validate()
}
