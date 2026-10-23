// Package apiclient is the admin JSON API as the admin UI calls it: the
// Client interface, one method per endpoint, and the types and errors those
// methods take and return. It is the only app package apps/admin/ui may
// import for data (depguard): the UI never reaches services, repositories
// or the databases, only the API, so every read and write passes through
// the API's access gate, validation and error mapping.
//
// api.Client is the implementation: it runs each api.Endpoint in-process
// (no network hop; cmd/admin does not even mount the JSON API, and browsers
// only ever talk to the UI). A client over HTTP would implement the same
// interface against the same endpoints and map the envelope back to these
// errors.
//
// The types are aliases, so the UI and the API share one definition.
package apiclient

import (
	"context"

	"github.com/jayleaton/go-fast-stack/apps/admin/internal/access"
	"github.com/jayleaton/go-fast-stack/apps/admin/internal/services"
	"github.com/jayleaton/go-fast-stack/apps/admin/internal/validate"
)

// Client is the admin API (/api/admin/v1). The caller is the request the
// ctx belongs to; the API's access policy decides whether they may call
// (access.ErrSignedOut, access.ErrDenied), and every service then applies
// services.Policy (2FA when required: ErrTwoFactorRequired).
type Client interface {
	ListUsers(ctx context.Context, input ListUsers) (UserPage, error)
	GetUser(ctx context.Context, userID string) (UserDetail, error)
	BanUser(ctx context.Context, userID string, input BanUser) error
	UnbanUser(ctx context.Context, userID string) error
	SetRole(ctx context.Context, userID string, input SetUserRole) error
	RevokeSessions(ctx context.Context, userID string) error
	RevokeSession(ctx context.Context, userID, sessionID string) error

	ListAdmins(ctx context.Context) ([]Admin, error)
	InviteAdmin(ctx context.Context, input CreateAdminInvite) (Admin, error)
	DisableAdmin(ctx context.Context, adminID string) error
	EnableAdmin(ctx context.Context, adminID string) error
	RemoveAdmin(ctx context.Context, adminID string) error
	SendPasswordReset(ctx context.Context, adminID string) error
	ResetTwoFactor(ctx context.Context, adminID string) error

	ListAuditLog(ctx context.Context, input ListAuditLog) (AuditPage, error)
	Overview(ctx context.Context) (Overview, error)

	// Sign-in flows. Results carry the cookies the UI server action sets
	// (setCookies): the session, the two-factor challenge, the trusted
	// device. The browser never sees the API.
	//
	// SignIn and SetPassword return a *TwoFactorChallenge AS THE ERROR when
	// a second factor (or enrolment) is needed: set its Cookies and send the
	// browser to the matching page.
	SignIn(ctx context.Context, input SignIn) (AuthResult, error)
	// SignOut ends the request's session (if any) and clears its cookie.
	SignOut(ctx context.Context) (AuthResult, error)
	SetPassword(ctx context.Context, input SetPassword) (AuthResult, error)
	// RequestPasswordReset answers nil whether or not email is an admin.
	RequestPasswordReset(ctx context.Context, input CreatePasswordReset) error
	// VerifyTwoFactor finishes sign-in (or SetupRequired enrolment) for the
	// challenge in the request's cookie.
	VerifyTwoFactor(ctx context.Context, input VerifyTwoFactor) (AuthResult, error)
	// StartTwoFactorSetup enrols the SetupRequired challenge in the cookie.
	StartTwoFactorSetup(ctx context.Context) (TOTPSetup, error)
	// EnableTwoFactor and ConfirmTwoFactor are optional enrolment for the
	// signed-in admin.
	EnableTwoFactor(ctx context.Context, input EnableTwoFactor) (TOTPSetup, error)
	ConfirmTwoFactor(ctx context.Context, input ConfirmTwoFactor) error
}

// Data the API returns.
type (
	User       = services.User
	Session    = services.Session // one of a user's sessions (UserDetail)
	UserDetail = services.UserDetail
	UserPage   = services.UserPage
	Admin      = services.Admin
	AuditEntry = services.AuditEntry
	AuditPage  = services.AuditPage
	Overview   = services.Overview
)

// Cookie is a cookie for the browser, as data: the UI sets it with Path=/,
// HttpOnly and SameSite=Lax. MaxAge is in seconds; negative deletes it.
type Cookie struct {
	Name   string `json:"name"`
	Value  string `json:"value"`
	MaxAge int    `json:"maxAge"`
	Secure bool   `json:"secure"`
}

// AuthResult is the outcome of a sign-in flow step: whether a session was
// created, and the cookies for the UI's response (never serialized: they
// carry credentials).
type AuthResult struct {
	SignedIn bool     `json:"signedIn"`
	Cookies  []Cookie `json:"-"`
}

// TwoFactorChallenge is returned AS THE ERROR when a second factor (or,
// with SetupRequired, TOTP enrolment) is needed; no session exists yet.
// Cookies carry the challenge for the next step.
type TwoFactorChallenge struct {
	Methods       []string
	SetupRequired bool
	Cookies       []Cookie
}

func (challenge *TwoFactorChallenge) Error() string {
	if challenge.SetupRequired {
		return "two-factor setup required"
	}
	return "two-factor verification required"
}

// TOTPSetup is a fresh TOTP secret, its otpauth:// URI and the one-time
// backup codes. Shown once.
type TOTPSetup struct {
	Secret, URI string
	BackupCodes []string
}

// Two-factor methods of VerifyTwoFactor.Method.
const (
	MethodTOTP       = validate.MethodTOTP
	MethodBackupCode = validate.MethodBackupCode
)

// Auth failures of the sign-in flows (the server adapter translates
// packages/auth's errors into these, keeping the original in the chain).
var (
	ErrInvalidCredentials = &services.Error{Kind: services.KindUnauthorized, Message: "Invalid email or password"}
	ErrInvalidCode        = &services.Error{Kind: services.KindInvalid, Message: "Invalid two-factor code"}
	// ErrInvalidToken: a bad or expired set-password link.
	ErrInvalidToken   = &services.Error{Kind: services.KindInvalid, Message: "Invalid or expired link"}
	ErrPasswordPolicy = &services.Error{Kind: services.KindInvalid, Message: "Password does not meet the policy"}
	// ErrInvalidPassword: wrong current password when enabling 2FA.
	ErrInvalidPassword = &services.Error{Kind: services.KindInvalid, Message: "Invalid password"}
	// ErrChallengeExpired: the two-factor cookie is missing or expired.
	ErrChallengeExpired = &services.Error{Kind: services.KindUnauthorized, Message: "Two-factor challenge expired"}
	ErrTooManyRequests  = &services.Error{Kind: services.KindRateLimited, Message: "Too many requests. Please try again later."}
)

// Inputs, validated by the API (Validate runs there, not in the UI). The
// list inputs fill themselves from a query string (DecodeQuery).
type (
	ListUsers           = validate.ListUsers
	BanUser             = validate.BanUser
	SetUserRole         = validate.SetUserRole
	CreateAdminInvite   = validate.CreateAdminInvite
	ListAuditLog        = validate.ListAuditLog
	ValidationError     = validate.Error
	ValidationFields    = validate.Fields
	SignIn              = validate.SignIn
	SetPassword         = validate.SetPassword
	CreatePasswordReset = validate.CreatePasswordReset
	VerifyTwoFactor     = validate.VerifyTwoFactor
	EnableTwoFactor     = validate.EnableTwoFactor
	ConfirmTwoFactor    = validate.ConfirmTwoFactor
)

// Roles a user may be given (SetUserRole).
var Roles = services.Roles

// Error kinds, for a page's status code (KindOf).
type Kind = services.Kind

// The kinds a page maps to a status.
const (
	KindInvalid   = services.KindInvalid
	KindForbidden = services.KindForbidden
	KindNotFound  = services.KindNotFound
	KindConflict  = services.KindConflict
)

// KindOf is the kind of an API error (KindInternal for unknown errors).
var KindOf = services.KindOf

// ServiceError is a business-rule failure whose Message is safe to show.
type ServiceError = services.Error

// ReasonInternal is the failure reason recorded for an unexpected error.
const ReasonInternal = services.ReasonInternal

// Errors the API returns, matched with errors.Is. The kind sentinels
// (ErrNotFound...) match every error of their kind.
var (
	ErrUserNotFound      = services.ErrUserNotFound
	ErrSessionNotFound   = services.ErrSessionNotFound
	ErrInvalidRole       = services.ErrInvalidRole
	ErrRoleUnchanged     = services.ErrRoleUnchanged
	ErrUserNotBanned     = services.ErrUserNotBanned
	ErrAdminExists       = services.ErrAdminExists
	ErrAdminNotFound     = services.ErrAdminNotFound
	ErrAdminSelf         = services.ErrAdminSelf
	ErrLastAdmin         = services.ErrLastAdmin
	ErrAdminDisabled     = services.ErrAdminDisabled
	ErrAdminNotDisabled  = services.ErrAdminNotDisabled
	ErrAdminPending      = services.ErrAdminPending
	ErrAdminNoTwoFactor  = services.ErrAdminNoTwoFactor
	ErrAdminSelfReset    = services.ErrAdminSelfReset
	ErrTwoFactorRequired = services.ErrTwoFactorRequired
	ErrAuditUnavailable  = services.ErrAuditUnavailable
	ErrValidation        = services.ErrValidation
	ErrForbidden         = services.ErrForbidden
	ErrNotFound          = services.ErrNotFound
	ErrConflict          = services.ErrConflict
)

// Audit log vocabulary the audit page renders.
const (
	StatusPending   = services.StatusPending
	StatusSucceeded = services.StatusSucceeded
	StatusFailed    = services.StatusFailed
	StatusUnknown   = services.StatusUnknown

	TargetUser  = services.TargetUser
	TargetAdmin = services.TargetAdmin

	SignInPassword    = services.SignInPassword
	SignInTwoFactor   = services.SignInTwoFactor
	SignInSetPassword = services.SignInSetPassword

	ActionAdminBackupCodeUsed  = services.ActionAdminBackupCodeUsed
	ActionAdminBootstrap       = services.ActionAdminBootstrap
	ActionAdminDisable         = services.ActionAdminDisable
	ActionAdminEnable          = services.ActionAdminEnable
	ActionAdminInvite          = services.ActionAdminInvite
	ActionAdminPasswordReset   = services.ActionAdminPasswordReset
	ActionAdminRemove          = services.ActionAdminRemove
	ActionAdminResetTwoFactor  = services.ActionAdminResetTwoFactor
	ActionAdminSignIn          = services.ActionAdminSignIn
	ActionAdminSignOut         = services.ActionAdminSignOut
	ActionAdminTwoFactorFailed = services.ActionAdminTwoFactorFailed
	ActionUserBan              = services.ActionUserBan
	ActionUserRevokeSession    = services.ActionUserRevokeSession
	ActionUserRevokeSessions   = services.ActionUserRevokeSessions
	ActionUserSetRole          = services.ActionUserSetRole
	ActionUserUnban            = services.ActionUserUnban
)

// Route access, as the UI declares it on its own pages (the same policies
// and gate as the API's endpoints).
type (
	Caller        = access.Caller
	CallerAdmin   = access.Admin
	CallerSession = access.Session
	Policy        = access.Policy
	Gate          = access.Gate
)

// The shipped policies and access errors (docs/route-access.md).
var (
	Public       = access.Public
	SignedIn     = access.SignedIn
	ErrSignedOut = access.ErrSignedOut
	ErrDenied    = access.ErrDenied
)
