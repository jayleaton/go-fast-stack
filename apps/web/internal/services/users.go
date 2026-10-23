package services

//go:generate go tool mockgen -source=users.go -destination=mocks/users.go -package=mocks

import (
	"context"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jayleaton/go-fast-stack/apps/web/internal/validate"
	"github.com/jayleaton/go-fast-stack/packages/database/model"
)

// User and UserDataSummary are the repository models, re-exported so
// transports never import packages/database.
type (
	User              = model.User
	UserProfileUpdate = model.UserProfileUpdate
	UserDataSummary   = model.UserDataSummary
)

// Session is one of a user's sign-ins as the account pages show it. It never
// carries the session token.
type Session struct {
	ID        string    `json:"id"`
	CreatedAt time.Time `json:"createdAt"`
	ExpiresAt time.Time `json:"expiresAt"`
	IPAddress *string   `json:"ipAddress"`
	UserAgent *string   `json:"userAgent"`
	Current   bool      `json:"current"` // the session of the request that listed it
}

// UserRepository is the port to the user's data (database.UserRepository).
// Implementations return model.ErrNotFound for a missing user.
type UserRepository interface {
	GetByID(ctx context.Context, id string) (User, error)
	UpdateProfile(ctx context.Context, id string, update UserProfileUpdate) (User, error)
	Delete(ctx context.Context, id string) error
	DataSummary(ctx context.Context, id string) (UserDataSummary, error)
}

// AccountCredentials is the port to the user's credentials and sessions (an
// adapter over *auth.Auth in apps/web/internal/server). Implementations
// report the failures listed per method as the Err* values of this file
// (wrapping is fine), and ErrRateLimited from any method; any other error is
// an internal error.
type AccountCredentials interface {
	// ChangePassword fails with ErrInvalidPassword, ErrPasswordPolicy or
	// ErrNoPassword (an account without one, e.g. OAuth only).
	ChangePassword(ctx context.Context, userID, current, next string, revokeOthers bool, currentSessionID string) error
	// ChangeEmail sends a verification link to newEmail; the address changes
	// only when the link is opened. Fails with ErrSameEmail, ErrEmailTaken,
	// ErrInvalidCallbackURL, ErrSessionNotFresh or ErrUserNotFound.
	ChangeEmail(ctx context.Context, userID, newEmail, callbackURL string) error
	ListSessions(ctx context.Context, userID string) ([]Session, error)
	// RevokeSession fails with ErrSessionNotFound unless the session is the user's.
	RevokeSession(ctx context.Context, userID, sessionID string) error
	RevokeOtherSessions(ctx context.Context, userID, currentSessionID string) error
}

// Password length policy, counted in characters. Matches packages/auth's
// defaults; auth enforces its configured values again (ErrPasswordPolicy).
const (
	MinPasswordLength = 8
	MaxPasswordLength = 128
)

// UserService errors. Match them with errors.Is (the UI picks a translation
// per error); the transport maps their Kind to a status.
var (
	ErrUserNotFound       = &Error{KindNotFound, "User not found"}
	ErrSameEmail          = &Error{KindInvalid, "New email must be different from the current email"}
	ErrEmailTaken         = &Error{KindConflict, "Email is already in use"}
	ErrInvalidCallbackURL = &Error{KindInvalid, "Invalid callback URL"}
	// ErrSessionNotFresh: changing the email needs a recent sign-in.
	ErrSessionNotFresh = &Error{KindForbidden, "Sign in again to change your email"}
	// ErrInvalidPassword is Invalid, not Unauthorized: clients treat 401 as "signed out".
	ErrInvalidPassword = &Error{KindInvalid, "Current password is incorrect"}
	ErrSamePassword    = &Error{KindInvalid, "New password must be different from the current password"}
	ErrPasswordPolicy  = &Error{KindInvalid, fmt.Sprintf("Password must be %d-%d characters", MinPasswordLength, MaxPasswordLength)}
	ErrNoPassword      = &Error{KindInvalid, "This account has no password"}
	ErrSessionNotFound = &Error{KindNotFound, "Session not found"}
	ErrCurrentSession  = &Error{KindInvalid, "Use sign out to end the current session"}
)

// self is the authorization rule of every UserService method: users manage
// only their own account.
func self(actor Actor, userID string) error {
	if actor.UserID == "" || actor.UserID != userID {
		return ErrForbidden
	}
	return nil
}

// UserService is the signed-in user's account: profile, email, password,
// sessions and deletion. Every method names the user it acts on and fails
// with KindForbidden unless that is the actor.
type UserService struct {
	repo  UserRepository
	creds AccountCredentials
}

// NewUserService returns a UserService over its ports.
func NewUserService(repo UserRepository, creds AccountCredentials) *UserService {
	return &UserService{repo: repo, creds: creds}
}

// GetProfile returns the user.
func (service *UserService) GetProfile(ctx context.Context, actor Actor, userID string) (User, error) {
	if err := self(actor, userID); err != nil {
		return User{}, err
	}
	user, err := service.repo.GetByID(ctx, userID)
	return user, notFound(err, ErrUserNotFound)
}

// UpdateProfile changes the name and image.
func (service *UserService) UpdateProfile(ctx context.Context, actor Actor, userID string, in validate.UpdateUserSettings) (User, error) {
	if err := self(actor, userID); err != nil {
		return User{}, err
	}
	user, err := service.repo.UpdateProfile(ctx, userID, UserProfileUpdate{
		Name: in.Name, SetImage: in.Image.Set, Image: in.Image.Value,
	})
	return user, notFound(err, ErrUserNotFound)
}

// RequestEmailChange sends a verification link to the new address.
func (service *UserService) RequestEmailChange(ctx context.Context, actor Actor, userID string, in validate.UpdateUserEmail) error {
	if err := self(actor, userID); err != nil {
		return err
	}
	if strings.EqualFold(in.NewEmail, actor.Email) {
		return ErrSameEmail
	}
	return service.creds.ChangeEmail(ctx, userID, in.NewEmail, in.CallbackURL)
}

// ChangePassword replaces the password after checking the current one, and
// optionally signs out every other session.
func (service *UserService) ChangePassword(ctx context.Context, actor Actor, userID string, in validate.UpdateUserPassword) error {
	if err := self(actor, userID); err != nil {
		return err
	}
	if count := utf8.RuneCountInString(in.NewPassword); count < MinPasswordLength || count > MaxPasswordLength {
		return ErrPasswordPolicy
	}
	if in.NewPassword == in.CurrentPassword {
		return ErrSamePassword
	}
	return service.creds.ChangePassword(ctx, userID, in.CurrentPassword, in.NewPassword, in.RevokeOtherSessions, actor.SessionID)
}

// ListSessions returns the user's sessions, marking the actor's own.
func (service *UserService) ListSessions(ctx context.Context, actor Actor, userID string) ([]Session, error) {
	if err := self(actor, userID); err != nil {
		return nil, err
	}
	sessions, err := service.creds.ListSessions(ctx, userID)
	if err != nil {
		return nil, err
	}
	out := make([]Session, len(sessions)) // never nil: an empty list is [], not null
	for index, sess := range sessions {
		sess.Current = sess.ID == actor.SessionID
		out[index] = sess
	}
	return out, nil
}

// RevokeSession signs out one of the user's other sessions. The current
// session ends through sign-out, so a request cannot cut itself off.
func (service *UserService) RevokeSession(ctx context.Context, actor Actor, userID, sessionID string) error {
	if err := self(actor, userID); err != nil {
		return err
	}
	if sessionID == actor.SessionID {
		return ErrCurrentSession
	}
	return service.creds.RevokeSession(ctx, userID, sessionID)
}

// RevokeOtherSessions signs out every session but the actor's.
func (service *UserService) RevokeOtherSessions(ctx context.Context, actor Actor, userID string) error {
	if err := self(actor, userID); err != nil {
		return err
	}
	return service.creds.RevokeOtherSessions(ctx, userID, actor.SessionID)
}

// DeleteAccount deletes the user; sessions and app rows cascade.
func (service *UserService) DeleteAccount(ctx context.Context, actor Actor, userID string) error {
	if err := self(actor, userID); err != nil {
		return err
	}
	return notFound(service.repo.Delete(ctx, userID), ErrUserNotFound)
}

// DataSummary lists what the app stores for the user (shown before deletion).
func (service *UserService) DataSummary(ctx context.Context, actor Actor, userID string) (UserDataSummary, error) {
	if err := self(actor, userID); err != nil {
		return UserDataSummary{}, err
	}
	sum, err := service.repo.DataSummary(ctx, userID)
	return sum, notFound(err, ErrUserNotFound)
}
