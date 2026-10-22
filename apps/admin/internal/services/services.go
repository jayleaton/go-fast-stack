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

	"github.com/jayleaton/go-fast-stack/packages/database/adminrepo/model"
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
	// KindUnavailable: a dependency the action needs is down; nothing changed.
	KindUnavailable
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

// ErrAuditUnavailable refuses a mutation whose intent could not be written:
// admin changes fail closed, so nothing happens without an audit record.
var ErrAuditUnavailable = &Error{KindUnavailable, "The audit log is unavailable, so nothing was changed. Try again later."}

// mutate runs change as an audited admin mutation that fails closed:
//
//  1. the intent is appended (status pending) — if that fails, change never
//     runs and the caller gets ErrAuditUnavailable;
//  2. change runs;
//  3. the outcome is appended: succeeded, or failed with a safe reason.
//
// If step 3 fails after a successful change, the pending row still proves
// the attempt (the UI shows "outcome unknown", AuditReconciler resolves it
// later) and the caller gets an internal error saying the change WAS applied,
// so it is logged loudly by the transport. A failed change whose outcome
// could not be written returns the change's own error.
func mutate(ctx context.Context, log AuditLog, actor Actor, e NewAuditEntry, change func() error) error {
	e.ActorID, e.ActorEmail = actor.AdminID, actor.Email
	id, err := log.Begin(ctx, e)
	if err != nil {
		return fmt.Errorf("%w: audit intent %s %s/%s: %w", ErrAuditUnavailable, e.Action, e.TargetType, e.TargetID, err)
	}
	changeErr := change()
	outcome := AuditOutcome{Status: StatusSucceeded}
	if changeErr != nil {
		outcome = AuditOutcome{Status: StatusFailed, Reason: safeReason(changeErr)}
	}
	if err := log.Resolve(ctx, id, outcome); err != nil && changeErr == nil {
		return fmt.Errorf("audit outcome of entry %d (%s %s/%s) NOT recorded; the change WAS applied and stays pending until reconciled: %w",
			id, e.Action, e.TargetType, e.TargetID, err)
	}
	return changeErr
}

// ReasonInternal is the recorded reason of a failure that is not a service
// error (the details are in the server log, never in the audit log).
const ReasonInternal = "Internal error"

// safeReason is what an outcome row may say about a failure: the service
// error's message, never a raw database or driver error.
func safeReason(err error) string {
	var se *Error
	if errors.As(err, &se) && se.Kind != KindInternal {
		return se.Message
	}
	return ReasonInternal
}

// audit appends e for actor as a completed fact (sign-ins and other events
// that are not admin mutations). A failed write is returned as an internal
// error, never swallowed.
func audit(ctx context.Context, log AuditLog, actor Actor, e NewAuditEntry) error {
	e.ActorID, e.ActorEmail = actor.AdminID, actor.Email
	if err := log.Append(ctx, e); err != nil {
		return fmt.Errorf("audit %s %s/%s (action applied, not recorded): %w", e.Action, e.TargetType, e.TargetID, err)
	}
	return nil
}
