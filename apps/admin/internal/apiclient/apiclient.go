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

// Inputs, validated by the API (Validate runs there, not in the UI). The
// list inputs fill themselves from a query string (DecodeQuery).
type (
	ListUsers         = validate.ListUsers
	BanUser           = validate.BanUser
	SetUserRole       = validate.SetUserRole
	CreateAdminInvite = validate.CreateAdminInvite
	ListAuditLog      = validate.ListAuditLog
	ValidationError   = validate.Error
	ValidationFields  = validate.Fields
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
