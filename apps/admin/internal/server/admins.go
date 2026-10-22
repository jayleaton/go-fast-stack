package server

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jayleaton/ai-patterns-go/apps/admin/internal/config"
	"github.com/jayleaton/ai-patterns-go/apps/admin/internal/services"
	"github.com/jayleaton/ai-patterns-go/packages/auth"
	"github.com/jayleaton/ai-patterns-go/packages/database/adminrepo"
)

// maxAdmins bounds the admin list (one page of auth.ListUsers); a dashboard
// with more admins than this needs paging here.
const maxAdmins = 100

// AdminAccounts adapts the admin app's own *auth.Auth (admin database) to
// services.AdminAccounts, with adminrepo.Admins for removal (auth has no
// delete). Translation only.
type AdminAccounts struct {
	auth  *auth.Auth
	store *adminrepo.Admins
}

var _ services.AdminAccounts = AdminAccounts{}

// disabledReason is the ban reason auth stores for a disabled admin.
const disabledReason = "Disabled by an admin"

// NewAdminAccounts returns the adapter over the admin auth instance and the
// admin database's repository.
func NewAdminAccounts(a *auth.Auth, store *adminrepo.Admins) AdminAccounts {
	return AdminAccounts{auth: a, store: store}
}

func (a AdminAccounts) AdminByID(ctx context.Context, id string) (services.Admin, error) {
	d, err := a.auth.Users().GetUserDetail(ctx, id)
	if errors.Is(err, auth.ErrUserNotFound) {
		return services.Admin{}, fmt.Errorf("%w: %w", services.ErrRecordNotFound, err)
	}
	if err != nil {
		return services.Admin{}, err
	}
	return adminFromAuth(d.User), nil
}

// DisableAdmin bans the admin permanently: auth refuses their sign-in and
// revokes every session.
func (a AdminAccounts) DisableAdmin(ctx context.Context, id string) error {
	return a.auth.Users().BanUser(ctx, id, disabledReason, 0)
}

func (a AdminAccounts) EnableAdmin(ctx context.Context, id string) error {
	return a.auth.Users().UnbanUser(ctx, id)
}

func (a AdminAccounts) RemoveAdmin(ctx context.Context, id string) error {
	return a.store.Remove(ctx, id)
}

func (a AdminAccounts) ListAdmins(ctx context.Context) ([]services.Admin, error) {
	page, err := a.auth.Users().ListUsers(ctx, auth.ListUsersQuery{SortBy: "createdAt", Limit: maxAdmins})
	if err != nil {
		return nil, err
	}
	out := make([]services.Admin, len(page.Users))
	for i, u := range page.Users {
		out[i] = adminFromAuth(u)
	}
	return out, nil
}

func (a AdminAccounts) AdminByEmail(ctx context.Context, email string) (services.Admin, error) {
	page, err := a.auth.Users().ListUsers(ctx, auth.ListUsersQuery{Search: email, SearchField: "email", Limit: maxAdmins})
	if err != nil {
		return services.Admin{}, err
	}
	for _, u := range page.Users { // Search is "contains"; keep the exact match
		if strings.EqualFold(u.Email, email) {
			return adminFromAuth(u), nil
		}
	}
	return services.Admin{}, services.ErrRecordNotFound
}

func (a AdminAccounts) CreateAdmin(ctx context.Context, email, name string) (services.Admin, error) {
	u, err := a.auth.CreateUserWithoutPassword(ctx, auth.CreateUserInput{Name: name, Email: email, Role: "admin"})
	if errors.Is(err, auth.ErrUserAlreadyExists) {
		return services.Admin{}, fmt.Errorf("%w: %w", services.ErrAdminExists, err)
	}
	if err != nil {
		return services.Admin{}, err
	}
	return services.Admin{ID: u.ID, Name: u.Name, Email: u.Email, CreatedAt: u.CreatedAt}, nil
}

func (a AdminAccounts) SendSetPasswordLink(ctx context.Context, adminID string) error {
	return a.auth.IssueSetPasswordLink(ctx, adminID, config.AppRoutes.SetPassword)
}

func adminFromAuth(u auth.AdminUser) services.Admin {
	return services.Admin{ID: u.ID, Name: u.Name, Email: u.Email, CreatedAt: u.CreatedAt, TwoFactorEnabled: u.TwoFactorEnabled, HasPassword: u.HasPassword, Disabled: u.Banned}
}
