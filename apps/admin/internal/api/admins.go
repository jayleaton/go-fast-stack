// ci timing probe
package api

//go:generate go tool mockgen -source=admins.go -destination=mocks/admins.go -package=mocks

import (
	"context"
	"net/http"

	"github.com/jayleaton/go-fast-stack/apps/admin/internal/config"
	"github.com/jayleaton/go-fast-stack/apps/admin/internal/services"
	"github.com/jayleaton/go-fast-stack/apps/admin/internal/validate"
)

// AdminService is the port these endpoints call (*services.AdminService).
// Bootstrap is not on it: only the operator's CLI seeds the first admin.
type AdminService interface {
	ListAdmins(ctx context.Context, actor services.Actor) ([]services.Admin, error)
	InviteAdmin(ctx context.Context, actor services.Actor, in validate.CreateAdminInvite) (services.Admin, error)
	DisableAdmin(ctx context.Context, actor services.Actor, adminID string) error
	EnableAdmin(ctx context.Context, actor services.Actor, adminID string) error
	RemoveAdmin(ctx context.Context, actor services.Actor, adminID string) error
}

var _ AdminService = (*services.AdminService)(nil)

// Admins serves the dashboard's admins under /api/admin/v1/admins.
type Admins struct {
	svc    AdminService
	caller CallerFrom
}

// NewAdmins returns the admins endpoints.
func NewAdmins(svc AdminService, caller CallerFrom) *Admins {
	return &Admins{svc: svc, caller: caller}
}

// Routes registers the endpoints on mux.
func (a *Admins) Routes(mux *http.ServeMux) {
	r := config.APIRoutes
	mux.Handle("GET "+r.Admins, Authenticated(a.caller, a.list))
	mux.Handle("POST "+r.AdminInvites, Authenticated(a.caller, a.invite))
	mux.Handle("POST "+r.AdminDisable, Authenticated(a.caller, a.disable))
	mux.Handle("POST "+r.AdminEnable, Authenticated(a.caller, a.enable))
	mux.Handle("DELETE "+r.AdminByID, Authenticated(a.caller, a.remove))
}

func (a *Admins) list(r *http.Request, c Caller) ([]services.Admin, error) {
	return a.svc.ListAdmins(r.Context(), c.Actor())
}

func (a *Admins) invite(r *http.Request, c Caller) (services.Admin, error) {
	in, err := validate.Decode[validate.CreateAdminInvite](r.Body)
	if err != nil {
		return services.Admin{}, err
	}
	return a.svc.InviteAdmin(r.Context(), c.Actor(), in)
}

func (a *Admins) disable(r *http.Request, c Caller) (Success, error) {
	err := a.svc.DisableAdmin(r.Context(), c.Actor(), r.PathValue("id"))
	return Success{err == nil}, err
}

func (a *Admins) enable(r *http.Request, c Caller) (Success, error) {
	err := a.svc.EnableAdmin(r.Context(), c.Actor(), r.PathValue("id"))
	return Success{err == nil}, err
}

func (a *Admins) remove(r *http.Request, c Caller) (Success, error) {
	err := a.svc.RemoveAdmin(r.Context(), c.Actor(), r.PathValue("id"))
	return Success{err == nil}, err
}
