package api

import (
	"net/http"

	"github.com/jayleaton/ai-patterns-go/apps/web/internal/config"
	"github.com/jayleaton/ai-patterns-go/apps/web/internal/services"
	"github.com/jayleaton/ai-patterns-go/apps/web/internal/validate"
)

// CurrentUser is the GET /users response.
type CurrentUser struct {
	User    services.User `json:"user"`
	Session Session       `json:"session"`
}

// UserUpdated is the PATCH /users response.
type UserUpdated struct {
	Success bool          `json:"success"`
	User    services.User `json:"user"`
}

// UserDeleted is the DELETE /users response.
type UserDeleted struct {
	Success bool `json:"success"`
}

// Users serves /api/core/v1/users.
type Users struct {
	svc    *services.UserService
	caller CallerFrom
}

// NewUsers returns the users endpoints.
func NewUsers(svc *services.UserService, caller CallerFrom) *Users {
	return &Users{svc: svc, caller: caller}
}

// Routes registers the endpoints on mux.
func (u *Users) Routes(mux *http.ServeMux) {
	mux.Handle("GET "+config.APIRoutes.Users, Authenticated(u.caller, u.get))
	mux.Handle("PATCH "+config.APIRoutes.Users, Authenticated(u.caller, u.updateSelf))
	mux.Handle("PATCH "+config.APIRoutes.UserByID, Authenticated(u.caller, u.updateByID))
	mux.Handle("DELETE "+config.APIRoutes.Users, Authenticated(u.caller, u.delete))
}

func (u *Users) get(_ *http.Request, c Caller) (CurrentUser, error) {
	return CurrentUser(c), nil
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
	user, err := u.svc.UpdateUserSettings(r.Context(), c.Actor(), id, in)
	return UserUpdated{Success: err == nil, User: user}, err
}

func (u *Users) delete(r *http.Request, c Caller) (UserDeleted, error) {
	err := u.svc.DeleteAllUserData(r.Context(), c.Actor())
	return UserDeleted{Success: err == nil}, err
}
