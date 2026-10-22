package ui

import (
	"context"
	"net/http"
	"strconv"
	"strings"

	"github.com/jayleaton/ai-patterns-go/apps/admin/internal/config"
	"github.com/jayleaton/ai-patterns-go/apps/admin/internal/services"
	"github.com/jayleaton/ai-patterns-go/apps/admin/internal/validate"
	"github.com/jayleaton/ai-patterns-go/apps/admin/ui/i18n"
)

func (u *UI) usersPage(w http.ResponseWriter, r *http.Request) {
	a, ok := u.signedIn(w, r)
	if !ok {
		return
	}
	ctx := r.Context()
	q := r.URL.Query()
	d := UsersData{Filters: userFilters(q)}
	d.Sort = d.Filters.sortLinks()
	in, err := validate.DecodeQuery[validate.ListUsers](q)
	var page services.UserPage
	if err == nil {
		page, err = u.users.ListUsers(ctx, a, in)
	}
	if u.needsTwoFactor(w, r, err) {
		return
	}
	if err != nil {
		d.Error = u.errorMessage(ctx, err)
		u.render(w, r, statusOf(err), UsersPage(d))
		return
	}
	d.Users = page.Users
	d.Pager = pager(page.Offset, page.Limit, len(page.Users), page.Total, d.Filters.url)
	u.render(w, r, http.StatusOK, UsersPage(d))
}

func (u *UI) userPage(w http.ResponseWriter, r *http.Request) {
	a, ok := u.signedIn(w, r)
	if !ok {
		return
	}
	ctx := r.Context()
	d, err := u.users.GetUser(ctx, a, r.PathValue("id"))
	if u.needsTwoFactor(w, r, err) {
		return
	}
	if err != nil {
		u.render(w, r, statusOf(err), UserPage(UserData{Missing: true, Status: Status{Error: true, Message: u.errorMessage(ctx, err)}}))
		return
	}
	u.render(w, r, http.StatusOK, UserPage(UserData{Detail: d}))
}

func (u *UI) banUser(w http.ResponseWriter, r *http.Request) {
	u.userAction(w, r, "pages.user.status.banned", func(ctx context.Context, a services.Actor, id string) error {
		in, err := banInput(r.PostFormValue("banReason"), r.PostFormValue("banExpiresIn"))
		if err != nil {
			return err
		}
		return u.users.BanUser(ctx, a, id, in)
	})
}

func (u *UI) unbanUser(w http.ResponseWriter, r *http.Request) {
	u.userAction(w, r, "pages.user.status.unbanned", func(ctx context.Context, a services.Actor, id string) error {
		return u.users.UnbanUser(ctx, a, id)
	})
}

func (u *UI) setRole(w http.ResponseWriter, r *http.Request) {
	u.userAction(w, r, "pages.user.status.roleSet", func(ctx context.Context, a services.Actor, id string) error {
		in := validate.SetUserRole{Role: r.PostFormValue("role")}
		if err := in.Validate(); err != nil {
			return err
		}
		return u.users.SetRole(ctx, a, id, in)
	})
}

func (u *UI) revokeSessions(w http.ResponseWriter, r *http.Request) {
	u.userAction(w, r, "pages.user.status.sessionsRevoked", func(ctx context.Context, a services.Actor, id string) error {
		return u.users.RevokeSessions(ctx, a, id)
	})
}

func (u *UI) revokeSession(w http.ResponseWriter, r *http.Request) {
	u.userAction(w, r, "pages.user.status.sessionRevoked", func(ctx context.Context, a services.Actor, id string) error {
		return u.users.RevokeSession(ctx, a, id, r.PathValue("sessionId"))
	})
}

// userAction runs one mutation on the user in the path. A Datastar request
// gets the refreshed profile, ban and sessions fragments plus a status alert
// (or just an error alert); a plain post gets a 303 back to the user page, or
// the full page with the error.
func (u *UI) userAction(w http.ResponseWriter, r *http.Request, doneKey string, do func(ctx context.Context, a services.Actor, id string) error) {
	a, ok := u.signedInAction(w, r)
	if !ok {
		return
	}
	ctx := r.Context()
	id := r.PathValue("id")
	err := do(ctx, a, id)
	if u.needsTwoFactor(w, r, err) {
		return
	}
	if err != nil {
		st := Status{Error: true, Message: u.errorMessage(ctx, err)}
		if isDatastar(r) {
			u.patchOnly(w, r, UserStatus(st))
			return
		}
		d, gerr := u.users.GetUser(ctx, a, id)
		u.render(w, r, http.StatusUnprocessableEntity, UserPage(UserData{Detail: d, Missing: gerr != nil, Status: st}))
		return
	}
	if !isDatastar(r) {
		http.Redirect(w, r, config.UserPage(id), http.StatusSeeOther)
		return
	}
	st := Status{Message: i18n.T(ctx, doneKey)}
	d, err := u.users.GetUser(ctx, a, id)
	if err != nil {
		u.log.WarnContext(ctx, "ui: reload user after action", "err", err)
		u.patchOnly(w, r, UserStatus(st))
		return
	}
	u.patchOnly(w, r, UserProfile(d.User), UserBan(d.User), UserSessions(d.User.ID, d.Sessions), UserStatus(st))
}

// banInput reads the ban form: the reason and the expiry select in seconds
// ("0" or empty: permanent).
func banInput(reason, expiresIn string) (validate.BanUser, error) {
	in := validate.BanUser{BanReason: reason}
	if v := strings.TrimSpace(expiresIn); v != "" && v != "0" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			return in, validate.Fields{"banExpiresIn": "must be a whole number"}.Err()
		}
		in.BanExpiresIn = &n
	}
	return in, in.Validate()
}
