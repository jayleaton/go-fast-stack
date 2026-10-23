package services

//go:generate go tool mockgen -source=users.go -destination=mocks/users.go -package=mocks

import (
	"context"
	"slices"
	"time"

	"github.com/jayleaton/go-fast-stack/apps/admin/internal/validate"
)

// User is a user of the main app as the dashboard sees it: the Better Auth
// "user" row plus the admin plugin fields. It never carries credentials.
type User struct {
	ID               string     `json:"id"`
	Name             string     `json:"name"`
	Email            string     `json:"email"`
	EmailVerified    bool       `json:"emailVerified"`
	Image            *string    `json:"image"`
	CreatedAt        time.Time  `json:"createdAt"`
	UpdatedAt        time.Time  `json:"updatedAt"`
	Role             string     `json:"role"`
	Banned           bool       `json:"banned"`
	BanReason        *string    `json:"banReason"`
	BanExpires       *time.Time `json:"banExpires"` // nil while banned: permanent
	TwoFactorEnabled bool       `json:"twoFactorEnabled"`
	HasPassword      bool       `json:"hasPassword"`
}

// Session is one of a user's active sign-ins. It never carries the token.
type Session struct {
	ID        string    `json:"id"`
	CreatedAt time.Time `json:"createdAt"`
	ExpiresAt time.Time `json:"expiresAt"`
	IPAddress *string   `json:"ipAddress"`
	UserAgent *string   `json:"userAgent"`
}

// UserDetail is one user with their active sessions, newest first.
type UserDetail struct {
	User     User      `json:"user"`
	Sessions []Session `json:"sessions"`
}

// UserQuery filters, sorts and pages the user list.
type UserQuery struct {
	Search      string
	SearchField string // "email" or "name"
	Role        string // optional
	Banned      *bool  // optional
	SortBy      string // "createdAt", "email" or "name"
	SortDesc    bool
	Limit       int
	Offset      int
}

// UserPage is one page of users.
type UserPage struct {
	Users  []User `json:"users"`
	Total  int    `json:"total"`
	Limit  int    `json:"limit"`
	Offset int    `json:"offset"`
}

// UserDirectory is the port to the main app's users (an adapter over
// auth.UserAdmin in apps/admin/internal/server: packages/auth owns those
// tables, the ban rules and session revocation). Implementations report a
// missing user as ErrUserNotFound, a session that is not the user's as
// ErrSessionNotFound and a role auth rejects as ErrInvalidRole; any other
// error is internal.
type UserDirectory interface {
	ListUsers(ctx context.Context, query UserQuery) (UserPage, error)
	GetUserDetail(ctx context.Context, userID string) (UserDetail, error)
	// BanUser revokes every session of the user; duration 0 is permanent.
	BanUser(ctx context.Context, userID, reason string, duration time.Duration) error
	UnbanUser(ctx context.Context, userID string) error
	SetRole(ctx context.Context, userID, role string) error
	RevokeSession(ctx context.Context, userID, sessionID string) error
	RevokeUserSessions(ctx context.Context, userID string) error
}

// Roles are the app roles an admin may assign (Better Auth admin plugin
// defaults). Add a role here and in the app's own checks together.
var Roles = []string{"user", "admin"}

// Audit actions on app users.
const (
	ActionUserBan            = "user.ban"
	ActionUserUnban          = "user.unban"
	ActionUserSetRole        = "user.set_role"
	ActionUserRevokeSession  = "user.revoke_session"
	ActionUserRevokeSessions = "user.revoke_sessions"
)

// UserService errors. Match them with errors.Is (the UI picks a translation
// per error); the transport maps their Kind to a status.
var (
	ErrUserNotFound    = &Error{KindNotFound, "User not found"}
	ErrSessionNotFound = &Error{KindNotFound, "Session not found"}
	ErrInvalidRole     = &Error{KindInvalid, "Unknown role"}
	ErrRoleUnchanged   = &Error{KindInvalid, "The user already has this role"}
	ErrUserNotBanned   = &Error{KindInvalid, "The user is not banned"}
)

// UserService administers the main app's users: list, inspect, ban, unban,
// set the role and revoke sessions. Every mutation is audited.
type UserService struct {
	dir    UserDirectory
	log    AuditLog
	policy Policy
}

// NewUserService returns a UserService over its ports.
func NewUserService(dir UserDirectory, log AuditLog, policy Policy) *UserService {
	return &UserService{dir: dir, log: log, policy: policy}
}

// ListUsers returns one page of users.
func (service *UserService) ListUsers(ctx context.Context, actor Actor, in validate.ListUsers) (UserPage, error) {
	if err := service.policy.authorize(actor); err != nil {
		return UserPage{}, err
	}
	page, err := service.dir.ListUsers(ctx, UserQuery{
		Search: in.Search, SearchField: in.SearchField, Role: in.Role, Banned: in.Banned,
		SortBy: in.SortBy, SortDesc: in.SortDirection == "desc", Limit: in.Limit, Offset: in.Offset,
	})
	if page.Users == nil {
		page.Users = []User{} // an empty page is [], not null
	}
	return page, err
}

// GetUser returns the user with their active sessions.
func (service *UserService) GetUser(ctx context.Context, actor Actor, userID string) (UserDetail, error) {
	if err := service.policy.authorize(actor); err != nil {
		return UserDetail{}, err
	}
	detail, err := service.dir.GetUserDetail(ctx, userID)
	if err != nil {
		return UserDetail{}, err
	}
	if detail.Sessions == nil {
		detail.Sessions = []Session{}
	}
	return detail, nil
}

// BanUser bans the user (again, with a new reason and expiry, if already
// banned) and signs them out everywhere.
func (service *UserService) BanUser(ctx context.Context, actor Actor, userID string, in validate.BanUser) error {
	user, err := service.target(ctx, actor, userID)
	if err != nil {
		return err
	}
	var duration time.Duration
	if in.BanExpiresIn != nil {
		duration = time.Duration(*in.BanExpiresIn) * time.Second
	}
	expires := "permanent"
	if duration > 0 {
		expires = duration.String()
	}
	return mutate(ctx, service.log, actor, userEntry(ActionUserBan, user, map[string]string{"reason": in.BanReason, "expiresIn": expires}), func() error {
		return service.dir.BanUser(ctx, userID, in.BanReason, duration)
	})
}

// UnbanUser lifts the user's ban.
func (service *UserService) UnbanUser(ctx context.Context, actor Actor, userID string) error {
	user, err := service.target(ctx, actor, userID)
	if err != nil {
		return err
	}
	if !user.Banned {
		return ErrUserNotBanned
	}
	return mutate(ctx, service.log, actor, userEntry(ActionUserUnban, user, nil), func() error {
		return service.dir.UnbanUser(ctx, userID)
	})
}

// SetRole changes the user's app role to one of Roles.
func (service *UserService) SetRole(ctx context.Context, actor Actor, userID string, in validate.SetUserRole) error {
	if err := service.policy.authorize(actor); err != nil {
		return err
	}
	if !slices.Contains(Roles, in.Role) {
		return ErrInvalidRole
	}
	user, err := service.target(ctx, actor, userID)
	if err != nil {
		return err
	}
	if user.Role == in.Role {
		return ErrRoleUnchanged
	}
	return mutate(ctx, service.log, actor, userEntry(ActionUserSetRole, user, map[string]string{"from": user.Role, "to": in.Role}), func() error {
		return service.dir.SetRole(ctx, userID, in.Role)
	})
}

// RevokeSession signs the user out of one session.
func (service *UserService) RevokeSession(ctx context.Context, actor Actor, userID, sessionID string) error {
	user, err := service.target(ctx, actor, userID)
	if err != nil {
		return err
	}
	return mutate(ctx, service.log, actor, userEntry(ActionUserRevokeSession, user, map[string]string{"sessionId": sessionID}), func() error {
		return service.dir.RevokeSession(ctx, userID, sessionID)
	})
}

// RevokeSessions signs the user out everywhere.
func (service *UserService) RevokeSessions(ctx context.Context, actor Actor, userID string) error {
	user, err := service.target(ctx, actor, userID)
	if err != nil {
		return err
	}
	return mutate(ctx, service.log, actor, userEntry(ActionUserRevokeSessions, user, nil), func() error {
		return service.dir.RevokeUserSessions(ctx, userID)
	})
}

// target authorizes the actor and loads the user a mutation acts on, so a
// missing user is a 404 before anything changes and the audit entry can name
// the user's email.
func (service *UserService) target(ctx context.Context, actor Actor, userID string) (User, error) {
	if err := service.policy.authorize(actor); err != nil {
		return User{}, err
	}
	detail, err := service.dir.GetUserDetail(ctx, userID)
	return detail.User, err
}

func userEntry(action string, user User, details map[string]string) NewAuditEntry {
	return NewAuditEntry{Action: action, TargetType: TargetUser, TargetID: user.ID, TargetLabel: user.Email, Details: details}
}
