package api

//go:generate go tool mockgen -source=users.go -destination=mocks/users.go -package=mocks

import (
	"context"
	"net/http"

	"github.com/jayleaton/go-fast-stack/apps/web/internal/access"
	"github.com/jayleaton/go-fast-stack/apps/web/internal/apiclient"
	"github.com/jayleaton/go-fast-stack/apps/web/internal/config"
	"github.com/jayleaton/go-fast-stack/apps/web/internal/services"
	"github.com/jayleaton/go-fast-stack/apps/web/internal/validate"
)

// UserService is the port these endpoints call (*services.UserService).
// Handler tests use its generated mock in api/mocks.
type UserService interface {
	GetProfile(ctx context.Context, actor services.Actor, userID string) (services.User, error)
	UpdateProfile(ctx context.Context, actor services.Actor, userID string, in validate.UpdateUserSettings) (services.User, error)
	RequestEmailChange(ctx context.Context, actor services.Actor, userID string, in validate.UpdateUserEmail) error
	ChangePassword(ctx context.Context, actor services.Actor, userID string, in validate.UpdateUserPassword) error
	ListSessions(ctx context.Context, actor services.Actor, userID string) ([]services.Session, error)
	RevokeSession(ctx context.Context, actor services.Actor, userID, sessionID string) error
	RevokeOtherSessions(ctx context.Context, actor services.Actor, userID string) error
	DeleteAccount(ctx context.Context, actor services.Actor, userID string) error
	DataSummary(ctx context.Context, actor services.Actor, userID string) (services.UserDataSummary, error)
}

var _ UserService = (*services.UserService)(nil)

// The responses are the client's types (apiclient), so the UI and the API
// share one definition.
type (
	// CurrentUser is the GET /users/me response.
	CurrentUser = apiclient.CurrentUser
	// UserUpdated is the PATCH /users/me response.
	UserUpdated = apiclient.UserUpdated
)

// Success is the response of endpoints that only report the outcome.
type Success struct {
	Success bool `json:"success"`
}

// Users serves the signed-in user's account under /api/core/v1/users. Each
// endpoint is declared once, with its access policy; Routes serves it over
// HTTP and Client calls it in-process.
type Users struct {
	svc  UserService
	gate *access.Gate

	me                  Endpoint[NoInput, CurrentUser]
	updateSelf          Endpoint[validate.UpdateUserSettings, UserUpdated]
	updateByID          Endpoint[validate.UpdateUserSettings, UserUpdated]
	delete              Endpoint[NoInput, Success]
	dataSummary         Endpoint[NoInput, services.UserDataSummary]
	changeEmail         Endpoint[validate.UpdateUserEmail, Success]
	changePassword      Endpoint[validate.UpdateUserPassword, Success]
	listSessions        Endpoint[NoInput, []services.Session]
	revokeSession       Endpoint[NoInput, Success]
	revokeOtherSessions Endpoint[NoInput, Success]
}

// NewUsers returns the users endpoints.
func NewUsers(svc UserService, gate *access.Gate) *Users {
	users := &Users{svc: svc, gate: gate}
	routes := config.APIRoutes
	users.me = Endpoint[NoInput, CurrentUser]{"GET", routes.Me, access.SignedIn, users.get}
	users.updateSelf = Endpoint[validate.UpdateUserSettings, UserUpdated]{"PATCH", routes.Me, access.SignedIn, users.updateMine}
	users.updateByID = Endpoint[validate.UpdateUserSettings, UserUpdated]{"PATCH", routes.UserByID, access.SignedIn, users.updateUser}
	users.delete = Endpoint[NoInput, Success]{"DELETE", routes.Me, access.SignedIn, users.deleteMine}
	users.dataSummary = Endpoint[NoInput, services.UserDataSummary]{"GET", routes.MeData, access.SignedIn, users.summary}
	users.changeEmail = Endpoint[validate.UpdateUserEmail, Success]{"POST", routes.MeEmail, access.SignedIn, users.requestEmailChange}
	users.changePassword = Endpoint[validate.UpdateUserPassword, Success]{"POST", routes.MePassword, access.SignedIn, users.updatePassword}
	users.listSessions = Endpoint[NoInput, []services.Session]{"GET", routes.MeSessions, access.SignedIn, users.sessions}
	users.revokeSession = Endpoint[NoInput, Success]{"DELETE", routes.MeSessionByID, access.SignedIn, users.revoke}
	users.revokeOtherSessions = Endpoint[NoInput, Success]{"DELETE", routes.MeOtherSessions, access.SignedIn, users.revokeOthers}
	return users
}

// Routes registers the endpoints on mux.
func (users *Users) Routes(mux *http.ServeMux) {
	mux.Handle(users.me.Pattern(), users.me.Serve(users.gate))
	mux.Handle(users.updateSelf.Pattern(), users.updateSelf.Serve(users.gate))
	mux.Handle(users.updateByID.Pattern(), users.updateByID.Serve(users.gate))
	mux.Handle(users.delete.Pattern(), users.delete.Serve(users.gate))
	mux.Handle(users.dataSummary.Pattern(), users.dataSummary.Serve(users.gate))
	mux.Handle(users.changeEmail.Pattern(), users.changeEmail.Serve(users.gate))
	mux.Handle(users.changePassword.Pattern(), users.changePassword.Serve(users.gate))
	mux.Handle(users.listSessions.Pattern(), users.listSessions.Serve(users.gate))
	mux.Handle(users.revokeSession.Pattern(), users.revokeSession.Serve(users.gate))
	mux.Handle(users.revokeOtherSessions.Pattern(), users.revokeOtherSessions.Serve(users.gate))
}

func (users *Users) get(request *http.Request, caller access.Caller, _ NoInput) (CurrentUser, error) {
	user, err := users.svc.GetProfile(request.Context(), caller.Actor(), caller.User.ID)
	return CurrentUser{User: user, Session: caller.Session}, err
}

func (users *Users) updateMine(request *http.Request, caller access.Caller, input validate.UpdateUserSettings) (UserUpdated, error) {
	return users.update(request, caller, caller.User.ID, input)
}

func (users *Users) updateUser(request *http.Request, caller access.Caller, input validate.UpdateUserSettings) (UserUpdated, error) {
	return users.update(request, caller, request.PathValue("id"), input)
}

func (users *Users) update(request *http.Request, caller access.Caller, userID string, input validate.UpdateUserSettings) (UserUpdated, error) {
	user, err := users.svc.UpdateProfile(request.Context(), caller.Actor(), userID, input)
	return UserUpdated{Success: err == nil, User: user}, err
}

func (users *Users) deleteMine(request *http.Request, caller access.Caller, _ NoInput) (Success, error) {
	err := users.svc.DeleteAccount(request.Context(), caller.Actor(), caller.User.ID)
	return Success{err == nil}, err
}

func (users *Users) summary(request *http.Request, caller access.Caller, _ NoInput) (services.UserDataSummary, error) {
	return users.svc.DataSummary(request.Context(), caller.Actor(), caller.User.ID)
}

func (users *Users) requestEmailChange(request *http.Request, caller access.Caller, input validate.UpdateUserEmail) (Success, error) {
	err := users.svc.RequestEmailChange(request.Context(), caller.Actor(), caller.User.ID, input)
	return Success{err == nil}, err
}

func (users *Users) updatePassword(request *http.Request, caller access.Caller, input validate.UpdateUserPassword) (Success, error) {
	err := users.svc.ChangePassword(request.Context(), caller.Actor(), caller.User.ID, input)
	return Success{err == nil}, err
}

func (users *Users) sessions(request *http.Request, caller access.Caller, _ NoInput) ([]services.Session, error) {
	return users.svc.ListSessions(request.Context(), caller.Actor(), caller.User.ID)
}

func (users *Users) revoke(request *http.Request, caller access.Caller, _ NoInput) (Success, error) {
	err := users.svc.RevokeSession(request.Context(), caller.Actor(), caller.User.ID, request.PathValue("id"))
	return Success{err == nil}, err
}

func (users *Users) revokeOthers(request *http.Request, caller access.Caller, _ NoInput) (Success, error) {
	err := users.svc.RevokeOtherSessions(request.Context(), caller.Actor(), caller.User.ID)
	return Success{err == nil}, err
}
