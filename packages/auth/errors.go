package auth

import (
	"errors"
	"net/http"
	"time"
)

// Error is an auth failure with Better Auth's error code, message and HTTP
// status. Compare with errors.Is against the Err* values; the JSON endpoints
// render it as {"code": ..., "message": ...}.
type Error struct {
	Status  int
	Code    string
	Message string
}

func (e *Error) Error() string { return e.Message }

// Errors returned by the service methods. Codes and messages match Better Auth.
var (
	ErrInvalidEmail           = &Error{http.StatusBadRequest, "INVALID_EMAIL", "Invalid email"}
	ErrInvalidEmailOrPassword = &Error{http.StatusUnauthorized, "INVALID_EMAIL_OR_PASSWORD", "Invalid email or password"}
	ErrPasswordTooShort       = &Error{http.StatusBadRequest, "PASSWORD_TOO_SHORT", "Password too short"}
	ErrPasswordTooLong        = &Error{http.StatusBadRequest, "PASSWORD_TOO_LONG", "Password too long"}
	ErrUserAlreadyExists      = &Error{http.StatusUnprocessableEntity, "USER_ALREADY_EXISTS_USE_ANOTHER_EMAIL", "User already exists. Use another email."}
	ErrEmailNotVerified       = &Error{http.StatusForbidden, "EMAIL_NOT_VERIFIED", "Email not verified"}
	ErrEmailMismatch          = &Error{http.StatusBadRequest, "EMAIL_MISMATCH", "Email mismatch"}
	ErrEmailAlreadyVerified   = &Error{http.StatusBadRequest, "EMAIL_ALREADY_VERIFIED", "Email is already verified"}
	ErrInvalidToken           = &Error{http.StatusBadRequest, "INVALID_TOKEN", "Invalid token"}
	ErrTokenExpired           = &Error{http.StatusUnauthorized, "TOKEN_EXPIRED", "Token expired"}
	ErrUserNotFound           = &Error{http.StatusBadRequest, "USER_NOT_FOUND", "User not found"}
	ErrInvalidOrigin          = &Error{http.StatusForbidden, "INVALID_ORIGIN", "Invalid origin"}
	ErrMissingOrigin          = &Error{http.StatusForbidden, "MISSING_OR_NULL_ORIGIN", "Missing or null Origin"}
	ErrCrossSiteNavigation    = &Error{http.StatusForbidden, "CROSS_SITE_NAVIGATION_LOGIN_BLOCKED", "Cross-site navigation login blocked. This request appears to be a CSRF attack."}
	ErrInvalidCallbackURL     = &Error{http.StatusForbidden, "INVALID_CALLBACK_URL", "Invalid callbackURL"}
	ErrInvalidRedirectURL     = &Error{http.StatusForbidden, "INVALID_REDIRECT_URL", "Invalid redirectURL"}
	ErrUnauthorized           = &Error{http.StatusUnauthorized, "UNAUTHORIZED", "Unauthorized"}
	ErrTooManyRequests        = &Error{http.StatusTooManyRequests, "TOO_MANY_REQUESTS", "Too many requests. Please try again later."}

	// Account management (change password / email, sessions).
	ErrInvalidPassword           = &Error{http.StatusBadRequest, "INVALID_PASSWORD", "Invalid password"}
	ErrCredentialAccountNotFound = &Error{http.StatusBadRequest, "CREDENTIAL_ACCOUNT_NOT_FOUND", "Credential account not found"}
	ErrEmailTaken                = &Error{http.StatusUnprocessableEntity, "USER_ALREADY_EXISTS_USE_ANOTHER_EMAIL", "User already exists. Use another email."}
	ErrEmailUnchanged            = &Error{http.StatusBadRequest, "EMAIL_IS_THE_SAME", "Email is the same"}
	ErrSessionNotFound           = &Error{http.StatusNotFound, "SESSION_NOT_FOUND", "Session not found"}
)

// RateLimitError is returned when a client exceeded a rate limit. It matches
// ErrTooManyRequests with errors.Is.
type RateLimitError struct {
	RetryAfter time.Duration
}

func (e *RateLimitError) Error() string        { return ErrTooManyRequests.Message }
func (e *RateLimitError) Is(target error) bool { return target == ErrTooManyRequests }

// asError maps err to an *Error, or nil when it is not an auth error.
func asError(err error) *Error {
	if e, ok := errors.AsType[*Error](err); ok {
		return e
	}
	if errors.Is(err, ErrTooManyRequests) {
		return ErrTooManyRequests
	}
	return nil
}
