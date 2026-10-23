package ui

import (
	"context"
	"net/http"

	"github.com/jayleaton/go-fast-stack/apps/admin/internal/apiclient"
	"github.com/jayleaton/go-fast-stack/apps/admin/internal/config"
	"github.com/jayleaton/go-fast-stack/apps/admin/ui/i18n"
)

func (ui *UI) adminsPage(writer http.ResponseWriter, request *http.Request, caller apiclient.Caller) {
	ctx := request.Context()
	admins, err := ui.api.ListAdmins(ctx)
	if ui.handled(writer, request, err) {
		return
	}
	if err != nil {
		ui.render(writer, request, statusOf(err), AdminsPage(AdminsData{SelfID: caller.Admin.ID, Error: ui.errorMessage(ctx, err)}))
		return
	}
	ui.render(writer, request, http.StatusOK, AdminsPage(AdminsData{Admins: admins, SelfID: caller.Admin.ID}))
}

// inviteAdmin creates the admin (or re-sends a pending invite) and emails
// the set-password link; Datastar gets the refreshed table and a cleared form.
func (ui *UI) inviteAdmin(writer http.ResponseWriter, request *http.Request, caller apiclient.Caller) {
	ctx := request.Context()
	in := apiclient.CreateAdminInvite{Email: request.PostFormValue("email"), Name: request.PostFormValue("name")}
	invited, err := ui.api.InviteAdmin(ctx, in) // the API validates
	if ui.handled(writer, request, err) {
		return
	}
	if err != nil {
		inv := InviteState{Email: request.PostFormValue("email"), Name: request.PostFormValue("name"), Error: ui.errorMessage(ctx, err)}
		if isDatastar(request) {
			ui.patchOnly(writer, request, InviteCard(inv))
			return
		}
		admins, lerr := ui.api.ListAdmins(ctx)
		if lerr != nil {
			ui.log.WarnContext(ctx, "ui: list admins after failed invite", "err", lerr)
		}
		ui.render(writer, request, http.StatusUnprocessableEntity, AdminsPage(AdminsData{Admins: admins, SelfID: caller.Admin.ID, Invite: inv}))
		return
	}
	if !isDatastar(request) {
		http.Redirect(writer, request, config.AppRoutes.Admins, http.StatusSeeOther)
		return
	}
	inv := InviteState{Sent: i18n.Tf(ctx, "pages.admins.inviteSent", invited.Email)}
	admins, err := ui.api.ListAdmins(ctx)
	if err != nil {
		ui.log.WarnContext(ctx, "ui: reload admins after invite", "err", err)
		ui.patchOnly(writer, request, InviteCard(inv))
		return
	}
	ui.patchOnly(writer, request, AdminsTable(admins, caller.Admin.ID), InviteCard(inv))
}

func (ui *UI) disableAdmin(writer http.ResponseWriter, request *http.Request, caller apiclient.Caller) {
	ui.adminAction(writer, request, caller, "pages.admins.status.disabled", ui.api.DisableAdmin)
}

func (ui *UI) enableAdmin(writer http.ResponseWriter, request *http.Request, caller apiclient.Caller) {
	ui.adminAction(writer, request, caller, "pages.admins.status.enabled", ui.api.EnableAdmin)
}

func (ui *UI) removeAdmin(writer http.ResponseWriter, request *http.Request, caller apiclient.Caller) {
	ui.adminAction(writer, request, caller, "pages.admins.status.removed", ui.api.RemoveAdmin)
}

func (ui *UI) sendAdminPasswordReset(writer http.ResponseWriter, request *http.Request, caller apiclient.Caller) {
	ui.adminAction(writer, request, caller, "pages.admins.status.passwordReset", ui.api.SendPasswordReset)
}

func (ui *UI) resetAdminTwoFactor(writer http.ResponseWriter, request *http.Request, caller apiclient.Caller) {
	ui.adminAction(writer, request, caller, "pages.admins.status.twoFactorReset", ui.api.ResetTwoFactor)
}

// adminAction runs one mutation on the admin in the path. A Datastar request
// gets the refreshed admins table plus a status alert (or just an error
// alert); a plain post gets a 303 back to the admins page, or the full page
// with the error.
func (ui *UI) adminAction(writer http.ResponseWriter, request *http.Request, caller apiclient.Caller, doneKey string, do func(ctx context.Context, adminID string) error) {
	ctx := request.Context()
	err := do(ctx, request.PathValue("id"))
	if ui.handled(writer, request, err) {
		return
	}
	if err != nil {
		st := Status{Error: true, Message: ui.errorMessage(ctx, err)}
		if isDatastar(request) {
			ui.patchOnly(writer, request, AdminsStatus(st))
			return
		}
		admins, lerr := ui.api.ListAdmins(ctx)
		if lerr != nil {
			ui.log.WarnContext(ctx, "ui: list admins after failed action", "err", lerr)
		}
		ui.render(writer, request, http.StatusUnprocessableEntity, AdminsPage(AdminsData{Admins: admins, SelfID: caller.Admin.ID, Status: st}))
		return
	}
	if !isDatastar(request) {
		http.Redirect(writer, request, config.AppRoutes.Admins, http.StatusSeeOther)
		return
	}
	st := Status{Message: i18n.T(ctx, doneKey)}
	admins, err := ui.api.ListAdmins(ctx)
	if err != nil {
		ui.log.WarnContext(ctx, "ui: reload admins after action", "err", err)
		ui.patchOnly(writer, request, AdminsStatus(st))
		return
	}
	ui.patchOnly(writer, request, AdminsTable(admins, caller.Admin.ID), AdminsStatus(st))
}

func (ui *UI) auditPage(writer http.ResponseWriter, request *http.Request, caller apiclient.Caller) {
	ctx := request.Context()
	query := request.URL.Query()
	data := AuditData{Filters: auditFilters(query)}
	var in apiclient.ListAuditLog
	err := in.DecodeQuery(query) // shape only: the API validates
	var page apiclient.AuditPage
	if err == nil {
		page, err = ui.api.ListAuditLog(ctx, in)
	}
	if ui.handled(writer, request, err) {
		return
	}
	if err != nil {
		data.Error = ui.errorMessage(ctx, err)
		ui.render(writer, request, statusOf(err), AuditPage(data))
		return
	}
	data.Entries = page.Entries
	data.Pager = pager(page.Offset, page.Limit, len(page.Entries), page.Total, data.Filters.url)
	ui.render(writer, request, http.StatusOK, AuditPage(data))
}

func (ui *UI) analyticsPage(writer http.ResponseWriter, request *http.Request, caller apiclient.Caller) {
	ctx := request.Context()
	overview, err := ui.api.Overview(ctx)
	if ui.handled(writer, request, err) {
		return
	}
	if err != nil {
		ui.render(writer, request, statusOf(err), AnalyticsPage(AnalyticsData{Error: ui.errorMessage(ctx, err)}))
		return
	}
	ui.render(writer, request, http.StatusOK, AnalyticsPage(AnalyticsData{Overview: overview}))
}
