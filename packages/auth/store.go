package auth

import (
	"context"
	"crypto/hmac"
	"encoding/json"
	"errors"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// credentialProvider is Better Auth's providerId for email + password accounts.
const credentialProvider = "credential"

// DB is what Auth needs from Postgres: a *pgxpool.Pool, or a pool holder
// that swaps pools when DATABASE_URL rotates (database.Pool).
type DB interface {
	querier
	Begin(ctx context.Context) (pgx.Tx, error)
}

// querier is satisfied by *pgxpool.Pool and pgx.Tx.
type querier interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// store holds the hand-written queries over the Better Auth tables.
type store struct {
	pool DB
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

func (store *store) tx(ctx context.Context, fn func(txQuerier querier) error) error {
	return pgx.BeginFunc(ctx, store.pool, func(tx pgx.Tx) error { return fn(tx) })
}

const userColumns = `u."id", u."name", u."email", u."emailVerified", u."image", u."createdAt", u."updatedAt"`

func scanUser(row pgx.Row, extra ...any) (User, error) {
	var user User
	dest := append([]any{&user.ID, &user.Name, &user.Email, &user.EmailVerified, &user.Image, &user.CreatedAt, &user.UpdatedAt}, extra...)
	return user, notFound(row.Scan(dest...))
}

// userWithPassword returns the user for a lowercased email and the hash of
// their credential account (nil when they have none).
func (store *store) userWithPassword(ctx context.Context, email string) (User, *string, error) {
	var hash *string
	user, err := scanUser(store.pool.QueryRow(ctx, `
		SELECT `+userColumns+`, a."password"
		FROM "user" u
		LEFT JOIN "account" a ON a."userId" = u."id" AND a."providerId" = $2 AND a."accountId" = u."id"
		WHERE u."email" = $1`, email, credentialProvider), &hash)
	return user, hash, err
}

func (store *store) userByEmail(ctx context.Context, email string) (User, error) {
	return scanUser(store.pool.QueryRow(ctx, `SELECT `+userColumns+` FROM "user" u WHERE u."email" = $1`, email))
}

func (store *store) userByID(ctx context.Context, querier querier, id string) (User, error) {
	return scanUser(querier.QueryRow(ctx, `SELECT `+userColumns+` FROM "user" u WHERE u."id" = $1`, id))
}

func (store *store) createUser(ctx context.Context, querier querier, user User) error {
	_, err := querier.Exec(ctx, `
		INSERT INTO "user" ("id", "name", "email", "emailVerified", "image", "createdAt", "updatedAt")
		VALUES ($1, $2, $3, $4, $5, $6, $7)`,
		user.ID, user.Name, user.Email, user.EmailVerified, user.Image, user.CreatedAt, user.UpdatedAt)
	return err
}

// setCredentialPassword sets the password of the user's credential account,
// creating the account when the user has none (e.g. OAuth-only users).
func (store *store) setCredentialPassword(ctx context.Context, querier querier, userID, hash string, now time.Time) error {
	tag, err := querier.Exec(ctx, `
		UPDATE "account" SET "password" = $3, "updatedAt" = $4
		WHERE "userId" = $1 AND "providerId" = $2 AND "accountId" = $1`,
		userID, credentialProvider, hash, now)
	if err != nil || tag.RowsAffected() > 0 {
		return err
	}
	_, err = querier.Exec(ctx, `
		INSERT INTO "account" ("id", "accountId", "providerId", "userId", "password", "createdAt", "updatedAt")
		VALUES ($1, $2, $3, $2, $4, $5, $5)`,
		generateID(idLength), userID, credentialProvider, hash, now)
	return err
}

// verifyEmail marks an unverified user's email verified if check accepts
// their current password hash. The user row is locked, so it cannot race a
// password replacement. It returns errNotFound when the email is unknown or
// already verified, so only one of several concurrent verifications wins.
func (store *store) verifyEmail(ctx context.Context, email string, now time.Time, check func(hash *string) error) (User, error) {
	var out User
	err := store.tx(ctx, func(querier querier) error {
		var hash *string
		user, err := scanUser(querier.QueryRow(ctx, `
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
		user.EmailVerified, user.UpdatedAt = true, now
		_, err = querier.Exec(ctx, `UPDATE "user" SET "emailVerified" = true, "updatedAt" = $2 WHERE "id" = $1`, user.ID, now)
		out = user
		return err
	})
	return out, err
}

// replaceUnverifiedPassword sets a new password on an account whose email is
// still unverified and drops its sessions. It returns errNotFound when the
// account is verified (or gone).
// It returns the account and how many sessions it dropped.
func (store *store) replaceUnverifiedPassword(ctx context.Context, email, hash string, now time.Time) (id string, revoked int64, err error) {
	err = store.tx(ctx, func(querier querier) error {
		if err := querier.QueryRow(ctx, `SELECT "id" FROM "user" WHERE "email" = $1 AND NOT "emailVerified" FOR UPDATE`, email).Scan(&id); err != nil {
			return notFound(err)
		}
		var err error
		if revoked, err = store.deleteUserSessionsCount(ctx, querier, id); err != nil {
			return err
		}
		return store.setCredentialPassword(ctx, querier, id, hash, now)
	})
	return id, revoked, err
}

func (store *store) createSession(ctx context.Context, sess Session) error {
	_, err := store.pool.Exec(ctx, `
		INSERT INTO "session" ("id", "expiresAt", "token", "createdAt", "updatedAt", "ipAddress", "userAgent", "userId")
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`,
		sess.ID, sess.ExpiresAt, sess.Token, sess.CreatedAt, sess.UpdatedAt, sess.IPAddress, sess.UserAgent, sess.UserID)
	return err
}

// sessionWithUser loads a session and its user by token, expired or not.
// sessionWithUser loads a session, its user and the user's ban, by token,
// expired or not.
func (store *store) sessionWithUser(ctx context.Context, token string) (Session, User, ban, error) {
	var sess Session
	var banRow ban
	user, err := scanUser(store.pool.QueryRow(ctx, `
		SELECT `+userColumns+`, s."id", s."expiresAt", s."token", s."createdAt", s."updatedAt", s."ipAddress", s."userAgent", s."userId",
			u."banned", u."banExpires"
		FROM "session" s JOIN "user" u ON u."id" = s."userId"
		WHERE s."token" = $1`, token),
		&sess.ID, &sess.ExpiresAt, &sess.Token, &sess.CreatedAt, &sess.UpdatedAt, &sess.IPAddress, &sess.UserAgent, &sess.UserID,
		&banRow.banned, &banRow.expires)
	return sess, user, banRow, err
}

// ban is a user's admin-plugin ban state.
type ban struct {
	banned  bool
	expires *time.Time
}

// active reports whether the ban still applies at now; an expired ban does not.
func (ban ban) active(now time.Time) bool {
	return ban.banned && (ban.expires == nil || ban.expires.After(now))
}

func (store *store) banOf(ctx context.Context, userID string) (ban, error) {
	var banRow ban
	err := store.pool.QueryRow(ctx, `SELECT "banned", "banExpires" FROM "user" WHERE "id" = $1`, userID).Scan(&banRow.banned, &banRow.expires)
	return banRow, notFound(err)
}

// liftExpiredBan clears a ban whose expiry has passed (Better Auth lifts it
// lazily the same way).
func (store *store) liftExpiredBan(ctx context.Context, userID string, now time.Time) error {
	_, err := store.pool.Exec(ctx, `
		UPDATE "user" SET "banned" = false, "banReason" = NULL, "banExpires" = NULL, "updatedAt" = $2
		WHERE "id" = $1 AND "banned" AND "banExpires" IS NOT NULL AND "banExpires" <= $2`, userID, now)
	return err
}

func (store *store) extendSession(ctx context.Context, id string, expiresAt, now time.Time) error {
	_, err := store.pool.Exec(ctx, `UPDATE "session" SET "expiresAt" = $2, "updatedAt" = $3 WHERE "id" = $1`, id, expiresAt, now)
	return err
}

func (store *store) deleteSession(ctx context.Context, token string) error {
	_, err := store.pool.Exec(ctx, `DELETE FROM "session" WHERE "token" = $1`, token)
	return err
}

// endSession deletes the session with token and returns its id and user, or
// errNotFound.
func (store *store) endSession(ctx context.Context, token string) (id, userID string, err error) {
	err = store.pool.QueryRow(ctx, `DELETE FROM "session" WHERE "token" = $1 RETURNING "id", "userId"`, token).Scan(&id, &userID)
	return id, userID, notFound(err)
}

// deleteUserSessionsCount deletes every session of a user and returns how many.
func (store *store) deleteUserSessionsCount(ctx context.Context, querier querier, userID string) (int64, error) {
	tag, err := querier.Exec(ctx, `DELETE FROM "session" WHERE "userId" = $1`, userID)
	return tag.RowsAffected(), err
}

func (store *store) createVerification(ctx context.Context, identifier, value string, expiresAt, now time.Time) error {
	_, err := store.pool.Exec(ctx, `
		INSERT INTO "verification" ("id", "identifier", "value", "expiresAt", "createdAt", "updatedAt")
		VALUES ($1, $2, $3, $4, $5, $5)`,
		generateID(idLength), identifier, value, expiresAt, now)
	return err
}

// verificationValue returns the value of an unexpired verification row.
func (store *store) verificationValue(ctx context.Context, identifier string, now time.Time) (string, error) {
	var value string
	err := store.pool.QueryRow(ctx, `
		SELECT "value" FROM "verification" WHERE "identifier" = $1 AND "expiresAt" > $2
		ORDER BY "createdAt" DESC LIMIT 1`, identifier, now).Scan(&value)
	return value, notFound(err)
}

// consumeVerification deletes every row for identifier and returns the value
// if one was still valid. The delete makes the token single-use even under
// concurrent requests: only one transaction gets the row back.
func (store *store) consumeVerification(ctx context.Context, querier querier, identifier string, now time.Time) (string, error) {
	rows, err := querier.Query(ctx, `DELETE FROM "verification" WHERE "identifier" = $1 RETURNING "value", "expiresAt"`, identifier)
	if err != nil {
		return "", err
	}
	defer rows.Close()
	value := ""
	for rows.Next() {
		var raw string
		var expiresAt time.Time
		if err := rows.Scan(&raw, &expiresAt); err != nil {
			return "", err
		}
		if expiresAt.After(now) {
			value = raw
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
func (store *store) deleteResetTokens(ctx context.Context, querier querier, userID string) error {
	_, err := querier.Exec(ctx, `DELETE FROM "verification" WHERE "identifier" LIKE 'reset-password:%' AND "value" = $1`, userID)
	return err
}

// deleteExpiredVerifications removes stale verification rows.
func (store *store) deleteExpiredVerifications(ctx context.Context, now time.Time) error {
	_, err := store.pool.Exec(ctx, `DELETE FROM "verification" WHERE "expiresAt" <= $1`, now)
	return err
}

// credentialHash returns the password hash of the user's credential account,
// or errNotFound when they have none.
func (store *store) credentialHash(ctx context.Context, userID string) (string, error) {
	var hash *string
	err := store.pool.QueryRow(ctx, `
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
func (store *store) listSessions(ctx context.Context, userID string, now time.Time) ([]Session, error) {
	rows, err := store.pool.Query(ctx, `
		SELECT `+sessionColumns+` FROM "session"
		WHERE "userId" = $1 AND "expiresAt" > $2
		ORDER BY "createdAt" DESC, "id"`, userID, now)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (Session, error) {
		var session Session
		err := row.Scan(&session.ID, &session.ExpiresAt, &session.CreatedAt, &session.UpdatedAt, &session.IPAddress, &session.UserAgent, &session.UserID)
		return session, err
	})
}

// deleteUserSession deletes one of the user's sessions, matched by id or by
// token. It returns errNotFound when the user has no such session.
func (store *store) deleteUserSession(ctx context.Context, userID, id, token string) (string, error) {
	var deleted string
	err := store.pool.QueryRow(ctx, `
		DELETE FROM "session"
		WHERE "userId" = $1 AND (("id" = $2 AND $2 <> '') OR ("token" = $3 AND $3 <> ''))
		RETURNING "id"`, userID, id, token).Scan(&deleted)
	return deleted, notFound(err)
}

// deleteOtherSessions deletes every session of the user except keepID and
// returns how many.
func (store *store) deleteOtherSessions(ctx context.Context, querier querier, userID, keepID string) (int64, error) {
	tag, err := querier.Exec(ctx, `DELETE FROM "session" WHERE "userId" = $1 AND "id" <> $2`, userID, keepID)
	return tag.RowsAffected(), err
}

// changeEmailPrefix prefixes the verification row of a pending email change;
// its value is the user id.
const changeEmailPrefix = "change-email:"

// deleteEmailChanges cancels every pending email change of a user.
func (store *store) deleteEmailChanges(ctx context.Context, querier querier, userID string) error {
	_, err := querier.Exec(ctx, `DELETE FROM "verification" WHERE "identifier" LIKE 'change-email:%' AND "value" = $1`, userID)
	return err
}

// startEmailChange records a pending email change (nonce) for userID,
// replacing any earlier one so only the newest link works.
func (store *store) startEmailChange(ctx context.Context, userID, nonce string, expiresAt, now time.Time) error {
	return store.tx(ctx, func(querier querier) error {
		if err := store.deleteEmailChanges(ctx, querier, userID); err != nil {
			return err
		}
		_, err := querier.Exec(ctx, `
			INSERT INTO "verification" ("id", "identifier", "value", "expiresAt", "createdAt", "updatedAt")
			VALUES ($1, $2, $3, $4, $5, $5)`,
			generateID(idLength), changeEmailPrefix+nonce, userID, expiresAt, now)
		return err
	})
}

// changeEmail applies a pending email change: it consumes the nonce row
// (which must belong to userID), moves the user from email to newEmail,
// marks it verified and drops their reset links (they went to the old
// address). It returns errNotFound when the change is no longer pending or the
// user's email moved on, and a unique violation when newEmail was taken
// meanwhile.
func (store *store) changeEmail(ctx context.Context, userID, nonce, email, newEmail string, now time.Time) (User, error) {
	var out User
	err := store.tx(ctx, func(querier querier) error {
		owner, err := store.consumeVerification(ctx, querier, changeEmailPrefix+nonce, now)
		if err != nil {
			return err
		}
		if owner != userID {
			return errNotFound
		}
		user, err := scanUser(querier.QueryRow(ctx, `
			UPDATE "user" u SET "email" = $3, "emailVerified" = true, "updatedAt" = $4
			WHERE u."id" = $1 AND u."email" = $2
			RETURNING `+userColumns, userID, email, newEmail, now))
		if err != nil {
			return err
		}
		out = user
		return store.deleteResetTokens(ctx, querier, user.ID)
	})
	return out, err
}

// changePassword replaces the credential password only if it is still
// oldHash, so concurrent changes cannot both win. It returns errNotFound when
// the password changed meanwhile.
func (store *store) changePassword(ctx context.Context, querier querier, userID, oldHash, newHash string, now time.Time) error {
	tag, err := querier.Exec(ctx, `
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

// twoFactorRow is a user's "twoFactor" row.
type twoFactorRow struct {
	secret      string
	backupCodes []string
	verified    bool
	lockedUntil *time.Time
	lastStep    int64
}

func (store *store) twoFactor(ctx context.Context, userID string) (twoFactorRow, error) {
	var row twoFactorRow
	var codes string
	err := store.pool.QueryRow(ctx, `
		SELECT "secret", "backupCodes", "verified", "lockedUntil", "lastUsedStep" FROM "twoFactor" WHERE "userId" = $1`,
		userID).Scan(&row.secret, &codes, &row.verified, &row.lockedUntil, &row.lastStep)
	if err != nil {
		return twoFactorRow{}, notFound(err)
	}
	if err := json.Unmarshal([]byte(codes), &row.backupCodes); err != nil {
		return twoFactorRow{}, err
	}
	return row, nil
}

// startTwoFactor stores a new, unverified secret and backup codes for the
// user, replacing an earlier unverified enrolment. It returns errAlreadyEnabled
// when a verified one exists.
func (store *store) startTwoFactor(ctx context.Context, userID, secret string, codes []string, now time.Time) error {
	raw, err := json.Marshal(codes)
	if err != nil {
		return err
	}
	tag, err := store.pool.Exec(ctx, `
		INSERT INTO "twoFactor" ("id", "secret", "backupCodes", "userId", "verified", "createdAt", "updatedAt")
		VALUES ($1, $2, $3, $4, false, $5, $5)
		ON CONFLICT ("userId") DO UPDATE SET
			"secret" = EXCLUDED."secret", "backupCodes" = EXCLUDED."backupCodes", "lastUsedStep" = 0,
			"failedVerificationCount" = 0, "lockedUntil" = NULL, "updatedAt" = EXCLUDED."updatedAt"
		WHERE NOT "twoFactor"."verified"`,
		generateID(idLength), secret, string(raw), userID, now)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return errAlreadyEnabled
	}
	return nil
}

var errAlreadyEnabled = errors.New("auth: two-factor already enabled")

// acceptTOTPStep records step as used if it is newer than the last accepted
// one, verifying the enrolment and enabling 2FA on the user the first time.
// It returns errNotFound for a replayed (or older) step.
// The update is pinned to secret, the one the code was checked against, so an
// enrolment replaced meanwhile is never the one that gets verified.
// enabled reports whether this call is the one that turned 2FA on.
func (store *store) acceptTOTPStep(ctx context.Context, userID, secret string, step int64, now time.Time) (enabled bool, err error) {
	err = store.tx(ctx, func(querier querier) error {
		tag, err := querier.Exec(ctx, `
			UPDATE "twoFactor" SET "lastUsedStep" = $2, "verified" = true, "failedVerificationCount" = 0,
				"lockedUntil" = NULL, "updatedAt" = $3
			WHERE "userId" = $1 AND "lastUsedStep" < $2 AND "secret" = $4`, userID, step, now, secret)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return errNotFound
		}
		tag, err = querier.Exec(ctx, `UPDATE "user" SET "twoFactorEnabled" = true, "updatedAt" = $2 WHERE "id" = $1 AND NOT "twoFactorEnabled"`, userID, now)
		enabled = tag.RowsAffected() > 0
		return err
	})
	return enabled, err
}

// useBackupCode removes hash from the user's verified backup codes. It
// returns errNotFound when the code is not (or no longer) there.
func (store *store) useBackupCode(ctx context.Context, userID, hash string, now time.Time) error {
	return store.tx(ctx, func(querier querier) error {
		var raw string
		err := querier.QueryRow(ctx, `SELECT "backupCodes" FROM "twoFactor" WHERE "userId" = $1 AND "verified" FOR UPDATE`, userID).Scan(&raw)
		if err != nil {
			return notFound(err)
		}
		var codes []string
		if err := json.Unmarshal([]byte(raw), &codes); err != nil {
			return err
		}
		index := slices.IndexFunc(codes, func(code string) bool { return hmac.Equal([]byte(code), []byte(hash)) })
		if index < 0 {
			return errNotFound
		}
		left, err := json.Marshal(slices.Delete(codes, index, index+1))
		if err != nil {
			return err
		}
		_, err = querier.Exec(ctx, `
			UPDATE "twoFactor" SET "backupCodes" = $2, "failedVerificationCount" = 0, "lockedUntil" = NULL, "updatedAt" = $3
			WHERE "userId" = $1`, userID, string(left), now)
		return err
	})
}

// restoreBackupCode puts back a backup code that was spent without effect.
func (store *store) restoreBackupCode(ctx context.Context, userID, hash string, now time.Time) error {
	_, err := store.pool.Exec(ctx, `
		UPDATE "twoFactor" SET "backupCodes" = ("backupCodes"::jsonb || to_jsonb($2::text))::text, "updatedAt" = $3
		WHERE "userId" = $1 AND "verified" AND NOT ("backupCodes"::jsonb ? $2)`, userID, hash, now)
	return err
}

// resetTwoFactorFailures clears an expired lockout so failures count afresh.
func (store *store) resetTwoFactorFailures(ctx context.Context, userID string, now time.Time) error {
	_, err := store.pool.Exec(ctx, `
		UPDATE "twoFactor" SET "failedVerificationCount" = 0, "lockedUntil" = NULL, "updatedAt" = $2
		WHERE "userId" = $1 AND "lockedUntil" <= $2`, userID, now)
	return err
}

// setBackupCodes replaces a verified enrolment's backup codes.
func (store *store) setBackupCodes(ctx context.Context, userID string, codes []string, now time.Time) error {
	raw, err := json.Marshal(codes)
	if err != nil {
		return err
	}
	tag, err := store.pool.Exec(ctx, `UPDATE "twoFactor" SET "backupCodes" = $2, "updatedAt" = $3 WHERE "userId" = $1 AND "verified"`, userID, string(raw), now)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return errNotFound
	}
	return nil
}

// recordTwoFactorFailure counts a wrong code and locks the user's second
// factor for lockFor once maxFailures is reached.
func (store *store) recordTwoFactorFailure(ctx context.Context, userID string, maxFailures int, lockFor time.Duration, now time.Time) error {
	_, err := store.pool.Exec(ctx, `
		UPDATE "twoFactor" SET
			"failedVerificationCount" = "failedVerificationCount" + 1,
			"lockedUntil" = CASE WHEN "failedVerificationCount" + 1 >= $2 THEN $3::timestamptz ELSE "lockedUntil" END,
			"updatedAt" = $4
		WHERE "userId" = $1`, userID, maxFailures, now.Add(lockFor), now)
	return err
}

// disableTwoFactor removes the user's enrolment and trusted devices.
func (store *store) disableTwoFactor(ctx context.Context, userID string, now time.Time) error {
	return store.tx(ctx, func(querier querier) error {
		if _, err := querier.Exec(ctx, `DELETE FROM "twoFactor" WHERE "userId" = $1`, userID); err != nil {
			return err
		}
		if _, err := querier.Exec(ctx, `UPDATE "user" SET "twoFactorEnabled" = false, "updatedAt" = $2 WHERE "id" = $1`, userID, now); err != nil {
			return err
		}
		return store.deleteTrustedDevices(ctx, querier, userID)
	})
}

// twoFactorEnabled reports the user's twoFactorEnabled flag.
func (store *store) twoFactorEnabled(ctx context.Context, userID string) (bool, error) {
	var on bool
	err := store.pool.QueryRow(ctx, `SELECT "twoFactorEnabled" FROM "user" WHERE "id" = $1`, userID).Scan(&on)
	return on, notFound(err)
}

const (
	challengePrefix   = "2fa:"
	trustDevicePrefix = "trust-device:"
)

// deleteTrustedDevices forgets every "trust this device" of a user.
func (store *store) deleteTrustedDevices(ctx context.Context, querier querier, userID string) error {
	_, err := querier.Exec(ctx, `DELETE FROM "verification" WHERE "identifier" LIKE 'trust-device:%' AND "value" = $1`, userID)
	return err
}

// deleteChallenges cancels a user's pending 2FA sign-ins.
func (store *store) deleteChallenges(ctx context.Context, querier querier, userID string) error {
	_, err := querier.Exec(ctx, `DELETE FROM "verification" WHERE "identifier" LIKE '2fa:%' AND ("value" = $1 OR "value" = 'setup:' || $1)`, userID)
	return err
}
