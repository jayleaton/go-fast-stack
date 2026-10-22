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
	// Disabled admins cannot sign in; another admin can enable them again.
	Disabled bool `json:"disabled"`
}

// Active reports whether the admin can sign in: invite accepted, not disabled.
func (a Admin) Active() bool { return a.HasPassword && !a.Disabled }

// AdminAccounts is the port to the admin auth instance (an adapter over the
// admin *auth.Auth in apps/admin/internal/server). Implementations report a
// missing admin as ErrRecordNotFound and an email already in use as
// ErrAdminExists; any other error is internal.
type AdminAccounts interface {
	ListAdmins(ctx context.Context) ([]Admin, error)
	AdminByID(ctx context.Context, id string) (Admin, error)
	AdminByEmail(ctx context.Context, email string) (Admin, error)
	// DisableAdmin blocks sign-in and revokes every session of the admin.
	DisableAdmin(ctx context.Context, id string) error
	EnableAdmin(ctx context.Context, id string) error
	// RemoveAdmin deletes the admin; their sessions go with them. Audit
	// entries keep their id and email.
	RemoveAdmin(ctx context.Context, id string) error
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
	ActionAdminDisable   = "admin.disable"
	ActionAdminEnable    = "admin.enable"
	ActionAdminRemove    = "admin.remove"
)

// AdminService errors.
var (
	ErrAdminExists      = &Error{KindConflict, "This person is already an admin"}
	ErrAdminNotFound    = &Error{KindNotFound, "Admin not found"}
	ErrAdminSelf        = &Error{KindInvalid, "You cannot disable or remove yourself"}
	ErrLastAdmin        = &Error{KindConflict, "The last active admin cannot be disabled or removed"}
	ErrAdminDisabled    = &Error{KindInvalid, "This admin is already disabled"}
	ErrAdminNotDisabled = &Error{KindInvalid, "This admin is not disabled"}
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
		// An existing admin (e.g. seeded by bootstrap) may never have been sent
		// a link: it is a resend only if an earlier invite is on record.
		prior, err := s.log.List(ctx, AuditQuery{Action: ActionAdminInvite, TargetID: a.ID, Limit: 1})
		if err != nil {
			return Admin{}, err
		}
		if prior.Total > 0 {
			details["resent"] = "true"
		}
	}
	return a, audit(ctx, s.log, actor, adminEntry(ActionAdminInvite, a, details))
}

// DisableAdmin blocks the admin's sign-in and signs them out everywhere.
// Nobody disables themselves or the last active admin.
func (s *AdminService) DisableAdmin(ctx context.Context, actor Actor, adminID string) error {
	a, err := s.removable(ctx, actor, adminID)
	if err != nil {
		return err
	}
	if a.Disabled {
		return ErrAdminDisabled
	}
	if err := s.accounts.DisableAdmin(ctx, a.ID); err != nil {
		return adminMissing(err)
	}
	return audit(ctx, s.log, actor, adminEntry(ActionAdminDisable, a, nil))
}

// EnableAdmin lets a disabled admin sign in again.
func (s *AdminService) EnableAdmin(ctx context.Context, actor Actor, adminID string) error {
	if err := s.policy.authorize(actor); err != nil {
		return err
	}
	a, err := s.byID(ctx, adminID)
	if err != nil {
		return err
	}
	if !a.Disabled {
		return ErrAdminNotDisabled
	}
	if err := s.accounts.EnableAdmin(ctx, a.ID); err != nil {
		return adminMissing(err)
	}
	return audit(ctx, s.log, actor, adminEntry(ActionAdminEnable, a, nil))
}

// RemoveAdmin deletes the admin (or cancels a pending invite). Nobody removes
// themselves or the last active admin.
func (s *AdminService) RemoveAdmin(ctx context.Context, actor Actor, adminID string) error {
	a, err := s.removable(ctx, actor, adminID)
	if err != nil {
		return err
	}
	if err := s.accounts.RemoveAdmin(ctx, a.ID); err != nil {
		return adminMissing(err)
	}
	return audit(ctx, s.log, actor, adminEntry(ActionAdminRemove, a, nil))
}

// removable authorizes the actor and loads an admin they may disable or
// remove: not themselves, and never the last active admin, so the dashboard
// cannot lock everyone out.
func (s *AdminService) removable(ctx context.Context, actor Actor, adminID string) (Admin, error) {
	if err := s.policy.authorize(actor); err != nil {
		return Admin{}, err
	}
	if adminID == actor.AdminID {
		return Admin{}, ErrAdminSelf
	}
	a, err := s.byID(ctx, adminID)
	if err != nil || !a.Active() {
		return a, err
	}
	all, err := s.accounts.ListAdmins(ctx)
	if err != nil {
		return Admin{}, err
	}
	active := 0
	for _, other := range all {
		if other.Active() {
			active++
		}
	}
	if active <= 1 {
		return Admin{}, ErrLastAdmin
	}
	return a, nil
}

func (s *AdminService) byID(ctx context.Context, id string) (Admin, error) {
	a, err := s.accounts.AdminByID(ctx, id)
	if err != nil {
		return Admin{}, adminMissing(err)
	}
	return a, nil
}

// adminMissing maps a port miss (also one between the lookup and the change,
// when another admin removed them first) to ErrAdminNotFound.
func adminMissing(err error) error {
	if errors.Is(err, ErrRecordNotFound) {
		return ErrAdminNotFound
	}
	return err
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
