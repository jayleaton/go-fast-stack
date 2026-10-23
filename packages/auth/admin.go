package auth

import (
	"context"
	"errors"
	"log/slog"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// UserAdmin manages users the way Better Auth's admin plugin does: list and
// search, inspect, ban, unban, set roles and revoke sessions. It needs no
// secret or cookies, so an admin app can run one over the app's database.
// It does not check who is calling: authorisation belongs to the caller.
type UserAdmin struct {
	store *store
	now   func() time.Time
	emit  func(ctx context.Context, event Event) // nil: no events
	sink  *eventSink                             // where emit queues (drained by Shutdown)
}

// NewUserAdmin returns a UserAdmin over pool (a database migrated by Migrate).
func NewUserAdmin(pool DB) *UserAdmin {
	return &UserAdmin{store: &store{pool: pool}, now: time.Now}
}

// Users returns a UserAdmin over this instance's database.
func (auth *Auth) Users() *UserAdmin {
	return &UserAdmin{store: auth.store, now: auth.now, emit: auth.emit, sink: auth.events}
}

// WithEvents returns a copy of u that reports bans, unbans, session
// revocations and 2FA resets to h, with the same delivery guarantees as
// Config.Events. Call it once at startup and keep the result: each call
// starts its own delivery worker. A UserAdmin from Auth.Users already reports
// to that instance's Config.Events. Events carry no request metadata here (a
// standalone UserAdmin has no request context of its own).
func (admin *UserAdmin) WithEvents(handler EventHandler) *UserAdmin {
	sink := newEventSink(handler, slog.Default(), admin.store.userEmail)
	userAdmin := *admin
	userAdmin.sink = sink
	userAdmin.emit = func(ctx context.Context, event Event) {
		event.At = userAdmin.now()
		sink.emit(ctx, event)
	}
	return &userAdmin
}

// DroppedEvents is how many of this UserAdmin's events were dropped because
// the queue was full (see Auth.DroppedEvents).
func (admin *UserAdmin) DroppedEvents() int64 { return admin.sink.droppedCount() }

// Shutdown waits for this UserAdmin's queued events to be delivered (or ctx).
// Call it on graceful shutdown after the last admin action.
func (admin *UserAdmin) Shutdown(ctx context.Context) error {
	return admin.sink.drain(ctx)
}

func (admin *UserAdmin) event(ctx context.Context, event Event) {
	if admin.emit != nil {
		admin.emit(ctx, event)
	}
}

func (admin *UserAdmin) revoked(ctx context.Context, userID, sessionID string, count int64, by string) {
	if count > 0 {
		admin.event(ctx, Event{Kind: EventSessionRevoked, UserID: userID, SessionID: sessionID,
			Details: map[string]string{"count": strconv.FormatInt(count, 10), "by": by}})
	}
}

// AdminUser is a user with the admin plugin's fields.
type AdminUser struct {
	User
	Role             string     `json:"role"`
	Banned           bool       `json:"banned"`
	BanReason        *string    `json:"banReason"`
	BanExpires       *time.Time `json:"banExpires"` // nil: permanent
	TwoFactorEnabled bool       `json:"twoFactorEnabled"`
	HasPassword      bool       `json:"hasPassword"`
}

// Page sizes for ListUsersQuery.Limit.
const (
	defaultUserListLimit = 25  // when Limit is unset
	maxUserListLimit     = 100 // larger Limits are capped to this
)

// ListUsersQuery selects a page of users.
type ListUsersQuery struct {
	Search      string // case-insensitive "contains"
	SearchField string // "email" (default) or "name"
	Role        string // only users with this role
	Banned      *bool  // only banned / not banned users
	SortBy      string // "createdAt" (default), "email" or "name"
	SortDesc    bool
	Limit       int // default 25, max 100
	Offset      int
}

// UserPage is one page of ListUsers.
type UserPage struct {
	Users  []AdminUser `json:"users"`
	Total  int         `json:"total"`
	Limit  int         `json:"limit"`
	Offset int         `json:"offset"`
}

// UserDetail is a user with their active sessions (newest first, no tokens).
type UserDetail struct {
	User     AdminUser `json:"user"`
	Sessions []Session `json:"sessions"`
}

const adminUserColumns = userColumns + `, u."role", u."banned", u."banReason", u."banExpires", u."twoFactorEnabled",
	EXISTS (SELECT 1 FROM "account" a WHERE a."userId" = u."id" AND a."providerId" = 'credential' AND a."password" IS NOT NULL)`

func scanAdminUser(row pgx.Row, extra ...any) (AdminUser, error) {
	var user AdminUser
	dest := append([]any{&user.Role, &user.Banned, &user.BanReason, &user.BanExpires, &user.TwoFactorEnabled, &user.HasPassword}, extra...)
	scanned, err := scanUser(row, dest...)
	user.User = scanned
	return user, err
}

var sortColumns = map[string]string{"": `u."createdAt"`, "createdAt": `u."createdAt"`, "email": `u."email"`, "name": `u."name"`}

// likeEscaper escapes LIKE wildcards so a search is always a literal substring.
var likeEscaper = strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)

// ListUsers returns one page of users matching q, with the total match count.
func (admin *UserAdmin) ListUsers(ctx context.Context, query ListUsersQuery) (UserPage, error) {
	order, ok := sortColumns[query.SortBy]
	if !ok {
		order = sortColumns[""]
	}
	dir := "ASC"
	if query.SortDesc {
		dir = "DESC"
	}
	field := `u."email"`
	if query.SearchField == "name" {
		field = `u."name"`
	}
	if query.Limit <= 0 {
		query.Limit = defaultUserListLimit
	}
	query.Limit = min(query.Limit, maxUserListLimit)
	query.Offset = max(query.Offset, 0)

	where := []string{"TRUE"}
	args := []any{}
	arg := func(value any) string {
		args = append(args, value)
		return "$" + strconv.Itoa(len(args))
	}
	if query.Search != "" {
		where = append(where, field+` ILIKE `+arg("%"+likeEscaper.Replace(query.Search)+"%"))
	}
	if query.Role != "" {
		where = append(where, `u."role" = `+arg(query.Role))
	}
	if query.Banned != nil {
		where = append(where, `u."banned" = `+arg(*query.Banned))
	}
	sql := `SELECT ` + adminUserColumns + `, count(*) OVER () FROM "user" u WHERE ` + strings.Join(where, " AND ") +
		` ORDER BY ` + order + ` ` + dir + `, u."id" ` + dir + ` LIMIT ` + arg(query.Limit) + ` OFFSET ` + arg(query.Offset)

	rows, err := admin.store.pool.Query(ctx, sql, args...)
	if err != nil {
		return UserPage{}, err
	}
	page := UserPage{Users: []AdminUser{}, Limit: query.Limit, Offset: query.Offset}
	var total int
	users, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (AdminUser, error) {
		return scanAdminUser(row, &total)
	})
	if err != nil {
		return UserPage{}, err
	}
	if len(users) > 0 {
		page.Users, page.Total = users, total
	} else if query.Offset > 0 {
		// Past the end: count separately so the caller can still page back.
		if err := admin.store.pool.QueryRow(ctx, `SELECT count(*) FROM "user" u WHERE `+strings.Join(where, " AND "), args[:len(args)-2]...).Scan(&page.Total); err != nil {
			return UserPage{}, err
		}
	}
	return page, nil
}

func (admin *UserAdmin) adminUser(ctx context.Context, userID string) (AdminUser, error) {
	user, err := scanAdminUser(admin.store.pool.QueryRow(ctx, `SELECT `+adminUserColumns+` FROM "user" u WHERE u."id" = $1`, userID))
	if errors.Is(err, errNotFound) {
		return AdminUser{}, ErrUserNotFound
	}
	return user, err
}

// GetUserDetail returns a user with their active sessions.
func (admin *UserAdmin) GetUserDetail(ctx context.Context, userID string) (UserDetail, error) {
	user, err := admin.adminUser(ctx, userID)
	if err != nil {
		return UserDetail{}, err
	}
	sessions, err := admin.store.listSessions(ctx, userID, admin.now())
	if err != nil {
		return UserDetail{}, err
	}
	return UserDetail{User: user, Sessions: sessions}, nil
}

// BanUser bans a user for duration (0: until UnbanUser) and revokes all their
// sessions. Banned users cannot sign in and their sessions stop working.
func (admin *UserAdmin) BanUser(ctx context.Context, userID, reason string, duration time.Duration) error {
	if duration < 0 {
		return errors.New("auth: negative ban duration")
	}
	now := admin.now()
	var expires *time.Time
	if duration > 0 {
		moment := now.Add(duration)
		expires = &moment
	}
	var why *string
	if reason = strings.TrimSpace(reason); reason != "" {
		why = &reason
	}
	var revoked int64
	err := admin.store.tx(ctx, func(querier querier) error {
		tag, err := querier.Exec(ctx, `
			UPDATE "user" SET "banned" = true, "banReason" = $2, "banExpires" = $3, "updatedAt" = $4 WHERE "id" = $1`,
			userID, why, expires, now)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return ErrUserNotFound
		}
		revoked, err = admin.store.deleteUserSessionsCount(ctx, querier, userID)
		return err
	})
	if err != nil {
		return err
	}
	details := map[string]string{}
	if reason != "" {
		details["reason"] = reason
	}
	if expires != nil {
		details["expiresAt"] = expires.UTC().Format(time.RFC3339)
	}
	admin.event(ctx, Event{Kind: EventUserBanned, UserID: userID, Details: details})
	admin.revoked(ctx, userID, "", revoked, "ban")
	return nil
}

// UnbanUser lifts a ban; the user can sign in again.
func (admin *UserAdmin) UnbanUser(ctx context.Context, userID string) error {
	tag, err := admin.store.pool.Exec(ctx, `
		UPDATE "user" SET "banned" = false, "banReason" = NULL, "banExpires" = NULL, "updatedAt" = $2 WHERE "id" = $1`,
		userID, admin.now())
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrUserNotFound
	}
	admin.event(ctx, Event{Kind: EventUserUnbanned, UserID: userID})
	return nil
}

// validRole accepts Better Auth role strings: one or more comma-separated names.
var validRole = regexp.MustCompile(`^[A-Za-z0-9_-]{1,32}(,[A-Za-z0-9_-]{1,32}){0,7}$`)

// SetRole sets a user's role (e.g. "user", "admin"; comma-separated for several).
func (admin *UserAdmin) SetRole(ctx context.Context, userID, role string) error {
	if !validRole.MatchString(role) {
		return ErrInvalidRole
	}
	tag, err := admin.store.pool.Exec(ctx, `UPDATE "user" SET "role" = $2, "updatedAt" = $3 WHERE "id" = $1`, userID, role, admin.now())
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrUserNotFound
	}
	return nil
}

// RevokeSession signs out one session of userID (ErrSessionNotFound otherwise).
func (admin *UserAdmin) RevokeSession(ctx context.Context, userID, sessionID string) error {
	if sessionID == "" {
		return ErrSessionNotFound
	}
	id, err := admin.store.deleteUserSession(ctx, userID, sessionID, "")
	if errors.Is(err, errNotFound) {
		return ErrSessionNotFound
	}
	if err != nil {
		return err
	}
	admin.revoked(ctx, userID, id, 1, "admin")
	return nil
}

// ResetTwoFactor removes a user's 2FA enrolment, trusted devices and pending
// 2FA sign-ins, for account recovery (lost authenticator and backup codes, or
// a rotated auth secret). With TwoFactorConfig.Required the user enrols again
// at their next sign-in.
func (admin *UserAdmin) ResetTwoFactor(ctx context.Context, userID string) error {
	if _, err := admin.adminUser(ctx, userID); err != nil {
		return err
	}
	err := admin.store.tx(ctx, func(querier querier) error {
		if err := admin.store.deleteChallenges(ctx, querier, userID); err != nil {
			return err
		}
		if _, err := querier.Exec(ctx, `DELETE FROM "twoFactor" WHERE "userId" = $1`, userID); err != nil {
			return err
		}
		if _, err := querier.Exec(ctx, `UPDATE "user" SET "twoFactorEnabled" = false, "updatedAt" = $2 WHERE "id" = $1`, userID, admin.now()); err != nil {
			return err
		}
		return admin.store.deleteTrustedDevices(ctx, querier, userID)
	})
	if err == nil {
		admin.event(ctx, Event{Kind: EventTwoFactorDisabled, UserID: userID, Details: map[string]string{"by": "admin"}})
	}
	return err
}

// RevokeUserSessions signs out every session of userID.
func (admin *UserAdmin) RevokeUserSessions(ctx context.Context, userID string) error {
	count, err := admin.store.deleteUserSessionsCount(ctx, admin.store.pool, userID)
	if err == nil {
		admin.revoked(ctx, userID, "", count, "admin")
	}
	return err
}
