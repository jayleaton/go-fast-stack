// Package services holds the admin app's business rules. Both admin
// transports (the JSON API and the HTML UI) call services in-process. Services
// see everything below them through ports declared here, by the consumer:
// repositories in packages/database/adminrepo and adapters over packages/auth
// in apps/admin/internal/server. Services never import net/http, templ, pgx,
// sqlc output or packages/auth.
//
// Every method takes the acting admin first and checks Policy.authorize
// before touching a port; every mutating method writes the audit log.
// UserService (users.go) is the reference.
package services

import (
	"context"
	"errors"
	"fmt"

	"github.com/jayleaton/ai-patterns-go/packages/database/adminrepo/model"
)

// Actor is the admin a service call acts for and the session the request
// came in on. Transports build it from the caller; the CLI uses SystemActor.
type Actor struct {
	AdminID          string
	Email            string
	SessionID        string
	TwoFactorEnabled bool
	// System marks the trusted operator (the admin binary's CLI, run with
	// shell access to the deployment). Transports never set it.
	System bool
}

// SystemActor is the operator running an `admin` CLI command; name shows in
// the audit log (e.g. "cli").
func SystemActor(name string) Actor {
	return Actor{AdminID: "system:" + name, Email: name, System: true}
}

// Kind classifies a service error; transports map it to a status code.
type Kind int

const (
	KindInternal Kind = iota
	KindInvalid
	KindUnauthorized
	KindForbidden
	KindNotFound
	KindConflict
	KindRateLimited
)

// Error is a business-rule failure whose Message is safe to show to callers.
type Error struct {
	Kind    Kind
	Message string
}

func (e *Error) Error() string { return e.Message }

// Kind sentinels: errors.Is(err, ErrNotFound) holds for every not-found
// error, so callers can branch on a kind without knowing the specific error.
var (
	ErrValidation = &Error{Kind: KindInvalid, Message: "Invalid request"}
	ErrForbidden  = &Error{Kind: KindForbidden, Message: "Forbidden"}
	ErrNotFound   = &Error{Kind: KindNotFound, Message: "Not found"}
	ErrConflict   = &Error{Kind: KindConflict, Message: "Conflict"}
)

// Is makes the kind sentinels match every error of their kind. Specific
// errors (ErrUserNotFound...) match only themselves.
func (e *Error) Is(target error) bool {
	switch target {
	case ErrValidation, ErrForbidden, ErrNotFound, ErrConflict:
		return e.Kind == target.(*Error).Kind
	}
	return false
}

// ErrTwoFactorRequired: the dashboard requires 2FA and the admin has not
// enrolled (a session from before ADMIN_REQUIRE_2FA was turned on).
var ErrTwoFactorRequired = &Error{KindForbidden, "Two-factor authentication is required"}

// ErrRecordNotFound is what repository ports return for a missing row
// (model.ErrNotFound, re-exported so service tests need not import adminrepo).
var ErrRecordNotFound = model.ErrNotFound

// KindOf returns the Kind of err (KindInternal for unknown errors).
func KindOf(err error) Kind {
	var se *Error
	if errors.As(err, &se) {
		return se.Kind
	}
	return KindInternal
}

// Policy is the authorization rule shared by every admin service: any admin
// may act (all admins are equal), once they meet the 2FA requirement.
type Policy struct {
	RequireTwoFactor bool
}

func (p Policy) authorize(a Actor) error {
	switch {
	case a.System:
		return nil
	case a.AdminID == "":
		return ErrForbidden
	case p.RequireTwoFactor && !a.TwoFactorEnabled:
		return ErrTwoFactorRequired
	}
	return nil
}

// audit writes e for actor. The action already happened in another database,
// so a failed write is returned as an internal error (never swallowed): the
// caller sees the request fail and the log line names what is missing.
func audit(ctx context.Context, log AuditLog, actor Actor, e NewAuditEntry) error {
	e.ActorID, e.ActorEmail = actor.AdminID, actor.Email
	if err := log.Append(ctx, e); err != nil {
		return fmt.Errorf("audit %s %s/%s (action applied, not recorded): %w", e.Action, e.TargetType, e.TargetID, err)
	}
	return nil
}
