package server

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jayleaton/go-fast-stack/apps/admin/internal/config"
	"github.com/jayleaton/go-fast-stack/apps/admin/internal/services"
	"github.com/jayleaton/go-fast-stack/packages/auth"
	"github.com/jayleaton/go-fast-stack/packages/database/adminrepo"
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
func NewAdminAccounts(adminAuth *auth.Auth, store *adminrepo.Admins) AdminAccounts {
	return AdminAccounts{auth: adminAuth, store: store}
}

func (accounts AdminAccounts) AdminByID(ctx context.Context, id string) (services.Admin, error) {
	detail, err := accounts.auth.Users().GetUserDetail(ctx, id)
	if errors.Is(err, auth.ErrUserNotFound) {
		return services.Admin{}, fmt.Errorf("%w: %w", services.ErrRecordNotFound, err)
	}
	if err != nil {
		return services.Admin{}, err
	}
	return adminFromAuth(detail.User), nil
}

// DisableAdmin bans the admin permanently: auth refuses their sign-in and
// revokes every session.
func (accounts AdminAccounts) DisableAdmin(ctx context.Context, id string) error {
	return accounts.auth.Users().BanUser(ctx, id, disabledReason, 0)
}

func (accounts AdminAccounts) EnableAdmin(ctx context.Context, id string) error {
	return accounts.auth.Users().UnbanUser(ctx, id)
}

func (accounts AdminAccounts) RemoveAdmin(ctx context.Context, id string) error {
	return accounts.store.Remove(ctx, id)
}

func (accounts AdminAccounts) ListAdmins(ctx context.Context) ([]services.Admin, error) {
	page, err := accounts.auth.Users().ListUsers(ctx, auth.ListUsersQuery{SortBy: "createdAt", Limit: maxAdmins})
	if err != nil {
		return nil, err
	}
	out := make([]services.Admin, len(page.Users))
	for index, user := range page.Users {
		out[index] = adminFromAuth(user)
	}
	return out, nil
}

func (accounts AdminAccounts) AdminByEmail(ctx context.Context, email string) (services.Admin, error) {
	page, err := accounts.auth.Users().ListUsers(ctx, auth.ListUsersQuery{Search: email, SearchField: "email", Limit: maxAdmins})
	if err != nil {
		return services.Admin{}, err
	}
	for _, user := range page.Users { // Search is "contains"; keep the exact match
		if strings.EqualFold(user.Email, email) {
			return adminFromAuth(user), nil
		}
	}
	return services.Admin{}, services.ErrRecordNotFound
}

func (accounts AdminAccounts) CreateAdmin(ctx context.Context, email, name string) (services.Admin, error) {
	user, err := accounts.auth.CreateUserWithoutPassword(ctx, auth.CreateUserInput{Name: name, Email: email, Role: "admin"})
	if errors.Is(err, auth.ErrUserAlreadyExists) {
		return services.Admin{}, fmt.Errorf("%w: %w", services.ErrAdminExists, err)
	}
	if err != nil {
		return services.Admin{}, err
	}
	return services.Admin{ID: user.ID, Name: user.Name, Email: user.Email, CreatedAt: user.CreatedAt}, nil
}

func (accounts AdminAccounts) SendSetPasswordLink(ctx context.Context, adminID string) error {
	return accounts.auth.IssueSetPasswordLink(ctx, adminID, config.AppRoutes.SetPassword)
}

func (accounts AdminAccounts) ResetTwoFactor(ctx context.Context, adminID string) error {
	err := accounts.auth.Users().ResetTwoFactor(ctx, adminID)
	if errors.Is(err, auth.ErrUserNotFound) {
		return fmt.Errorf("%w: %w", services.ErrRecordNotFound, err)
	}
	return err
}

func adminFromAuth(user auth.AdminUser) services.Admin {
	return services.Admin{ID: user.ID, Name: user.Name, Email: user.Email, CreatedAt: user.CreatedAt, TwoFactorEnabled: user.TwoFactorEnabled, HasPassword: user.HasPassword, Disabled: user.Banned}
}
