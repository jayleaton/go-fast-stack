package auth

import (
	"context"
	"errors"
	"strings"
)

// CreateUserInput is a user created by an administrator, without a password.
type CreateUserInput struct {
	Name  string
	Email string
	Role  string // default "user"
}

// CreateUserWithoutPassword creates a user who cannot sign in until they
// choose a password through IssueSetPasswordLink. A taken email returns
// ErrEmailTaken. The email is marked verified when the link is used.
func (a *Auth) CreateUserWithoutPassword(ctx context.Context, in CreateUserInput) (User, error) {
	email, err := normalizeEmail(in.Email)
	if err != nil {
		return User{}, err
	}
	role := in.Role
	if role == "" {
		role = "user"
	}
	if !validRole.MatchString(role) {
		return User{}, ErrInvalidRole
	}
	now := a.now()
	user := User{ID: generateID(idLength), Name: strings.TrimSpace(in.Name), Email: email, CreatedAt: now, UpdatedAt: now}
	err = a.store.tx(ctx, func(q querier) error {
		if err := a.store.createUser(ctx, q, user); err != nil {
			return err
		}
		_, err := q.Exec(ctx, `UPDATE "user" SET "role" = $2 WHERE "id" = $1`, user.ID, role)
		return err
	})
	if isUniqueViolation(err) {
		return User{}, ErrEmailTaken
	}
	if err != nil {
		return User{}, err
	}
	return user, nil
}

// IssueSetPasswordLink emails the user a single-use "set your password" link,
// valid for Config.InviteTTL. It replaces any earlier invite or reset link.
// The link is built like a reset link (see passwordLink): a relative
// redirectTo is opened directly with ?token=, a trusted absolute URL through
// GET /api/auth/reset-password/{token}. That page then calls
// CompleteSetPassword (or POST /api/auth/set-password).
func (a *Auth) IssueSetPasswordLink(ctx context.Context, userID, redirectTo string) error {
	if redirectTo == "" {
		return ErrInvalidRedirectURL
	}
	if err := a.checkCallbackURL(redirectTo, ErrInvalidRedirectURL); err != nil {
		return err
	}
	user, err := a.store.userByID(ctx, a.store.pool, userID)
	if errors.Is(err, errNotFound) {
		return ErrUserNotFound
	}
	if err != nil {
		return err
	}
	token, now := generateID(idLength), a.now()
	err = a.store.tx(ctx, func(q querier) error {
		if err := a.store.deleteResetTokens(ctx, q, userID); err != nil {
			return err
		}
		_, err := q.Exec(ctx, `
			INSERT INTO "verification" ("id", "identifier", "value", "expiresAt", "createdAt", "updatedAt")
			VALUES ($1, $2, $3, $4, $5, $5)`,
			generateID(idLength), "reset-password:"+token, userID, now.Add(a.cfg.InviteTTL), now)
		return err
	})
	if err != nil {
		return err
	}
	link := a.passwordLink(token, redirectTo)
	a.sendMail(ctx, "set-password", user.Email, func(ctx context.Context) error {
		return a.mailer.SendSetPasswordEmail(ctx, user.Email, link)
	})
	return nil
}

// CompleteSetPassword spends a set-password (or reset) token, sets the
// password, marks the email verified (the link proved it) and signs the user
// in. When a second factor applies it returns a *TwoFactorChallenge instead,
// with SetupRequired when TwoFactorConfig.Required and the user has not
// enrolled: the invite then continues into TOTP enrolment.
func (a *Auth) CompleteSetPassword(ctx context.Context, token, newPassword string) (User, Session, error) {
	userID, revoked, err := a.setPassword(ctx, token, newPassword, true)
	if err != nil {
		return User{}, Session{}, err
	}
	a.emit(ctx, Event{Kind: EventPasswordReset, UserID: userID, Method: SignInMethodSetPassword})
	a.emitRevoked(ctx, userID, "", revoked, "password_reset")
	user, err := a.store.userByID(ctx, a.store.pool, userID)
	if err != nil {
		return User{}, Session{}, err
	}
	if c, err := a.secondFactor(ctx, userID); err != nil {
		return User{}, Session{}, err
	} else if c != nil {
		return user, Session{}, c
	}
	sess, err := a.createSession(ctx, userID)
	if err != nil {
		return User{}, Session{}, err
	}
	a.emit(ctx, Event{Kind: EventSignIn, UserID: userID, Email: user.Email, SessionID: sess.ID, Method: SignInMethodSetPassword})
	return user, sess, nil
}
