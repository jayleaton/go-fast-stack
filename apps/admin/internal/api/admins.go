package api

//go:generate go tool mockgen -source=admins.go -destination=mocks/admins.go -package=mocks

import (
	"context"
	"net/http"

	"github.com/jayleaton/go-fast-stack/apps/admin/internal/access"
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
	SendPasswordReset(ctx context.Context, actor services.Actor, adminID string) error
	ResetTwoFactor(ctx context.Context, actor services.Actor, adminID string) error
}

var _ AdminService = (*services.AdminService)(nil)

// Admins serves the dashboard's admins under /api/admin/v1/admins.
type Admins struct {
	svc  AdminService
	gate *access.Gate
}

// NewAdmins returns the admins endpoints.
func NewAdmins(svc AdminService, gate *access.Gate) *Admins {
	return &Admins{svc: svc, gate: gate}
}

// Routes registers the endpoints on mux, each with its access policy.
func (endpoints *Admins) Routes(mux *http.ServeMux) {
	paths := config.APIRoutes
	mux.Handle("GET "+paths.Admins, Route(endpoints.gate, access.SignedIn, endpoints.list))
	mux.Handle("POST "+paths.AdminInvites, Route(endpoints.gate, access.SignedIn, endpoints.invite))
	mux.Handle("POST "+paths.AdminDisable, Route(endpoints.gate, access.SignedIn, endpoints.disable))
	mux.Handle("POST "+paths.AdminEnable, Route(endpoints.gate, access.SignedIn, endpoints.enable))
	mux.Handle("DELETE "+paths.AdminByID, Route(endpoints.gate, access.SignedIn, endpoints.remove))
	mux.Handle("POST "+paths.AdminPasswordReset, Route(endpoints.gate, access.SignedIn, endpoints.passwordReset))
	mux.Handle("POST "+paths.AdminTwoFactorReset, Route(endpoints.gate, access.SignedIn, endpoints.resetTwoFactor))
}

func (endpoints *Admins) list(request *http.Request, caller access.Caller) ([]services.Admin, error) {
	return endpoints.svc.ListAdmins(request.Context(), caller.Actor())
}

func (endpoints *Admins) invite(request *http.Request, caller access.Caller) (services.Admin, error) {
	in, err := validate.Decode[validate.CreateAdminInvite](request.Body)
	if err != nil {
		return services.Admin{}, err
	}
	return endpoints.svc.InviteAdmin(request.Context(), caller.Actor(), in)
}

func (endpoints *Admins) disable(request *http.Request, caller access.Caller) (Success, error) {
	err := endpoints.svc.DisableAdmin(request.Context(), caller.Actor(), request.PathValue("id"))
	return Success{err == nil}, err
}

func (endpoints *Admins) enable(request *http.Request, caller access.Caller) (Success, error) {
	err := endpoints.svc.EnableAdmin(request.Context(), caller.Actor(), request.PathValue("id"))
	return Success{err == nil}, err
}

func (endpoints *Admins) remove(request *http.Request, caller access.Caller) (Success, error) {
	err := endpoints.svc.RemoveAdmin(request.Context(), caller.Actor(), request.PathValue("id"))
	return Success{err == nil}, err
}

func (endpoints *Admins) passwordReset(request *http.Request, caller access.Caller) (Success, error) {
	err := endpoints.svc.SendPasswordReset(request.Context(), caller.Actor(), request.PathValue("id"))
	return Success{err == nil}, err
}

func (endpoints *Admins) resetTwoFactor(request *http.Request, caller access.Caller) (Success, error) {
	err := endpoints.svc.ResetTwoFactor(request.Context(), caller.Actor(), request.PathValue("id"))
	return Success{err == nil}, err
}
