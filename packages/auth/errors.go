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

func (err *Error) Error() string { return err.Message }

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
	ErrSessionNotFresh           = &Error{http.StatusForbidden, "SESSION_NOT_FRESH", "Session is not fresh"}
	ErrInvalidUser               = &Error{http.StatusBadRequest, "INVALID_USER", "Invalid user"}

	// Admin plugin.
	ErrBannedUser     = &Error{http.StatusForbidden, "BANNED_USER", "You have been banned from this application. Please contact support if you believe this is an error."}
	ErrInvalidRole    = &Error{http.StatusBadRequest, "INVALID_ROLE_TYPE", "Invalid role type"}
	ErrSignUpDisabled = &Error{http.StatusBadRequest, "EMAIL_PASSWORD_SIGN_UP_DISABLED", "Email and password sign up is not enabled"}

	// Two-factor plugin.
	ErrInvalidCode            = &Error{http.StatusUnauthorized, "INVALID_CODE", "Invalid code"}
	ErrInvalidBackupCode      = &Error{http.StatusUnauthorized, "INVALID_BACKUP_CODE", "Invalid backup code"}
	ErrInvalidTwoFactorCookie = &Error{http.StatusUnauthorized, "INVALID_TWO_FACTOR_COOKIE", "Invalid two factor cookie"}
	ErrTwoFactorNotEnabled    = &Error{http.StatusBadRequest, "TWO_FACTOR_NOT_ENABLED", "Two factor isn't enabled"}
	ErrTOTPAlreadyEnabled     = &Error{http.StatusBadRequest, "TOTP_ALREADY_ENABLED", "TOTP is already enabled"}
	ErrTwoFactorLocked        = &Error{http.StatusTooManyRequests, "ACCOUNT_TEMPORARILY_LOCKED", "Too many failed verification attempts. Your account is temporarily locked. Please try again later."}
)

// RateLimitError is returned when a client exceeded a rate limit. It matches
// ErrTooManyRequests with errors.Is.
type RateLimitError struct {
	RetryAfter time.Duration
}

func (rateLimitError *RateLimitError) Error() string        { return ErrTooManyRequests.Message }
func (rateLimitError *RateLimitError) Is(target error) bool { return target == ErrTooManyRequests }

// asError maps err to an *Error, or nil when it is not an auth error.
func asError(err error) *Error {
	if authErr, ok := errors.AsType[*Error](err); ok {
		return authErr
	}
	if errors.Is(err, ErrTooManyRequests) {
		return ErrTooManyRequests
	}
	return nil
}
