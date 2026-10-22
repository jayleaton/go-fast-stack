package ui

import (
	"context"
	"net/http"

	"github.com/jayleaton/ai-patterns-go/apps/admin/internal/config"
	"github.com/jayleaton/ai-patterns-go/apps/admin/internal/services"
	"github.com/jayleaton/ai-patterns-go/apps/admin/internal/validate"
	"github.com/jayleaton/ai-patterns-go/apps/admin/ui/i18n"
)

func (u *UI) adminsPage(w http.ResponseWriter, r *http.Request) {
	a, ok := u.signedIn(w, r)
	if !ok {
		return
	}
	ctx := r.Context()
	admins, err := u.admins.ListAdmins(ctx, a)
	if u.needsTwoFactor(w, r, err) {
		return
	}
	if err != nil {
		u.render(w, r, statusOf(err), AdminsPage(AdminsData{SelfID: a.AdminID, Error: u.errorMessage(ctx, err)}))
		return
	}
	u.render(w, r, http.StatusOK, AdminsPage(AdminsData{Admins: admins, SelfID: a.AdminID}))
}

// inviteAdmin creates the admin (or re-sends a pending invite) and emails
// the set-password link; Datastar gets the refreshed table and a cleared form.
func (u *UI) inviteAdmin(w http.ResponseWriter, r *http.Request) {
	a, ok := u.signedInAction(w, r)
	if !ok {
		return
	}
	ctx := r.Context()
	in := validate.CreateAdminInvite{Email: r.PostFormValue("email"), Name: r.PostFormValue("name")}
	err := in.Validate()
	var invited services.Admin
	if err == nil {
		invited, err = u.admins.InviteAdmin(ctx, a, in)
	}
	if u.needsTwoFactor(w, r, err) {
		return
	}
	if err != nil {
		inv := InviteState{Email: r.PostFormValue("email"), Name: r.PostFormValue("name"), Error: u.errorMessage(ctx, err)}
		if isDatastar(r) {
			u.patchOnly(w, r, InviteCard(inv))
			return
		}
		admins, lerr := u.admins.ListAdmins(ctx, a)
		if lerr != nil {
			u.log.WarnContext(ctx, "ui: list admins after failed invite", "err", lerr)
		}
		u.render(w, r, http.StatusUnprocessableEntity, AdminsPage(AdminsData{Admins: admins, SelfID: a.AdminID, Invite: inv}))
		return
	}
	if !isDatastar(r) {
		http.Redirect(w, r, config.AppRoutes.Admins, http.StatusSeeOther)
		return
	}
	inv := InviteState{Sent: i18n.Tf(ctx, "pages.admins.inviteSent", invited.Email)}
	admins, err := u.admins.ListAdmins(ctx, a)
	if err != nil {
		u.log.WarnContext(ctx, "ui: reload admins after invite", "err", err)
		u.patchOnly(w, r, InviteCard(inv))
		return
	}
	u.patchOnly(w, r, AdminsTable(admins, a.AdminID), InviteCard(inv))
}

func (u *UI) disableAdmin(w http.ResponseWriter, r *http.Request) {
	u.adminAction(w, r, "pages.admins.status.disabled", u.admins.DisableAdmin)
}

func (u *UI) enableAdmin(w http.ResponseWriter, r *http.Request) {
	u.adminAction(w, r, "pages.admins.status.enabled", u.admins.EnableAdmin)
}

func (u *UI) removeAdmin(w http.ResponseWriter, r *http.Request) {
	u.adminAction(w, r, "pages.admins.status.removed", u.admins.RemoveAdmin)
}

// adminAction runs one mutation on the admin in the path. A Datastar request
// gets the refreshed admins table plus a status alert (or just an error
// alert); a plain post gets a 303 back to the admins page, or the full page
// with the error.
func (u *UI) adminAction(w http.ResponseWriter, r *http.Request, doneKey string, do func(ctx context.Context, a services.Actor, adminID string) error) {
	a, ok := u.signedInAction(w, r)
	if !ok {
		return
	}
	ctx := r.Context()
	err := do(ctx, a, r.PathValue("id"))
	if u.needsTwoFactor(w, r, err) {
		return
	}
	if err != nil {
		st := Status{Error: true, Message: u.errorMessage(ctx, err)}
		if isDatastar(r) {
			u.patchOnly(w, r, AdminsStatus(st))
			return
		}
		admins, lerr := u.admins.ListAdmins(ctx, a)
		if lerr != nil {
			u.log.WarnContext(ctx, "ui: list admins after failed action", "err", lerr)
		}
		u.render(w, r, http.StatusUnprocessableEntity, AdminsPage(AdminsData{Admins: admins, SelfID: a.AdminID, Status: st}))
		return
	}
	if !isDatastar(r) {
		http.Redirect(w, r, config.AppRoutes.Admins, http.StatusSeeOther)
		return
	}
	st := Status{Message: i18n.T(ctx, doneKey)}
	admins, err := u.admins.ListAdmins(ctx, a)
	if err != nil {
		u.log.WarnContext(ctx, "ui: reload admins after action", "err", err)
		u.patchOnly(w, r, AdminsStatus(st))
		return
	}
	u.patchOnly(w, r, AdminsTable(admins, a.AdminID), AdminsStatus(st))
}

func (u *UI) auditPage(w http.ResponseWriter, r *http.Request) {
	a, ok := u.signedIn(w, r)
	if !ok {
		return
	}
	ctx := r.Context()
	q := r.URL.Query()
	d := AuditData{Filters: auditFilters(q)}
	in, err := validate.DecodeQuery[validate.ListAuditLog](q)
	var page services.AuditPage
	if err == nil {
		page, err = u.audit.List(ctx, a, in)
	}
	if u.needsTwoFactor(w, r, err) {
		return
	}
	if err != nil {
		d.Error = u.errorMessage(ctx, err)
		u.render(w, r, statusOf(err), AuditPage(d))
		return
	}
	d.Entries = page.Entries
	d.Pager = pager(page.Offset, page.Limit, len(page.Entries), page.Total, d.Filters.url)
	u.render(w, r, http.StatusOK, AuditPage(d))
}

func (u *UI) analyticsPage(w http.ResponseWriter, r *http.Request) {
	a, ok := u.signedIn(w, r)
	if !ok {
		return
	}
	ctx := r.Context()
	o, err := u.analytics.Overview(ctx, a)
	if u.needsTwoFactor(w, r, err) {
		return
	}
	if err != nil {
		u.render(w, r, statusOf(err), AnalyticsPage(AnalyticsData{Error: u.errorMessage(ctx, err)}))
		return
	}
	u.render(w, r, http.StatusOK, AnalyticsPage(AnalyticsData{Overview: o}))
}
