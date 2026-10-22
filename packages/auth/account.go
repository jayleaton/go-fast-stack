package auth

import (
	"context"
	"errors"
	"net/url"
)

// ChangePassword sets a new password after checking the current one (Better
// Auth /change-password). Outstanding reset links stop working. With
// revokeOtherSessions every session of the user except currentSessionID is
// revoked. Users without a password (OAuth only) get
// ErrCredentialAccountNotFound; a wrong current password ErrInvalidPassword.
func (a *Auth) ChangePassword(ctx context.Context, userID, currentPassword, newPassword string, revokeOtherSessions bool, currentSessionID string) error {
	if err := a.rateLimit(ctx, "change-password", RuleSignIn); err != nil {
		return err
	}
	if err := a.checkPasswordLength(newPassword); err != nil {
		return err
	}
	if len([]rune(currentPassword)) > a.cfg.MaxPasswordLength {
		return ErrInvalidPassword
	}
	current, err := a.store.credentialHash(ctx, userID)
	if errors.Is(err, errNotFound) {
		return ErrCredentialAccountNotFound
	}
	if err != nil {
		return err
	}
	hash, err := hashPassword(newPassword, a.argon)
	if err != nil {
		return err
	}
	ok, err := verifyPassword(currentPassword, current)
	if err != nil {
		a.log.ErrorContext(ctx, "auth: stored password hash is unreadable", "userId", userID, "err", err)
		return ErrInvalidPassword
	}
	if !ok {
		return ErrInvalidPassword
	}
	now := a.now()
	return a.store.tx(ctx, func(q querier) error {
		if err := a.store.setCredentialPassword(ctx, q, userID, hash, now); err != nil {
			return err
		}
		if err := a.store.deleteResetTokens(ctx, q, userID); err != nil {
			return err
		}
		if revokeOtherSessions {
			return a.store.deleteOtherSessions(ctx, q, userID, currentSessionID)
		}
		return nil
	})
}

// ChangeEmail starts Better Auth's change-email flow: a verification link
// (24 h) goes to newEmail and a notice to the current address. The email only
// changes, and becomes verified, when the link is opened (VerifyEmail).
//
// A newEmail that belongs to another account returns ErrEmailTaken when email
// verification is off. When it is required (production) the call succeeds
// silently and sends nothing, like sign-up, so the endpoint does not reveal
// which addresses are registered. The address is checked again, atomically,
// when the link is opened.
func (a *Auth) ChangeEmail(ctx context.Context, userID, newEmail, callbackURL string) error {
	if err := a.rateLimit(ctx, "change-email", RuleEmailSending); err != nil {
		return err
	}
	if err := a.checkCallbackURL(callbackURL, ErrInvalidCallbackURL); err != nil {
		return err
	}
	newEmail, err := normalizeEmail(newEmail)
	if err != nil {
		return err
	}
	user, err := a.store.userByID(ctx, a.store.pool, userID)
	if errors.Is(err, errNotFound) {
		return ErrUserNotFound
	}
	if err != nil {
		return err
	}
	if newEmail == user.Email {
		return ErrEmailUnchanged
	}
	if _, err := a.store.userByEmail(ctx, newEmail); err == nil {
		if a.cfg.RequireEmailVerification {
			a.log.InfoContext(ctx, "auth: change-email to an existing address", "userId", userID)
			return nil
		}
		return ErrEmailTaken
	} else if !errors.Is(err, errNotFound) {
		return err
	}

	if callbackURL == "" {
		callbackURL = "/"
	}
	link := a.authURL("/verify-email") + "?token=" + a.signChangeEmailToken(user.Email, newEmail, VerificationTokenTTL) +
		"&callbackURL=" + url.QueryEscape(callbackURL)
	a.sendMail(ctx, "change-email", newEmail, func(ctx context.Context) error {
		return a.mailer.SendVerificationEmail(ctx, newEmail, link)
	})
	a.sendMail(ctx, "change-email-notice", user.Email, func(ctx context.Context) error {
		return a.mailer.SendEmailChangeNotice(ctx, user.Email, newEmail)
	})
	return nil
}

// confirmEmailChange applies a change-email link: the account that still has
// email moves to newEmail (verified) and the opener is signed in, keeping
// their session when it already belongs to that account.
func (a *Auth) confirmEmailChange(ctx context.Context, email, newEmail string) (User, Session, error) {
	user, err := a.store.changeEmail(ctx, email, newEmail, a.now())
	if errors.Is(err, errNotFound) {
		return User{}, Session{}, ErrInvalidToken // already used, or the email changed since
	}
	if isUniqueViolation(err) {
		return User{}, Session{}, ErrEmailTaken
	}
	if err != nil {
		return User{}, Session{}, err
	}
	if current, sess, ok := UserFrom(ctx); ok && current.ID == user.ID {
		return user, sess, nil
	}
	sess, err := a.createSession(ctx, user.ID)
	if err != nil {
		return User{}, Session{}, err
	}
	return user, sess, nil
}

// ListSessions returns the user's active sessions, newest first. Tokens are
// never included (Session.Token is empty), so the list cannot be used to
// hijack a session.
func (a *Auth) ListSessions(ctx context.Context, userID string) ([]Session, error) {
	return a.store.listSessions(ctx, userID, a.now())
}

// RevokeSession signs out one of the user's own sessions. Another user's
// session, or an unknown id, returns ErrSessionNotFound.
func (a *Auth) RevokeSession(ctx context.Context, userID, sessionID string) error {
	return a.revokeSession(ctx, userID, sessionID, "")
}

func (a *Auth) revokeSession(ctx context.Context, userID, sessionID, token string) error {
	if sessionID == "" && token == "" {
		return ErrSessionNotFound
	}
	err := a.store.deleteUserSession(ctx, userID, sessionID, token)
	if errors.Is(err, errNotFound) {
		return ErrSessionNotFound
	}
	return err
}

// RevokeOtherSessions signs out every session of the user except
// currentSessionID.
func (a *Auth) RevokeOtherSessions(ctx context.Context, userID, currentSessionID string) error {
	return a.store.deleteOtherSessions(ctx, a.store.pool, userID, currentSessionID)
}
