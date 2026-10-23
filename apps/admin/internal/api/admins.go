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

// Admins serves the dashboard's admins under /api/admin/v1/admins. Each
// endpoint is declared once, with its access policy; Routes serves it over
// HTTP and Client calls it in-process.
type Admins struct {
	svc  AdminService
	gate *access.Gate

	list           Endpoint[NoInput, []services.Admin]
	invite         Endpoint[validate.CreateAdminInvite, services.Admin]
	disable        Endpoint[NoInput, Success]
	enable         Endpoint[NoInput, Success]
	remove         Endpoint[NoInput, Success]
	passwordReset  Endpoint[NoInput, Success]
	resetTwoFactor Endpoint[NoInput, Success]
}

// NewAdmins returns the admins endpoints.
func NewAdmins(svc AdminService, gate *access.Gate) *Admins {
	admins := &Admins{svc: svc, gate: gate}
	paths := config.APIRoutes
	admins.list = Endpoint[NoInput, []services.Admin]{"GET", paths.Admins, access.SignedIn, admins.listAdmins}
	admins.invite = Endpoint[validate.CreateAdminInvite, services.Admin]{"POST", paths.AdminInvites, access.SignedIn, admins.inviteAdmin}
	admins.disable = Endpoint[NoInput, Success]{"POST", paths.AdminDisable, access.SignedIn, admins.byID(svc.DisableAdmin)}
	admins.enable = Endpoint[NoInput, Success]{"POST", paths.AdminEnable, access.SignedIn, admins.byID(svc.EnableAdmin)}
	admins.remove = Endpoint[NoInput, Success]{"DELETE", paths.AdminByID, access.SignedIn, admins.byID(svc.RemoveAdmin)}
	admins.passwordReset = Endpoint[NoInput, Success]{"POST", paths.AdminPasswordReset, access.SignedIn, admins.byID(svc.SendPasswordReset)}
	admins.resetTwoFactor = Endpoint[NoInput, Success]{"POST", paths.AdminTwoFactorReset, access.SignedIn, admins.byID(svc.ResetTwoFactor)}
	return admins
}

// Routes registers the endpoints on mux.
func (admins *Admins) Routes(mux *http.ServeMux) {
	for _, endpoint := range []Endpoint[NoInput, Success]{admins.disable, admins.enable, admins.remove, admins.passwordReset, admins.resetTwoFactor} {
		mux.Handle(endpoint.Pattern(), endpoint.Serve(admins.gate))
	}
	mux.Handle(admins.list.Pattern(), admins.list.Serve(admins.gate))
	mux.Handle(admins.invite.Pattern(), admins.invite.Serve(admins.gate))
}

func (admins *Admins) listAdmins(request *http.Request, caller access.Caller, _ NoInput) ([]services.Admin, error) {
	return admins.svc.ListAdmins(request.Context(), caller.Actor())
}

func (admins *Admins) inviteAdmin(request *http.Request, caller access.Caller, input validate.CreateAdminInvite) (services.Admin, error) {
	return admins.svc.InviteAdmin(request.Context(), caller.Actor(), input)
}

// byID is an endpoint that runs one service mutation on the admin in the
// path ({id}) and reports the outcome.
func (admins *Admins) byID(mutate func(ctx context.Context, actor services.Actor, adminID string) error) func(*http.Request, access.Caller, NoInput) (Success, error) {
	return func(request *http.Request, caller access.Caller, _ NoInput) (Success, error) {
		err := mutate(request.Context(), caller.Actor(), request.PathValue("id"))
		return Success{err == nil}, err
	}
}
