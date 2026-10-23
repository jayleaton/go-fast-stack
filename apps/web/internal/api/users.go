package api

//go:generate go tool mockgen -source=users.go -destination=mocks/users.go -package=mocks

import (
	"context"
	"net/http"

	"github.com/jayleaton/go-fast-stack/apps/web/internal/access"
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

// CurrentUser is the GET /users/me response.
type CurrentUser struct {
	User    services.User  `json:"user"`
	Session access.Session `json:"session"`
}

// UserUpdated is the PATCH /users/me response.
type UserUpdated struct {
	Success bool          `json:"success"`
	User    services.User `json:"user"`
}

// Success is the response of endpoints that only report the outcome.
type Success struct {
	Success bool `json:"success"`
}

// Users serves the signed-in user's account under /api/core/v1/users.
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
	mux.Handle("GET "+routes.Me, Route(users.gate, access.SignedIn, users.get))
	mux.Handle("PATCH "+routes.Me, Route(users.gate, access.SignedIn, users.updateSelf))
	mux.Handle("PATCH "+routes.UserByID, Route(users.gate, access.SignedIn, users.updateByID))
	mux.Handle("DELETE "+routes.Me, Route(users.gate, access.SignedIn, users.delete))
	mux.Handle("GET "+routes.MeData, Route(users.gate, access.SignedIn, users.dataSummary))
	mux.Handle("POST "+routes.MeEmail, Route(users.gate, access.SignedIn, users.changeEmail))
	mux.Handle("POST "+routes.MePassword, Route(users.gate, access.SignedIn, users.changePassword))
	mux.Handle("GET "+routes.MeSessions, Route(users.gate, access.SignedIn, users.listSessions))
	mux.Handle("DELETE "+routes.MeSessionByID, Route(users.gate, access.SignedIn, users.revokeSession))
	mux.Handle("DELETE "+routes.MeOtherSessions, Route(users.gate, access.SignedIn, users.revokeOtherSessions))
}

func (users *Users) get(request *http.Request, caller access.Caller) (CurrentUser, error) {
	user, err := users.svc.GetProfile(request.Context(), caller.Actor(), caller.User.ID)
	return CurrentUser{User: user, Session: caller.Session}, err
}

func (users *Users) updateSelf(request *http.Request, caller access.Caller) (UserUpdated, error) {
	return users.update(request, caller, caller.User.ID)
}

func (users *Users) updateByID(request *http.Request, caller access.Caller) (UserUpdated, error) {
	return users.update(request, caller, request.PathValue("id"))
}

func (users *Users) update(request *http.Request, caller access.Caller, userID string) (UserUpdated, error) {
	input, err := validate.Decode[validate.UpdateUserSettings](request.Body)
	if err != nil {
		return UserUpdated{}, err
	}
	user, err := users.svc.UpdateProfile(request.Context(), caller.Actor(), userID, input)
	return UserUpdated{Success: err == nil, User: user}, err
}

func (users *Users) delete(request *http.Request, caller access.Caller) (Success, error) {
	err := users.svc.DeleteAccount(request.Context(), caller.Actor(), caller.User.ID)
	return Success{err == nil}, err
}

func (users *Users) dataSummary(request *http.Request, caller access.Caller) (services.UserDataSummary, error) {
	return users.svc.DataSummary(request.Context(), caller.Actor(), caller.User.ID)
}

func (users *Users) changeEmail(request *http.Request, caller access.Caller) (Success, error) {
	input, err := validate.Decode[validate.UpdateUserEmail](request.Body)
	if err != nil {
		return Success{}, err
	}
	err = users.svc.RequestEmailChange(request.Context(), caller.Actor(), caller.User.ID, input)
	return Success{err == nil}, err
}

func (users *Users) changePassword(request *http.Request, caller access.Caller) (Success, error) {
	input, err := validate.Decode[validate.UpdateUserPassword](request.Body)
	if err != nil {
		return Success{}, err
	}
	err = users.svc.ChangePassword(request.Context(), caller.Actor(), caller.User.ID, input)
	return Success{err == nil}, err
}

func (users *Users) listSessions(request *http.Request, caller access.Caller) ([]services.Session, error) {
	return users.svc.ListSessions(request.Context(), caller.Actor(), caller.User.ID)
}

func (users *Users) revokeSession(request *http.Request, caller access.Caller) (Success, error) {
	err := users.svc.RevokeSession(request.Context(), caller.Actor(), caller.User.ID, request.PathValue("id"))
	return Success{err == nil}, err
}

func (users *Users) revokeOtherSessions(request *http.Request, caller access.Caller) (Success, error) {
	err := users.svc.RevokeOtherSessions(request.Context(), caller.Actor(), caller.User.ID)
	return Success{err == nil}, err
}
