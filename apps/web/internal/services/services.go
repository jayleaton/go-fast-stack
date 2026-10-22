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

	"github.com/jayleaton/ai-patterns-go/packages/database/model"
)

// ErrNotFound is what repository ports return for a missing row
// (model.ErrNotFound, re-exported for implementations and mocks).
var ErrNotFound = model.ErrNotFound

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
)

// Error is a business-rule failure whose Message is safe to show to callers.
type Error struct {
	Kind    Kind
	Message string
}

func (e *Error) Error() string { return e.Message }

func fail(kind Kind, msg string) *Error { return &Error{Kind: kind, Message: msg} }

// KindOf returns the Kind of err (KindInternal for unknown errors).
func KindOf(err error) Kind {
	var se *Error
	if errors.As(err, &se) {
		return se.Kind
	}
	return KindInternal
}

// notFound maps a repository miss to a service error and passes others through.
func notFound(err error, msg string) error {
	if errors.Is(err, ErrNotFound) {
		return fail(KindNotFound, msg)
	}
	return err
}
