package server

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jayleaton/go-fast-stack/apps/admin/internal/services"
	"github.com/jayleaton/go-fast-stack/packages/auth"
)

// UserDirectory adapts *auth.UserAdmin (over the main app's database) to
// services.UserDirectory. It is translation only: who may call it is decided
// in services, the ban and session rules live in packages/auth.
type UserDirectory struct {
	users *auth.UserAdmin
}

var _ services.UserDirectory = UserDirectory{}

// NewUserDirectory returns the adapter over u.
func NewUserDirectory(admin *auth.UserAdmin) UserDirectory {
	return UserDirectory{users: admin}
}

func (directory UserDirectory) ListUsers(ctx context.Context, query services.UserQuery) (services.UserPage, error) {
	page, err := directory.users.ListUsers(ctx, auth.ListUsersQuery{
		Search: query.Search, SearchField: query.SearchField, Role: query.Role, Banned: query.Banned,
		SortBy: query.SortBy, SortDesc: query.SortDesc, Limit: query.Limit, Offset: query.Offset,
	})
	if err != nil {
		return services.UserPage{}, directoryErr(err)
	}
	out := services.UserPage{Users: make([]services.User, len(page.Users)), Total: page.Total, Limit: page.Limit, Offset: page.Offset}
	for index, user := range page.Users {
		out.Users[index] = userFromAuth(user)
	}
	return out, nil
}

func (directory UserDirectory) GetUserDetail(ctx context.Context, userID string) (services.UserDetail, error) {
	detail, err := directory.users.GetUserDetail(ctx, userID)
	if err != nil {
		return services.UserDetail{}, directoryErr(err)
	}
	return services.UserDetail{User: userFromAuth(detail.User), Sessions: sessionsFromAuth(detail.Sessions)}, nil
}

func (directory UserDirectory) BanUser(ctx context.Context, userID, reason string, duration time.Duration) error {
	return directoryErr(directory.users.BanUser(ctx, userID, reason, duration))
}

func (directory UserDirectory) UnbanUser(ctx context.Context, userID string) error {
	return directoryErr(directory.users.UnbanUser(ctx, userID))
}

func (directory UserDirectory) SetRole(ctx context.Context, userID, role string) error {
	return directoryErr(directory.users.SetRole(ctx, userID, role))
}

func (directory UserDirectory) RevokeSession(ctx context.Context, userID, sessionID string) error {
	return directoryErr(directory.users.RevokeSession(ctx, userID, sessionID))
}

func (directory UserDirectory) RevokeUserSessions(ctx context.Context, userID string) error {
	return directoryErr(directory.users.RevokeUserSessions(ctx, userID))
}

func userFromAuth(user auth.AdminUser) services.User {
	return services.User{
		ID: user.ID, Name: user.Name, Email: user.Email, EmailVerified: user.EmailVerified, Image: user.Image,
		CreatedAt: user.CreatedAt, UpdatedAt: user.UpdatedAt, Role: user.Role, Banned: user.Banned,
		BanReason: user.BanReason, BanExpires: user.BanExpires, TwoFactorEnabled: user.TwoFactorEnabled, HasPassword: user.HasPassword,
	}
}

func sessionsFromAuth(in []auth.Session) []services.Session {
	out := make([]services.Session, len(in))
	for index, session := range in { // never the token
		out[index] = services.Session{ID: session.ID, CreatedAt: session.CreatedAt, ExpiresAt: session.ExpiresAt, IPAddress: session.IPAddress, UserAgent: session.UserAgent}
	}
	return out
}

// directoryErrors translates auth errors into the port's.
var directoryErrors = []struct {
	auth error
	port *services.Error
}{
	{auth.ErrUserNotFound, services.ErrUserNotFound},
	{auth.ErrSessionNotFound, services.ErrSessionNotFound},
	{auth.ErrInvalidRole, services.ErrInvalidRole},
}

// directoryErr wraps a known auth error in its port error (keeping the
// original in the chain) and passes everything else through.
func directoryErr(err error) error {
	for _, directoryError := range directoryErrors {
		if errors.Is(err, directoryError.auth) {
			return fmt.Errorf("%w: %w", directoryError.port, err)
		}
	}
	return err
}
