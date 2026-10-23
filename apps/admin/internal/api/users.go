package api

//go:generate go tool mockgen -source=users.go -destination=mocks/users.go -package=mocks

import (
	"context"
	"net/http"

	"github.com/jayleaton/go-fast-stack/apps/admin/internal/access"
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
	svc  UserService
	gate *access.Gate
}

// NewUsers returns the users endpoints.
func NewUsers(svc UserService, gate *access.Gate) *Users {
	return &Users{svc: svc, gate: gate}
}

// Routes registers the endpoints on mux, each with its access policy.
func (users *Users) Routes(mux *http.ServeMux) {
	routes := config.APIRoutes
	mux.Handle("GET "+routes.Users, Route(users.gate, access.SignedIn, users.list))
	mux.Handle("GET "+routes.UserByID, Route(users.gate, access.SignedIn, users.get))
	mux.Handle("POST "+routes.UserBan, Route(users.gate, access.SignedIn, users.ban))
	mux.Handle("POST "+routes.UserUnban, Route(users.gate, access.SignedIn, users.unban))
	mux.Handle("PUT "+routes.UserRole, Route(users.gate, access.SignedIn, users.setRole))
	mux.Handle("DELETE "+routes.UserSessions, Route(users.gate, access.SignedIn, users.revokeSessions))
	mux.Handle("DELETE "+routes.UserSessionByID, Route(users.gate, access.SignedIn, users.revokeSession))
}

func (users *Users) list(request *http.Request, caller access.Caller) (services.UserPage, error) {
	input, err := validate.DecodeQuery[validate.ListUsers](request.URL.Query())
	if err != nil {
		return services.UserPage{}, err
	}
	return users.svc.ListUsers(request.Context(), caller.Actor(), input)
}

func (users *Users) get(request *http.Request, caller access.Caller) (services.UserDetail, error) {
	return users.svc.GetUser(request.Context(), caller.Actor(), request.PathValue("id"))
}

func (users *Users) ban(request *http.Request, caller access.Caller) (Success, error) {
	input, err := validate.Decode[validate.BanUser](request.Body)
	if err != nil {
		return Success{}, err
	}
	err = users.svc.BanUser(request.Context(), caller.Actor(), request.PathValue("id"), input)
	return Success{err == nil}, err
}

func (users *Users) unban(request *http.Request, caller access.Caller) (Success, error) {
	err := users.svc.UnbanUser(request.Context(), caller.Actor(), request.PathValue("id"))
	return Success{err == nil}, err
}

func (users *Users) setRole(request *http.Request, caller access.Caller) (Success, error) {
	input, err := validate.Decode[validate.SetUserRole](request.Body)
	if err != nil {
		return Success{}, err
	}
	err = users.svc.SetRole(request.Context(), caller.Actor(), request.PathValue("id"), input)
	return Success{err == nil}, err
}

func (users *Users) revokeSessions(request *http.Request, caller access.Caller) (Success, error) {
	err := users.svc.RevokeSessions(request.Context(), caller.Actor(), request.PathValue("id"))
	return Success{err == nil}, err
}

func (users *Users) revokeSession(request *http.Request, caller access.Caller) (Success, error) {
	err := users.svc.RevokeSession(request.Context(), caller.Actor(), request.PathValue("id"), request.PathValue("sessionId"))
	return Success{err == nil}, err
}
