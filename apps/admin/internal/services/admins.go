package services

//go:generate go tool mockgen -source=admins.go -destination=mocks/admins.go -package=mocks

import (
	"context"
	"errors"
	"time"

	"github.com/jayleaton/ai-patterns-go/apps/admin/internal/validate"
)

// Admin is an admin of this dashboard: a user of the admin app's own auth
// instance (admin database), never a row in the main app's "user" table.
type Admin struct {
	ID               string    `json:"id"`
	Name             string    `json:"name"`
	Email            string    `json:"email"`
	CreatedAt        time.Time `json:"createdAt"`
	TwoFactorEnabled bool      `json:"twoFactorEnabled"`
	// HasPassword is false until the invite's set-password link is used.
	HasPassword bool `json:"hasPassword"`
}

// AdminAccounts is the port to the admin auth instance (an adapter over the
// admin *auth.Auth in apps/admin/internal/server). Implementations report a
// missing admin as ErrRecordNotFound and an email already in use as
// ErrAdminExists; any other error is internal.
type AdminAccounts interface {
	ListAdmins(ctx context.Context) ([]Admin, error)
	AdminByEmail(ctx context.Context, email string) (Admin, error)
	// CreateAdmin creates an admin without a password (the invite link sets it).
	CreateAdmin(ctx context.Context, email, name string) (Admin, error)
	// SendSetPasswordLink emails a single-use set-password link; using it
	// may continue into 2FA enrolment.
	SendSetPasswordLink(ctx context.Context, adminID string) error
}

// Audit actions on admins.
const (
	ActionAdminInvite    = "admin.invite"
	ActionAdminBootstrap = "admin.bootstrap"
)

// AdminService errors.
var (
	ErrAdminExists = &Error{KindConflict, "This person is already an admin"}
)

// AdminService manages who can use the dashboard. Only admins invite admins
// (UI, API, or the operator through `admin invite`).
type AdminService struct {
	accounts AdminAccounts
	log      AuditLog
	policy   Policy
}

// NewAdminService returns an AdminService over its ports.
func NewAdminService(accounts AdminAccounts, log AuditLog, policy Policy) *AdminService {
	return &AdminService{accounts: accounts, log: log, policy: policy}
}

// ListAdmins returns every admin, including pending invites.
func (s *AdminService) ListAdmins(ctx context.Context, actor Actor) ([]Admin, error) {
	if err := s.policy.authorize(actor); err != nil {
		return nil, err
	}
	admins, err := s.accounts.ListAdmins(ctx)
	if admins == nil {
		admins = []Admin{}
	}
	return admins, err
}

// InviteAdmin creates the admin (unless a pending invite exists, which is
// sent again) and emails the set-password link.
func (s *AdminService) InviteAdmin(ctx context.Context, actor Actor, in validate.CreateAdminInvite) (Admin, error) {
	if err := s.policy.authorize(actor); err != nil {
		return Admin{}, err
	}
	a, resend, err := s.findOrCreate(ctx, in)
	if err != nil {
		return Admin{}, err
	}
	if resend && a.HasPassword {
		return Admin{}, ErrAdminExists
	}
	if err := s.accounts.SendSetPasswordLink(ctx, a.ID); err != nil {
		return Admin{}, err
	}
	details := map[string]string{}
	if resend {
		details["resent"] = "true"
	}
	return a, audit(ctx, s.log, actor, adminEntry(ActionAdminInvite, a, details))
}

// Bootstrap seeds the first admin (ADMIN_BOOTSTRAP_EMAIL) without a
// password; `admin invite` then sends the link. Idempotent: an existing
// admin is returned with created false. Only the operator (SystemActor) may
// call it.
func (s *AdminService) Bootstrap(ctx context.Context, actor Actor, in validate.CreateAdminInvite) (a Admin, created bool, err error) {
	if !actor.System {
		return Admin{}, false, ErrForbidden
	}
	a, existed, err := s.findOrCreate(ctx, in)
	if err != nil || existed {
		return a, false, err
	}
	return a, true, audit(ctx, s.log, actor, adminEntry(ActionAdminBootstrap, a, nil))
}

// findOrCreate returns the admin with in.Email, creating it when missing;
// existed reports which.
func (s *AdminService) findOrCreate(ctx context.Context, in validate.CreateAdminInvite) (a Admin, existed bool, err error) {
	a, err = s.accounts.AdminByEmail(ctx, in.Email)
	if err == nil {
		return a, true, nil
	}
	if !errors.Is(err, ErrRecordNotFound) {
		return Admin{}, false, err
	}
	a, err = s.accounts.CreateAdmin(ctx, in.Email, in.Name)
	return a, false, err
}

func adminEntry(action string, a Admin, details map[string]string) NewAuditEntry {
	return NewAuditEntry{Action: action, TargetType: TargetAdmin, TargetID: a.ID, TargetLabel: a.Email, Details: details}
}
