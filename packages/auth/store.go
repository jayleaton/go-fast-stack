package auth

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// credentialProvider is Better Auth's providerId for email + password accounts.
const credentialProvider = "credential"

// querier is satisfied by *pgxpool.Pool and pgx.Tx.
type querier interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// store holds the hand-written queries over the Better Auth tables.
type store struct {
	pool *pgxpool.Pool
}

var errNotFound = errors.New("auth: not found")

func notFound(err error) error {
	if errors.Is(err, pgx.ErrNoRows) {
		return errNotFound
	}
	return err
}

func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}

func (s *store) tx(ctx context.Context, fn func(q querier) error) error {
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error { return fn(tx) })
}

const userColumns = `u."id", u."name", u."email", u."emailVerified", u."image", u."createdAt", u."updatedAt"`

func scanUser(row pgx.Row, extra ...any) (User, error) {
	var u User
	dest := append([]any{&u.ID, &u.Name, &u.Email, &u.EmailVerified, &u.Image, &u.CreatedAt, &u.UpdatedAt}, extra...)
	return u, notFound(row.Scan(dest...))
}

// userWithPassword returns the user for a lowercased email and the hash of
// their credential account (nil when they have none).
func (s *store) userWithPassword(ctx context.Context, email string) (User, *string, error) {
	var hash *string
	u, err := scanUser(s.pool.QueryRow(ctx, `
		SELECT `+userColumns+`, a."password"
		FROM "user" u
		LEFT JOIN "account" a ON a."userId" = u."id" AND a."providerId" = $2 AND a."accountId" = u."id"
		WHERE u."email" = $1`, email, credentialProvider), &hash)
	return u, hash, err
}

func (s *store) userByEmail(ctx context.Context, email string) (User, error) {
	return scanUser(s.pool.QueryRow(ctx, `SELECT `+userColumns+` FROM "user" u WHERE u."email" = $1`, email))
}

func (s *store) userByID(ctx context.Context, q querier, id string) (User, error) {
	return scanUser(q.QueryRow(ctx, `SELECT `+userColumns+` FROM "user" u WHERE u."id" = $1`, id))
}

func (s *store) createUser(ctx context.Context, q querier, u User) error {
	_, err := q.Exec(ctx, `
		INSERT INTO "user" ("id", "name", "email", "emailVerified", "image", "createdAt", "updatedAt")
		VALUES ($1, $2, $3, $4, $5, $6, $7)`,
		u.ID, u.Name, u.Email, u.EmailVerified, u.Image, u.CreatedAt, u.UpdatedAt)
	return err
}

// setCredentialPassword sets the password of the user's credential account,
// creating the account when the user has none (e.g. OAuth-only users).
func (s *store) setCredentialPassword(ctx context.Context, q querier, userID, hash string, now time.Time) error {
	tag, err := q.Exec(ctx, `
		UPDATE "account" SET "password" = $3, "updatedAt" = $4
		WHERE "userId" = $1 AND "providerId" = $2 AND "accountId" = $1`,
		userID, credentialProvider, hash, now)
	if err != nil || tag.RowsAffected() > 0 {
		return err
	}
	_, err = q.Exec(ctx, `
		INSERT INTO "account" ("id", "accountId", "providerId", "userId", "password", "createdAt", "updatedAt")
		VALUES ($1, $2, $3, $2, $4, $5, $5)`,
		generateID(32), userID, credentialProvider, hash, now)
	return err
}

// verifyEmail marks an unverified user's email verified if check accepts
// their current password hash. The user row is locked, so it cannot race a
// password replacement. It returns errNotFound when the email is unknown or
// already verified, so only one of several concurrent verifications wins.
func (s *store) verifyEmail(ctx context.Context, email string, now time.Time, check func(hash *string) error) (User, error) {
	var out User
	err := s.tx(ctx, func(q querier) error {
		var hash *string
		u, err := scanUser(q.QueryRow(ctx, `
			SELECT `+userColumns+`, a."password"
			FROM "user" u
			LEFT JOIN "account" a ON a."userId" = u."id" AND a."providerId" = $2 AND a."accountId" = u."id"
			WHERE u."email" = $1 AND NOT u."emailVerified"
			FOR UPDATE OF u`, email, credentialProvider), &hash)
		if err != nil {
			return err
		}
		if err := check(hash); err != nil {
			return err
		}
		u.EmailVerified, u.UpdatedAt = true, now
		_, err = q.Exec(ctx, `UPDATE "user" SET "emailVerified" = true, "updatedAt" = $2 WHERE "id" = $1`, u.ID, now)
		out = u
		return err
	})
	return out, err
}

// replaceUnverifiedPassword sets a new password on an account whose email is
// still unverified and drops its sessions. It returns errNotFound when the
// account is verified (or gone).
func (s *store) replaceUnverifiedPassword(ctx context.Context, email, hash string, now time.Time) error {
	return s.tx(ctx, func(q querier) error {
		var id string
		err := q.QueryRow(ctx, `SELECT "id" FROM "user" WHERE "email" = $1 AND NOT "emailVerified" FOR UPDATE`, email).Scan(&id)
		if err != nil {
			return notFound(err)
		}
		if err := s.deleteUserSessions(ctx, q, id); err != nil {
			return err
		}
		return s.setCredentialPassword(ctx, q, id, hash, now)
	})
}

func (s *store) createSession(ctx context.Context, sess Session) error {
	_, err := s.pool.Exec(ctx, `
		INSERT INTO "session" ("id", "expiresAt", "token", "createdAt", "updatedAt", "ipAddress", "userAgent", "userId")
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`,
		sess.ID, sess.ExpiresAt, sess.Token, sess.CreatedAt, sess.UpdatedAt, sess.IPAddress, sess.UserAgent, sess.UserID)
	return err
}

// sessionWithUser loads a session and its user by token, expired or not.
func (s *store) sessionWithUser(ctx context.Context, token string) (Session, User, error) {
	var sess Session
	u, err := scanUser(s.pool.QueryRow(ctx, `
		SELECT `+userColumns+`, s."id", s."expiresAt", s."token", s."createdAt", s."updatedAt", s."ipAddress", s."userAgent", s."userId"
		FROM "session" s JOIN "user" u ON u."id" = s."userId"
		WHERE s."token" = $1`, token),
		&sess.ID, &sess.ExpiresAt, &sess.Token, &sess.CreatedAt, &sess.UpdatedAt, &sess.IPAddress, &sess.UserAgent, &sess.UserID)
	return sess, u, err
}

func (s *store) extendSession(ctx context.Context, id string, expiresAt, now time.Time) error {
	_, err := s.pool.Exec(ctx, `UPDATE "session" SET "expiresAt" = $2, "updatedAt" = $3 WHERE "id" = $1`, id, expiresAt, now)
	return err
}

func (s *store) deleteSession(ctx context.Context, token string) error {
	_, err := s.pool.Exec(ctx, `DELETE FROM "session" WHERE "token" = $1`, token)
	return err
}

func (s *store) deleteUserSessions(ctx context.Context, q querier, userID string) error {
	_, err := q.Exec(ctx, `DELETE FROM "session" WHERE "userId" = $1`, userID)
	return err
}

func (s *store) createVerification(ctx context.Context, identifier, value string, expiresAt, now time.Time) error {
	_, err := s.pool.Exec(ctx, `
		INSERT INTO "verification" ("id", "identifier", "value", "expiresAt", "createdAt", "updatedAt")
		VALUES ($1, $2, $3, $4, $5, $5)`,
		generateID(32), identifier, value, expiresAt, now)
	return err
}

// verificationValue returns the value of an unexpired verification row.
func (s *store) verificationValue(ctx context.Context, identifier string, now time.Time) (string, error) {
	var value string
	err := s.pool.QueryRow(ctx, `
		SELECT "value" FROM "verification" WHERE "identifier" = $1 AND "expiresAt" > $2
		ORDER BY "createdAt" DESC LIMIT 1`, identifier, now).Scan(&value)
	return value, notFound(err)
}

// consumeVerification deletes every row for identifier and returns the value
// if one was still valid. The delete makes the token single-use even under
// concurrent requests: only one transaction gets the row back.
func (s *store) consumeVerification(ctx context.Context, q querier, identifier string, now time.Time) (string, error) {
	rows, err := q.Query(ctx, `DELETE FROM "verification" WHERE "identifier" = $1 RETURNING "value", "expiresAt"`, identifier)
	if err != nil {
		return "", err
	}
	defer rows.Close()
	value := ""
	for rows.Next() {
		var v string
		var expiresAt time.Time
		if err := rows.Scan(&v, &expiresAt); err != nil {
			return "", err
		}
		if expiresAt.After(now) {
			value = v
		}
	}
	if err := rows.Err(); err != nil {
		return "", err
	}
	if value == "" {
		return "", errNotFound
	}
	return value, nil
}

// deleteResetTokens removes every outstanding password reset token of a user.
func (s *store) deleteResetTokens(ctx context.Context, q querier, userID string) error {
	_, err := q.Exec(ctx, `DELETE FROM "verification" WHERE "identifier" LIKE 'reset-password:%' AND "value" = $1`, userID)
	return err
}

// deleteExpiredVerifications removes stale verification rows.
func (s *store) deleteExpiredVerifications(ctx context.Context, now time.Time) error {
	_, err := s.pool.Exec(ctx, `DELETE FROM "verification" WHERE "expiresAt" <= $1`, now)
	return err
}
