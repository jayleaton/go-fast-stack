package api

//go:generate go tool mockgen -source=admins.go -destination=mocks/admins.go -package=mocks

import (
	"context"
	"net/http"

	"github.com/jayleaton/ai-patterns-go/apps/admin/internal/config"
	"github.com/jayleaton/ai-patterns-go/apps/admin/internal/services"
	"github.com/jayleaton/ai-patterns-go/apps/admin/internal/validate"
)

// AdminService is the port these endpoints call (*services.AdminService).
// Bootstrap is not on it: only the operator's CLI seeds the first admin.
type AdminService interface {
	ListAdmins(ctx context.Context, actor services.Actor) ([]services.Admin, error)
	InviteAdmin(ctx context.Context, actor services.Actor, in validate.CreateAdminInvite) (services.Admin, error)
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
