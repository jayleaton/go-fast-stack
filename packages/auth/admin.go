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
	emit  func(ctx context.Context, e Event) // nil: no events
	sink  *eventSink                         // where emit queues (drained by Shutdown)
}

// NewUserAdmin returns a UserAdmin over pool (a database migrated by Migrate).
func NewUserAdmin(pool DB) *UserAdmin {
	return &UserAdmin{store: &store{pool: pool}, now: time.Now}
}

// Users returns a UserAdmin over this instance's database.
func (a *Auth) Users() *UserAdmin {
	return &UserAdmin{store: a.store, now: a.now, emit: a.emit, sink: a.events}
}

// WithEvents returns a copy of u that reports bans, unbans, session
// revocations and 2FA resets to h, with the same delivery guarantees as
// Config.Events. Call it once at startup and keep the result: each call
// starts its own delivery worker. A UserAdmin from Auth.Users already reports
// to that instance's Config.Events. Events carry no request metadata here (a
// standalone UserAdmin has no request context of its own).
func (u *UserAdmin) WithEvents(h EventHandler) *UserAdmin {
	sink := newEventSink(h, slog.Default(), u.store.userEmail)
	c := *u
	c.sink = sink
	c.emit = func(ctx context.Context, e Event) {
		e.At = c.now()
		sink.emit(ctx, e)
	}
	return &c
}

// DroppedEvents is how many of this UserAdmin's events were dropped because
// the queue was full (see Auth.DroppedEvents).
func (u *UserAdmin) DroppedEvents() int64 { return u.sink.droppedCount() }

// Shutdown waits for this UserAdmin's queued events to be delivered (or ctx).
// Call it on graceful shutdown after the last admin action.
func (u *UserAdmin) Shutdown(ctx context.Context) error {
	return u.sink.drain(ctx)
}

func (u *UserAdmin) event(ctx context.Context, e Event) {
	if u.emit != nil {
		u.emit(ctx, e)
	}
}

func (u *UserAdmin) revoked(ctx context.Context, userID, sessionID string, count int64, by string) {
	if count > 0 {
		u.event(ctx, Event{Kind: EventSessionRevoked, UserID: userID, SessionID: sessionID,
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
	var x AdminUser
	dest := append([]any{&x.Role, &x.Banned, &x.BanReason, &x.BanExpires, &x.TwoFactorEnabled, &x.HasPassword}, extra...)
	u, err := scanUser(row, dest...)
	x.User = u
	return x, err
}

var sortColumns = map[string]string{"": `u."createdAt"`, "createdAt": `u."createdAt"`, "email": `u."email"`, "name": `u."name"`}

// likeEscaper escapes LIKE wildcards so a search is always a literal substring.
var likeEscaper = strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)

// ListUsers returns one page of users matching q, with the total match count.
func (u *UserAdmin) ListUsers(ctx context.Context, q ListUsersQuery) (UserPage, error) {
	order, ok := sortColumns[q.SortBy]
	if !ok {
		order = sortColumns[""]
	}
	dir := "ASC"
	if q.SortDesc {
		dir = "DESC"
	}
	field := `u."email"`
	if q.SearchField == "name" {
		field = `u."name"`
	}
	if q.Limit <= 0 {
		q.Limit = defaultUserListLimit
	}
	q.Limit = min(q.Limit, maxUserListLimit)
	q.Offset = max(q.Offset, 0)

	where := []string{"TRUE"}
	args := []any{}
	arg := func(v any) string {
		args = append(args, v)
		return "$" + strconv.Itoa(len(args))
	}
	if q.Search != "" {
		where = append(where, field+` ILIKE `+arg("%"+likeEscaper.Replace(q.Search)+"%"))
	}
	if q.Role != "" {
		where = append(where, `u."role" = `+arg(q.Role))
	}
	if q.Banned != nil {
		where = append(where, `u."banned" = `+arg(*q.Banned))
	}
	sql := `SELECT ` + adminUserColumns + `, count(*) OVER () FROM "user" u WHERE ` + strings.Join(where, " AND ") +
		` ORDER BY ` + order + ` ` + dir + `, u."id" ` + dir + ` LIMIT ` + arg(q.Limit) + ` OFFSET ` + arg(q.Offset)

	rows, err := u.store.pool.Query(ctx, sql, args...)
	if err != nil {
		return UserPage{}, err
	}
	page := UserPage{Users: []AdminUser{}, Limit: q.Limit, Offset: q.Offset}
	var total int
	users, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (AdminUser, error) {
		return scanAdminUser(row, &total)
	})
	if err != nil {
		return UserPage{}, err
	}
	if len(users) > 0 {
		page.Users, page.Total = users, total
	} else if q.Offset > 0 {
		// Past the end: count separately so the caller can still page back.
		if err := u.store.pool.QueryRow(ctx, `SELECT count(*) FROM "user" u WHERE `+strings.Join(where, " AND "), args[:len(args)-2]...).Scan(&page.Total); err != nil {
			return UserPage{}, err
		}
	}
	return page, nil
}

func (u *UserAdmin) adminUser(ctx context.Context, userID string) (AdminUser, error) {
	x, err := scanAdminUser(u.store.pool.QueryRow(ctx, `SELECT `+adminUserColumns+` FROM "user" u WHERE u."id" = $1`, userID))
	if errors.Is(err, errNotFound) {
		return AdminUser{}, ErrUserNotFound
	}
	return x, err
}

// GetUserDetail returns a user with their active sessions.
func (u *UserAdmin) GetUserDetail(ctx context.Context, userID string) (UserDetail, error) {
	x, err := u.adminUser(ctx, userID)
	if err != nil {
		return UserDetail{}, err
	}
	sessions, err := u.store.listSessions(ctx, userID, u.now())
	if err != nil {
		return UserDetail{}, err
	}
	return UserDetail{User: x, Sessions: sessions}, nil
}

// BanUser bans a user for duration (0: until UnbanUser) and revokes all their
// sessions. Banned users cannot sign in and their sessions stop working.
func (u *UserAdmin) BanUser(ctx context.Context, userID, reason string, duration time.Duration) error {
	if duration < 0 {
		return errors.New("auth: negative ban duration")
	}
	now := u.now()
	var expires *time.Time
	if duration > 0 {
		e := now.Add(duration)
		expires = &e
	}
	var why *string
	if reason = strings.TrimSpace(reason); reason != "" {
		why = &reason
	}
	var revoked int64
	err := u.store.tx(ctx, func(q querier) error {
		tag, err := q.Exec(ctx, `
			UPDATE "user" SET "banned" = true, "banReason" = $2, "banExpires" = $3, "updatedAt" = $4 WHERE "id" = $1`,
			userID, why, expires, now)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return ErrUserNotFound
		}
		revoked, err = u.store.deleteUserSessionsCount(ctx, q, userID)
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
	u.event(ctx, Event{Kind: EventUserBanned, UserID: userID, Details: details})
	u.revoked(ctx, userID, "", revoked, "ban")
	return nil
}

// UnbanUser lifts a ban; the user can sign in again.
func (u *UserAdmin) UnbanUser(ctx context.Context, userID string) error {
	tag, err := u.store.pool.Exec(ctx, `
		UPDATE "user" SET "banned" = false, "banReason" = NULL, "banExpires" = NULL, "updatedAt" = $2 WHERE "id" = $1`,
		userID, u.now())
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrUserNotFound
	}
	u.event(ctx, Event{Kind: EventUserUnbanned, UserID: userID})
	return nil
}

// validRole accepts Better Auth role strings: one or more comma-separated names.
var validRole = regexp.MustCompile(`^[A-Za-z0-9_-]{1,32}(,[A-Za-z0-9_-]{1,32}){0,7}$`)

// SetRole sets a user's role (e.g. "user", "admin"; comma-separated for several).
func (u *UserAdmin) SetRole(ctx context.Context, userID, role string) error {
	if !validRole.MatchString(role) {
		return ErrInvalidRole
	}
	tag, err := u.store.pool.Exec(ctx, `UPDATE "user" SET "role" = $2, "updatedAt" = $3 WHERE "id" = $1`, userID, role, u.now())
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrUserNotFound
	}
	return nil
}

// RevokeSession signs out one session of userID (ErrSessionNotFound otherwise).
func (u *UserAdmin) RevokeSession(ctx context.Context, userID, sessionID string) error {
	if sessionID == "" {
		return ErrSessionNotFound
	}
	id, err := u.store.deleteUserSession(ctx, userID, sessionID, "")
	if errors.Is(err, errNotFound) {
		return ErrSessionNotFound
	}
	if err != nil {
		return err
	}
	u.revoked(ctx, userID, id, 1, "admin")
	return nil
}

// ResetTwoFactor removes a user's 2FA enrolment, trusted devices and pending
// 2FA sign-ins, for account recovery (lost authenticator and backup codes, or
// a rotated auth secret). With TwoFactorConfig.Required the user enrols again
// at their next sign-in.
func (u *UserAdmin) ResetTwoFactor(ctx context.Context, userID string) error {
	if _, err := u.adminUser(ctx, userID); err != nil {
		return err
	}
	err := u.store.tx(ctx, func(q querier) error {
		if err := u.store.deleteChallenges(ctx, q, userID); err != nil {
			return err
		}
		if _, err := q.Exec(ctx, `DELETE FROM "twoFactor" WHERE "userId" = $1`, userID); err != nil {
			return err
		}
		if _, err := q.Exec(ctx, `UPDATE "user" SET "twoFactorEnabled" = false, "updatedAt" = $2 WHERE "id" = $1`, userID, u.now()); err != nil {
			return err
		}
		return u.store.deleteTrustedDevices(ctx, q, userID)
	})
	if err == nil {
		u.event(ctx, Event{Kind: EventTwoFactorDisabled, UserID: userID, Details: map[string]string{"by": "admin"}})
	}
	return err
}

// RevokeUserSessions signs out every session of userID.
func (u *UserAdmin) RevokeUserSessions(ctx context.Context, userID string) error {
	n, err := u.store.deleteUserSessionsCount(ctx, u.store.pool, userID)
	if err == nil {
		u.revoked(ctx, userID, "", n, "admin")
	}
	return err
}
