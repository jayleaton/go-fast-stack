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
func NewUserDirectory(u *auth.UserAdmin) UserDirectory {
	return UserDirectory{users: u}
}

func (d UserDirectory) ListUsers(ctx context.Context, q services.UserQuery) (services.UserPage, error) {
	page, err := d.users.ListUsers(ctx, auth.ListUsersQuery{
		Search: q.Search, SearchField: q.SearchField, Role: q.Role, Banned: q.Banned,
		SortBy: q.SortBy, SortDesc: q.SortDesc, Limit: q.Limit, Offset: q.Offset,
	})
	if err != nil {
		return services.UserPage{}, directoryErr(err)
	}
	out := services.UserPage{Users: make([]services.User, len(page.Users)), Total: page.Total, Limit: page.Limit, Offset: page.Offset}
	for i, u := range page.Users {
		out.Users[i] = userFromAuth(u)
	}
	return out, nil
}

func (d UserDirectory) GetUserDetail(ctx context.Context, userID string) (services.UserDetail, error) {
	detail, err := d.users.GetUserDetail(ctx, userID)
	if err != nil {
		return services.UserDetail{}, directoryErr(err)
	}
	return services.UserDetail{User: userFromAuth(detail.User), Sessions: sessionsFromAuth(detail.Sessions)}, nil
}

func (d UserDirectory) BanUser(ctx context.Context, userID, reason string, duration time.Duration) error {
	return directoryErr(d.users.BanUser(ctx, userID, reason, duration))
}

func (d UserDirectory) UnbanUser(ctx context.Context, userID string) error {
	return directoryErr(d.users.UnbanUser(ctx, userID))
}

func (d UserDirectory) SetRole(ctx context.Context, userID, role string) error {
	return directoryErr(d.users.SetRole(ctx, userID, role))
}

func (d UserDirectory) RevokeSession(ctx context.Context, userID, sessionID string) error {
	return directoryErr(d.users.RevokeSession(ctx, userID, sessionID))
}

func (d UserDirectory) RevokeUserSessions(ctx context.Context, userID string) error {
	return directoryErr(d.users.RevokeUserSessions(ctx, userID))
}

func userFromAuth(u auth.AdminUser) services.User {
	return services.User{
		ID: u.ID, Name: u.Name, Email: u.Email, EmailVerified: u.EmailVerified, Image: u.Image,
		CreatedAt: u.CreatedAt, UpdatedAt: u.UpdatedAt, Role: u.Role, Banned: u.Banned,
		BanReason: u.BanReason, BanExpires: u.BanExpires, TwoFactorEnabled: u.TwoFactorEnabled, HasPassword: u.HasPassword,
	}
}

func sessionsFromAuth(in []auth.Session) []services.Session {
	out := make([]services.Session, len(in))
	for i, s := range in { // never the token
		out[i] = services.Session{ID: s.ID, CreatedAt: s.CreatedAt, ExpiresAt: s.ExpiresAt, IPAddress: s.IPAddress, UserAgent: s.UserAgent}
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
	for _, e := range directoryErrors {
		if errors.Is(err, e.auth) {
			return fmt.Errorf("%w: %w", e.port, err)
		}
	}
	return err
}
