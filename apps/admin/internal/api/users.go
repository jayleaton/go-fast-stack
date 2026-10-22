package api

//go:generate go tool mockgen -source=users.go -destination=mocks/users.go -package=mocks

import (
	"context"
	"net/http"

	"github.com/jayleaton/go-fast-stack/apps/admin/internal/config"
	"github.com/jayleaton/go-fast-stack/apps/admin/internal/services"
	"github.com/jayleaton/go-fast-stack/apps/admin/internal/validate"
)

// UserService is the port these endpoints call (*services.UserService).
// Handler tests use its generated mock in api/mocks.
type UserService interface {
	ListUsers(ctx context.Context, actor services.Actor, in validate.ListUsers) (services.UserPage, error)
	GetUser(ctx context.Context, actor services.Actor, userID string) (services.UserDetail, error)
	BanUser(ctx context.Context, actor services.Actor, userID string, in validate.BanUser) error
	UnbanUser(ctx context.Context, actor services.Actor, userID string) error
	SetRole(ctx context.Context, actor services.Actor, userID string, in validate.SetUserRole) error
	RevokeSession(ctx context.Context, actor services.Actor, userID, sessionID string) error
	RevokeSessions(ctx context.Context, actor services.Actor, userID string) error
}

var _ UserService = (*services.UserService)(nil)

// Success is the response of endpoints that only report the outcome.
type Success struct {
	Success bool `json:"success"`
}

// Users serves the main app's users under /api/admin/v1/users.
type Users struct {
	svc    UserService
	caller CallerFrom
}

// NewUsers returns the users endpoints.
func NewUsers(svc UserService, caller CallerFrom) *Users {
	return &Users{svc: svc, caller: caller}
}

// Routes registers the endpoints on mux.
func (u *Users) Routes(mux *http.ServeMux) {
	r := config.APIRoutes
	mux.Handle("GET "+r.Users, Authenticated(u.caller, u.list))
	mux.Handle("GET "+r.UserByID, Authenticated(u.caller, u.get))
	mux.Handle("POST "+r.UserBan, Authenticated(u.caller, u.ban))
	mux.Handle("POST "+r.UserUnban, Authenticated(u.caller, u.unban))
	mux.Handle("PUT "+r.UserRole, Authenticated(u.caller, u.setRole))
	mux.Handle("DELETE "+r.UserSessions, Authenticated(u.caller, u.revokeSessions))
	mux.Handle("DELETE "+r.UserSessionByID, Authenticated(u.caller, u.revokeSession))
}

func (u *Users) list(r *http.Request, c Caller) (services.UserPage, error) {
	in, err := validate.DecodeQuery[validate.ListUsers](r.URL.Query())
	if err != nil {
		return services.UserPage{}, err
	}
	return u.svc.ListUsers(r.Context(), c.Actor(), in)
}

func (u *Users) get(r *http.Request, c Caller) (services.UserDetail, error) {
	return u.svc.GetUser(r.Context(), c.Actor(), r.PathValue("id"))
}

func (u *Users) ban(r *http.Request, c Caller) (Success, error) {
	in, err := validate.Decode[validate.BanUser](r.Body)
	if err != nil {
		return Success{}, err
	}
	err = u.svc.BanUser(r.Context(), c.Actor(), r.PathValue("id"), in)
	return Success{err == nil}, err
}

func (u *Users) unban(r *http.Request, c Caller) (Success, error) {
	err := u.svc.UnbanUser(r.Context(), c.Actor(), r.PathValue("id"))
	return Success{err == nil}, err
}

func (u *Users) setRole(r *http.Request, c Caller) (Success, error) {
	in, err := validate.Decode[validate.SetUserRole](r.Body)
	if err != nil {
		return Success{}, err
	}
	err = u.svc.SetRole(r.Context(), c.Actor(), r.PathValue("id"), in)
	return Success{err == nil}, err
}

func (u *Users) revokeSessions(r *http.Request, c Caller) (Success, error) {
	err := u.svc.RevokeSessions(r.Context(), c.Actor(), r.PathValue("id"))
	return Success{err == nil}, err
}

func (u *Users) revokeSession(r *http.Request, c Caller) (Success, error) {
	err := u.svc.RevokeSession(r.Context(), c.Actor(), r.PathValue("id"), r.PathValue("sessionId"))
	return Success{err == nil}, err
}
