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

// Users serves the main app's users under /api/admin/v1/users. Each
// endpoint is declared once, with its access policy; Routes serves it over
// HTTP and Client calls it in-process.
type Users struct {
	svc  UserService
	gate *access.Gate

	list           Endpoint[validate.ListUsers, services.UserPage]
	get            Endpoint[NoInput, services.UserDetail]
	ban            Endpoint[validate.BanUser, Success]
	unban          Endpoint[NoInput, Success]
	setRole        Endpoint[validate.SetUserRole, Success]
	revokeSessions Endpoint[NoInput, Success]
	revokeSession  Endpoint[NoInput, Success]
}

// NewUsers returns the users endpoints.
func NewUsers(svc UserService, gate *access.Gate) *Users {
	users := &Users{svc: svc, gate: gate}
	routes := config.APIRoutes
	users.list = Endpoint[validate.ListUsers, services.UserPage]{"GET", routes.Users, access.SignedIn, users.listUsers}
	users.get = Endpoint[NoInput, services.UserDetail]{"GET", routes.UserByID, access.SignedIn, users.getUser}
	users.ban = Endpoint[validate.BanUser, Success]{"POST", routes.UserBan, access.SignedIn, users.banUser}
	users.unban = Endpoint[NoInput, Success]{"POST", routes.UserUnban, access.SignedIn, users.unbanUser}
	users.setRole = Endpoint[validate.SetUserRole, Success]{"PUT", routes.UserRole, access.SignedIn, users.setUserRole}
	users.revokeSessions = Endpoint[NoInput, Success]{"DELETE", routes.UserSessions, access.SignedIn, users.revokeAll}
	users.revokeSession = Endpoint[NoInput, Success]{"DELETE", routes.UserSessionByID, access.SignedIn, users.revokeOne}
	return users
}

// Routes registers the endpoints on mux.
func (users *Users) Routes(mux *http.ServeMux) {
	mux.Handle(users.list.Pattern(), users.list.Serve(users.gate))
	mux.Handle(users.get.Pattern(), users.get.Serve(users.gate))
	mux.Handle(users.ban.Pattern(), users.ban.Serve(users.gate))
	mux.Handle(users.unban.Pattern(), users.unban.Serve(users.gate))
	mux.Handle(users.setRole.Pattern(), users.setRole.Serve(users.gate))
	mux.Handle(users.revokeSessions.Pattern(), users.revokeSessions.Serve(users.gate))
	mux.Handle(users.revokeSession.Pattern(), users.revokeSession.Serve(users.gate))
}

func (users *Users) listUsers(request *http.Request, caller access.Caller, input validate.ListUsers) (services.UserPage, error) {
	return users.svc.ListUsers(request.Context(), caller.Actor(), input)
}

func (users *Users) getUser(request *http.Request, caller access.Caller, _ NoInput) (services.UserDetail, error) {
	return users.svc.GetUser(request.Context(), caller.Actor(), request.PathValue("id"))
}

func (users *Users) banUser(request *http.Request, caller access.Caller, input validate.BanUser) (Success, error) {
	err := users.svc.BanUser(request.Context(), caller.Actor(), request.PathValue("id"), input)
	return Success{err == nil}, err
}

func (users *Users) unbanUser(request *http.Request, caller access.Caller, _ NoInput) (Success, error) {
	err := users.svc.UnbanUser(request.Context(), caller.Actor(), request.PathValue("id"))
	return Success{err == nil}, err
}

func (users *Users) setUserRole(request *http.Request, caller access.Caller, input validate.SetUserRole) (Success, error) {
	err := users.svc.SetRole(request.Context(), caller.Actor(), request.PathValue("id"), input)
	return Success{err == nil}, err
}

func (users *Users) revokeAll(request *http.Request, caller access.Caller, _ NoInput) (Success, error) {
	err := users.svc.RevokeSessions(request.Context(), caller.Actor(), request.PathValue("id"))
	return Success{err == nil}, err
}

func (users *Users) revokeOne(request *http.Request, caller access.Caller, _ NoInput) (Success, error) {
	err := users.svc.RevokeSession(request.Context(), caller.Actor(), request.PathValue("id"), request.PathValue("sessionId"))
	return Success{err == nil}, err
}
