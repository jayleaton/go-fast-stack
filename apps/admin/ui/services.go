package ui

import (
	"context"

	"github.com/jayleaton/go-fast-stack/apps/admin/internal/services"
	"github.com/jayleaton/go-fast-stack/apps/admin/internal/validate"
)

// Users is the user service the users pages call (*services.UserService).
type Users interface {
	ListUsers(ctx context.Context, actor services.Actor, in validate.ListUsers) (services.UserPage, error)
	GetUser(ctx context.Context, actor services.Actor, userID string) (services.UserDetail, error)
	BanUser(ctx context.Context, actor services.Actor, userID string, in validate.BanUser) error
	UnbanUser(ctx context.Context, actor services.Actor, userID string) error
	SetRole(ctx context.Context, actor services.Actor, userID string, in validate.SetUserRole) error
	RevokeSession(ctx context.Context, actor services.Actor, userID, sessionID string) error
	RevokeSessions(ctx context.Context, actor services.Actor, userID string) error
}

// Admins is the admin service the admins page calls (*services.AdminService).
type Admins interface {
	ListAdmins(ctx context.Context, actor services.Actor) ([]services.Admin, error)
	InviteAdmin(ctx context.Context, actor services.Actor, in validate.CreateAdminInvite) (services.Admin, error)
	DisableAdmin(ctx context.Context, actor services.Actor, adminID string) error
	EnableAdmin(ctx context.Context, actor services.Actor, adminID string) error
	RemoveAdmin(ctx context.Context, actor services.Actor, adminID string) error
}

// Audit is the audit log service (*services.AuditService).
type Audit interface {
	List(ctx context.Context, actor services.Actor, in validate.ListAuditLog) (services.AuditPage, error)
}

// Analytics is the analytics placeholder (*services.AnalyticsService).
type Analytics interface {
	Overview(ctx context.Context, actor services.Actor) (services.Overview, error)
}

// The services plug in without adapters.
var (
	_ Users     = (*services.UserService)(nil)
	_ Admins    = (*services.AdminService)(nil)
	_ Audit     = (*services.AuditService)(nil)
	_ Analytics = (*services.AnalyticsService)(nil)
)
