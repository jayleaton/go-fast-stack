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

// credentialHash returns the password hash of the user's credential account,
// or errNotFound when they have none.
func (s *store) credentialHash(ctx context.Context, userID string) (string, error) {
	var hash *string
	err := s.pool.QueryRow(ctx, `
		SELECT "password" FROM "account"
		WHERE "userId" = $1 AND "providerId" = $2 AND "accountId" = $1`, userID, credentialProvider).Scan(&hash)
	if err != nil {
		return "", notFound(err)
	}
	if hash == nil || *hash == "" {
		return "", errNotFound
	}
	return *hash, nil
}

const sessionColumns = `"id", "expiresAt", "createdAt", "updatedAt", "ipAddress", "userAgent", "userId"`

// listSessions returns the user's unexpired sessions, newest first, without
// their tokens.
func (s *store) listSessions(ctx context.Context, userID string, now time.Time) ([]Session, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT `+sessionColumns+` FROM "session"
		WHERE "userId" = $1 AND "expiresAt" > $2
		ORDER BY "createdAt" DESC, "id"`, userID, now)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (Session, error) {
		var x Session
		err := row.Scan(&x.ID, &x.ExpiresAt, &x.CreatedAt, &x.UpdatedAt, &x.IPAddress, &x.UserAgent, &x.UserID)
		return x, err
	})
}

// deleteUserSession deletes one of the user's sessions, matched by id or by
// token. It returns errNotFound when the user has no such session.
func (s *store) deleteUserSession(ctx context.Context, userID, id, token string) error {
	tag, err := s.pool.Exec(ctx, `
		DELETE FROM "session"
		WHERE "userId" = $1 AND (("id" = $2 AND $2 <> '') OR ("token" = $3 AND $3 <> ''))`, userID, id, token)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return errNotFound
	}
	return nil
}

// deleteOtherSessions deletes every session of the user except keepID.
func (s *store) deleteOtherSessions(ctx context.Context, q querier, userID, keepID string) error {
	_, err := q.Exec(ctx, `DELETE FROM "session" WHERE "userId" = $1 AND "id" <> $2`, userID, keepID)
	return err
}

// changeEmailPrefix prefixes the verification row of a pending email change;
// its value is the user id.
const changeEmailPrefix = "change-email:"

// deleteEmailChanges cancels every pending email change of a user.
func (s *store) deleteEmailChanges(ctx context.Context, q querier, userID string) error {
	_, err := q.Exec(ctx, `DELETE FROM "verification" WHERE "identifier" LIKE 'change-email:%' AND "value" = $1`, userID)
	return err
}

// startEmailChange records a pending email change (nonce) for userID,
// replacing any earlier one so only the newest link works.
func (s *store) startEmailChange(ctx context.Context, userID, nonce string, expiresAt, now time.Time) error {
	return s.tx(ctx, func(q querier) error {
		if err := s.deleteEmailChanges(ctx, q, userID); err != nil {
			return err
		}
		_, err := q.Exec(ctx, `
			INSERT INTO "verification" ("id", "identifier", "value", "expiresAt", "createdAt", "updatedAt")
			VALUES ($1, $2, $3, $4, $5, $5)`,
			generateID(32), changeEmailPrefix+nonce, userID, expiresAt, now)
		return err
	})
}

// changeEmail applies a pending email change: it consumes the nonce row
// (which must belong to userID), moves the user from email to newEmail,
// marks it verified and drops their reset links (they went to the old
// address). It returns errNotFound when the change is no longer pending or the
// user's email moved on, and a unique violation when newEmail was taken
// meanwhile.
func (s *store) changeEmail(ctx context.Context, userID, nonce, email, newEmail string, now time.Time) (User, error) {
	var out User
	err := s.tx(ctx, func(q querier) error {
		owner, err := s.consumeVerification(ctx, q, changeEmailPrefix+nonce, now)
		if err != nil {
			return err
		}
		if owner != userID {
			return errNotFound
		}
		u, err := scanUser(q.QueryRow(ctx, `
			UPDATE "user" u SET "email" = $3, "emailVerified" = true, "updatedAt" = $4
			WHERE u."id" = $1 AND u."email" = $2
			RETURNING `+userColumns, userID, email, newEmail, now))
		if err != nil {
			return err
		}
		out = u
		return s.deleteResetTokens(ctx, q, u.ID)
	})
	return out, err
}

// changePassword replaces the credential password only if it is still
// oldHash, so concurrent changes cannot both win. It returns errNotFound when
// the password changed meanwhile.
func (s *store) changePassword(ctx context.Context, q querier, userID, oldHash, newHash string, now time.Time) error {
	tag, err := q.Exec(ctx, `
		UPDATE "account" SET "password" = $4, "updatedAt" = $5
		WHERE "userId" = $1 AND "providerId" = $2 AND "accountId" = $1 AND "password" = $3`,
		userID, credentialProvider, oldHash, newHash, now)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return errNotFound
	}
	return nil
}
