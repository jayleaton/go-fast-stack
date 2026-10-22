// ci timing probe
// Package services holds business rules. Both transports (the JSON API and
// the HTML UI) call services in-process. Services see everything below them
// through ports: small interfaces declared here, by the consumer, and
// implemented by repositories (packages/database) or adapters
// (apps/web/internal/server). Services never import net/http, templ, pgx,
// sqlc output or packages/auth.
//
// UserService (users.go) is the reference: copy it for a new service.
package services

import (
	"errors"

	"github.com/jayleaton/go-fast-stack/packages/database/model"
)

// Actor is the signed-in user a service call acts for, and the session the
// request came in on. Transports build it from the caller; services decide
// what the actor may do.
type Actor struct {
	UserID    string
	Email     string
	SessionID string
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
// Services may also return them as they are.
var (
	ErrValidation = &Error{Kind: KindInvalid, Message: "Invalid request"}
	ErrForbidden  = &Error{Kind: KindForbidden, Message: "Forbidden"}
	ErrNotFound   = &Error{Kind: KindNotFound, Message: "Not found"}
	ErrConflict   = &Error{Kind: KindConflict, Message: "Conflict"}
	// ErrRateLimited is the one rate-limit error; there is nothing more specific to match.
	ErrRateLimited = &Error{Kind: KindRateLimited, Message: "Too many requests. Please try again later."}
)

// Is makes the kind sentinels match every error of their kind. Specific
// errors (ErrEmailTaken...) match only themselves.
func (e *Error) Is(target error) bool {
	switch target {
	case ErrValidation, ErrForbidden, ErrNotFound, ErrConflict:
		return e.Kind == target.(*Error).Kind
	}
	return false
}

// ErrRecordNotFound is what repository ports return for a missing row
// (model.ErrNotFound, re-exported so service tests need not import packages/database).
var ErrRecordNotFound = model.ErrNotFound

// KindOf returns the Kind of err (KindInternal for unknown errors).
func KindOf(err error) Kind {
	var se *Error
	if errors.As(err, &se) {
		return se.Kind
	}
	return KindInternal
}

// notFound maps a repository miss to notFoundErr and passes others through.
func notFound(err error, notFoundErr *Error) error {
	if errors.Is(err, ErrRecordNotFound) {
		return notFoundErr
	}
	return err
}
