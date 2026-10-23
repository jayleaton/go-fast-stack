package services

//go:generate go tool mockgen -source=admins.go -destination=mocks/admins.go -package=mocks

import (
	"context"
	"errors"
	"time"

	"github.com/jayleaton/go-fast-stack/apps/admin/internal/validate"
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
func (admin Admin) Active() bool { return admin.HasPassword && !admin.Disabled }

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
	// ResetTwoFactor removes the admin's TOTP enrolment, backup codes and
	// trusted devices; with 2FA required they enrol again at next sign-in.
	ResetTwoFactor(ctx context.Context, adminID string) error
}

// Audit actions on admins.
const (
	ActionAdminInvite    = "admin.invite"
	ActionAdminBootstrap = "admin.bootstrap"
	ActionAdminDisable   = "admin.disable"
	ActionAdminEnable    = "admin.enable"
	ActionAdminRemove    = "admin.remove"
	// ActionAdminPasswordReset emails another admin a link to choose a new
	// password (their other sessions end when it is used).
	ActionAdminPasswordReset = "admin.password_reset"
	// ActionAdminResetTwoFactor removes an admin's 2FA (lost authenticator
	// and backup codes), from the Admins page or `admin reset-2fa`.
	ActionAdminResetTwoFactor = "admin.reset_two_factor"
)

// AdminService errors.
var (
	ErrAdminExists      = &Error{KindConflict, "This person is already an admin"}
	ErrAdminNotFound    = &Error{KindNotFound, "Admin not found"}
	ErrAdminSelf        = &Error{KindInvalid, "You cannot disable or remove yourself"}
	ErrLastAdmin        = &Error{KindConflict, "The last active admin cannot be disabled or removed"}
	ErrAdminDisabled    = &Error{KindInvalid, "This admin is already disabled"}
	ErrAdminNotDisabled = &Error{KindInvalid, "This admin is not disabled"}
	ErrAdminPending     = &Error{KindInvalid, "This admin has not accepted the invite yet; send the invite again instead"}
	ErrAdminNoTwoFactor = &Error{KindInvalid, "This admin has no two-factor authentication to reset"}
	ErrAdminSelfReset   = &Error{KindInvalid, "Use \"Forgot password?\" on the sign-in page or a backup code to recover your own account"}
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
func (service *AdminService) ListAdmins(ctx context.Context, actor Actor) ([]Admin, error) {
	if err := service.policy.authorize(actor); err != nil {
		return nil, err
	}
	admins, err := service.accounts.ListAdmins(ctx)
	if admins == nil {
		admins = []Admin{}
	}
	return admins, err
}

// InviteAdmin creates the admin (unless a pending invite exists, which is
// sent again) and emails the set-password link, as one audited mutation. The
// intent of a first invite targets the email: the admin has no id until the
// change runs.
func (service *AdminService) InviteAdmin(ctx context.Context, actor Actor, in validate.CreateAdminInvite) (Admin, error) {
	if err := service.policy.authorize(actor); err != nil {
		return Admin{}, err
	}
	admin, existed, err := service.find(ctx, in.Email)
	if err != nil {
		return Admin{}, err
	}
	if existed && admin.HasPassword {
		return Admin{}, ErrAdminExists
	}
	entry := NewAuditEntry{Action: ActionAdminInvite, TargetType: TargetAdmin, TargetID: in.Email, TargetLabel: in.Email, Details: map[string]string{}}
	if existed {
		// An existing admin (e.g. seeded by bootstrap) may never have been sent
		// a link: it is a resend only if an earlier invite is on record.
		resend, err := service.invitedBefore(ctx, admin)
		if err != nil {
			return Admin{}, err
		}
		entry = adminEntry(ActionAdminInvite, admin, map[string]string{})
		if resend {
			entry.Details["resent"] = "true"
		}
	}
	err = mutate(ctx, service.log, actor, entry, func() error {
		if !existed {
			created, err := service.accounts.CreateAdmin(ctx, in.Email, in.Name)
			if err != nil {
				return err
			}
			admin = created
		}
		return adminMissing(service.accounts.SendSetPasswordLink(ctx, admin.ID))
	})
	if err != nil {
		return Admin{}, err
	}
	return admin, nil
}

// invitedBefore reports whether an invite for a is on record (under its id,
// or its email for the first invite).
func (service *AdminService) invitedBefore(ctx context.Context, admin Admin) (bool, error) {
	for _, target := range []string{admin.ID, admin.Email} {
		prior, err := service.log.List(ctx, AuditQuery{Action: ActionAdminInvite, TargetID: target, Limit: 1})
		if err != nil || prior.Total > 0 {
			return err == nil, err
		}
	}
	return false, nil
}

// DisableAdmin blocks the admin's sign-in and signs them out everywhere.
// Nobody disables themselves or the last active admin.
func (service *AdminService) DisableAdmin(ctx context.Context, actor Actor, adminID string) error {
	admin, err := service.removable(ctx, actor, adminID)
	if err != nil {
		return err
	}
	if admin.Disabled {
		return ErrAdminDisabled
	}
	return mutate(ctx, service.log, actor, adminEntry(ActionAdminDisable, admin, nil), func() error {
		return adminMissing(service.accounts.DisableAdmin(ctx, admin.ID))
	})
}

// EnableAdmin lets a disabled admin sign in again.
func (service *AdminService) EnableAdmin(ctx context.Context, actor Actor, adminID string) error {
	if err := service.policy.authorize(actor); err != nil {
		return err
	}
	admin, err := service.byID(ctx, adminID)
	if err != nil {
		return err
	}
	if !admin.Disabled {
		return ErrAdminNotDisabled
	}
	return mutate(ctx, service.log, actor, adminEntry(ActionAdminEnable, admin, nil), func() error {
		return adminMissing(service.accounts.EnableAdmin(ctx, admin.ID))
	})
}

// RemoveAdmin deletes the admin (or cancels a pending invite). Nobody removes
// themselves or the last active admin.
func (service *AdminService) RemoveAdmin(ctx context.Context, actor Actor, adminID string) error {
	admin, err := service.removable(ctx, actor, adminID)
	if err != nil {
		return err
	}
	return mutate(ctx, service.log, actor, adminEntry(ActionAdminRemove, admin, nil), func() error {
		return adminMissing(service.accounts.RemoveAdmin(ctx, admin.ID))
	})
}

// SendPasswordReset emails another admin a single-use link to choose a new
// password (they can also ask for it themselves on the sign-in page). Using
// it ends their other sessions; sign-in still asks for the second factor. A
// pending admin gets the invite again instead (InviteAdmin).
func (service *AdminService) SendPasswordReset(ctx context.Context, actor Actor, adminID string) error {
	admin, err := service.recoverable(ctx, actor, adminID)
	if err != nil {
		return err
	}
	return mutate(ctx, service.log, actor, adminEntry(ActionAdminPasswordReset, admin, nil), func() error {
		return adminMissing(service.accounts.SendSetPasswordLink(ctx, admin.ID))
	})
}

// ResetTwoFactor removes another admin's 2FA, for an admin who lost both the
// authenticator and the backup codes. They enrol again at their next sign-in
// (ADMIN_REQUIRE_2FA) and Policy refuses them until they do. The operator
// (SystemActor, `admin reset-2fa`) uses it when no admin is left to do it.
func (service *AdminService) ResetTwoFactor(ctx context.Context, actor Actor, adminID string) error {
	admin, err := service.recoverable(ctx, actor, adminID)
	if err != nil {
		return err
	}
	if !admin.TwoFactorEnabled {
		return ErrAdminNoTwoFactor
	}
	return mutate(ctx, service.log, actor, adminEntry(ActionAdminResetTwoFactor, admin, nil), func() error {
		return adminMissing(service.accounts.ResetTwoFactor(ctx, admin.ID))
	})
}

// ResetTwoFactorByEmail is ResetTwoFactor for the operator (`admin
// reset-2fa <email>`), who knows the admin by email: the recovery when the
// last admin lost the authenticator and the backup codes. Only SystemActor
// may call it.
func (service *AdminService) ResetTwoFactorByEmail(ctx context.Context, actor Actor, in validate.ResetAdminTwoFactor) (Admin, error) {
	if !actor.System {
		return Admin{}, ErrForbidden
	}
	admin, existed, err := service.find(ctx, in.Email)
	if err != nil {
		return Admin{}, err
	}
	if !existed {
		return Admin{}, ErrAdminNotFound
	}
	if err := service.ResetTwoFactor(ctx, actor, admin.ID); err != nil {
		return Admin{}, err
	}
	admin.TwoFactorEnabled = false
	return admin, nil
}

// recoverable authorizes the actor and loads another admin whose account
// they may recover: not themselves (an admin who can act does not need it),
// and one who has accepted the invite.
func (service *AdminService) recoverable(ctx context.Context, actor Actor, adminID string) (Admin, error) {
	if err := service.policy.authorize(actor); err != nil {
		return Admin{}, err
	}
	if adminID == actor.AdminID {
		return Admin{}, ErrAdminSelfReset
	}
	admin, err := service.byID(ctx, adminID)
	if err != nil {
		return Admin{}, err
	}
	if !admin.HasPassword {
		return Admin{}, ErrAdminPending
	}
	return admin, nil
}

// removable authorizes the actor and loads an admin they may disable or
// remove: not themselves, and never the last active admin, so the dashboard
// cannot lock everyone out.
func (service *AdminService) removable(ctx context.Context, actor Actor, adminID string) (Admin, error) {
	if err := service.policy.authorize(actor); err != nil {
		return Admin{}, err
	}
	if adminID == actor.AdminID {
		return Admin{}, ErrAdminSelf
	}
	admin, err := service.byID(ctx, adminID)
	if err != nil || !admin.Active() {
		return admin, err
	}
	all, err := service.accounts.ListAdmins(ctx)
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
	return admin, nil
}

func (service *AdminService) byID(ctx context.Context, id string) (Admin, error) {
	admin, err := service.accounts.AdminByID(ctx, id)
	if err != nil {
		return Admin{}, adminMissing(err)
	}
	return admin, nil
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
// password; `admin migrate` then emails the invite (InviteBootstrapAdmin).
// Idempotent: an existing
// admin is returned with created false. Only the operator (SystemActor) may
// call it. Creating it is an audited mutation targeting the email.
func (service *AdminService) Bootstrap(ctx context.Context, actor Actor, in validate.CreateAdminInvite) (admin Admin, created bool, err error) {
	if !actor.System {
		return Admin{}, false, ErrForbidden
	}
	admin, existed, err := service.find(ctx, in.Email)
	if err != nil || existed {
		return admin, false, err
	}
	entry := NewAuditEntry{Action: ActionAdminBootstrap, TargetType: TargetAdmin, TargetID: in.Email, TargetLabel: in.Email}
	err = mutate(ctx, service.log, actor, entry, func() error {
		admin, err = service.accounts.CreateAdmin(ctx, in.Email, in.Name)
		return err
	})
	if err != nil {
		return Admin{}, false, err
	}
	return admin, true, nil
}

// InviteBootstrapAdmin emails the bootstrap admin's invite from `admin
// migrate` (the pre-deploy command, which runs inside the image), so a
// deployment whose image has no shell to run `admin invite` in still gets its
// first admin. It sends only while a is pending with no invite on record, so
// later deploys send nothing (a fresh link: "Forgot password?" on the sign-in
// page, or another admin). Only the operator may call it.
func (service *AdminService) InviteBootstrapAdmin(ctx context.Context, actor Actor, admin Admin) (sent bool, err error) {
	if !actor.System {
		return false, ErrForbidden
	}
	if admin.HasPassword {
		return false, nil
	}
	if invited, err := service.invitedBefore(ctx, admin); err != nil || invited {
		return false, err
	}
	err = mutate(ctx, service.log, actor, adminEntry(ActionAdminInvite, admin, map[string]string{}), func() error {
		return adminMissing(service.accounts.SendSetPasswordLink(ctx, admin.ID))
	})
	return err == nil, err
}

// find returns the admin with email; existed is false when there is none.
func (service *AdminService) find(ctx context.Context, email string) (admin Admin, existed bool, err error) {
	admin, err = service.accounts.AdminByEmail(ctx, email)
	switch {
	case err == nil:
		return admin, true, nil
	case errors.Is(err, ErrRecordNotFound):
		return Admin{}, false, nil
	}
	return Admin{}, false, err
}

func adminEntry(action string, admin Admin, details map[string]string) NewAuditEntry {
	return NewAuditEntry{Action: action, TargetType: TargetAdmin, TargetID: admin.ID, TargetLabel: admin.Email, Details: details}
}
