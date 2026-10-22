package api

//go:generate go tool mockgen -source=users.go -destination=mocks/users.go -package=mocks

import (
	"context"
	"net/http"

	"github.com/jayleaton/ai-patterns-go/apps/web/internal/config"
	"github.com/jayleaton/ai-patterns-go/apps/web/internal/services"
	"github.com/jayleaton/ai-patterns-go/apps/web/internal/validate"
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
	User    services.User `json:"user"`
	Session Session       `json:"session"`
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
	mux.Handle("GET "+r.Me, Authenticated(u.caller, u.get))
	mux.Handle("PATCH "+r.Me, Authenticated(u.caller, u.updateSelf))
	mux.Handle("PATCH "+r.UserByID, Authenticated(u.caller, u.updateByID))
	mux.Handle("DELETE "+r.Me, Authenticated(u.caller, u.delete))
	mux.Handle("GET "+r.MeData, Authenticated(u.caller, u.dataSummary))
	mux.Handle("POST "+r.MeEmail, Authenticated(u.caller, u.changeEmail))
	mux.Handle("POST "+r.MePassword, Authenticated(u.caller, u.changePassword))
	mux.Handle("GET "+r.MeSessions, Authenticated(u.caller, u.listSessions))
	mux.Handle("DELETE "+r.MeSessionByID, Authenticated(u.caller, u.revokeSession))
	mux.Handle("DELETE "+r.MeOtherSessions, Authenticated(u.caller, u.revokeOtherSessions))
}

func (u *Users) get(r *http.Request, c Caller) (CurrentUser, error) {
	user, err := u.svc.GetProfile(r.Context(), c.Actor(), c.User.ID)
	return CurrentUser{User: user, Session: c.Session}, err
}

func (u *Users) updateSelf(r *http.Request, c Caller) (UserUpdated, error) {
	return u.update(r, c, c.User.ID)
}

func (u *Users) updateByID(r *http.Request, c Caller) (UserUpdated, error) {
	return u.update(r, c, r.PathValue("id"))
}

func (u *Users) update(r *http.Request, c Caller, id string) (UserUpdated, error) {
	in, err := validate.Decode[validate.UpdateUserSettings](r.Body)
	if err != nil {
		return UserUpdated{}, err
	}
	user, err := u.svc.UpdateProfile(r.Context(), c.Actor(), id, in)
	return UserUpdated{Success: err == nil, User: user}, err
}

func (u *Users) delete(r *http.Request, c Caller) (Success, error) {
	err := u.svc.DeleteAccount(r.Context(), c.Actor(), c.User.ID)
	return Success{err == nil}, err
}

func (u *Users) dataSummary(r *http.Request, c Caller) (services.UserDataSummary, error) {
	return u.svc.DataSummary(r.Context(), c.Actor(), c.User.ID)
}

func (u *Users) changeEmail(r *http.Request, c Caller) (Success, error) {
	in, err := validate.Decode[validate.UpdateUserEmail](r.Body)
	if err != nil {
		return Success{}, err
	}
	err = u.svc.RequestEmailChange(r.Context(), c.Actor(), c.User.ID, in)
	return Success{err == nil}, err
}

func (u *Users) changePassword(r *http.Request, c Caller) (Success, error) {
	in, err := validate.Decode[validate.UpdateUserPassword](r.Body)
	if err != nil {
		return Success{}, err
	}
	err = u.svc.ChangePassword(r.Context(), c.Actor(), c.User.ID, in)
	return Success{err == nil}, err
}

func (u *Users) listSessions(r *http.Request, c Caller) ([]services.Session, error) {
	return u.svc.ListSessions(r.Context(), c.Actor(), c.User.ID)
}

func (u *Users) revokeSession(r *http.Request, c Caller) (Success, error) {
	err := u.svc.RevokeSession(r.Context(), c.Actor(), c.User.ID, r.PathValue("id"))
	return Success{err == nil}, err
}

func (u *Users) revokeOtherSessions(r *http.Request, c Caller) (Success, error) {
	err := u.svc.RevokeOtherSessions(r.Context(), c.Actor(), c.User.ID)
	return Success{err == nil}, err
}
