package auth

import (
	"context"
	"errors"
)

// ChangePassword sets a new password after checking the current one (Better
// Auth /change-password). Outstanding reset links and pending email changes
// stop working. With revokeOtherSessions every session of the user except
// currentSessionID is revoked. Users without a password (OAuth only) get
// ErrCredentialAccountNotFound; a wrong current password ErrInvalidPassword.
// Attempts are limited per client IP and per user.
func (a *Auth) ChangePassword(ctx context.Context, userID, currentPassword, newPassword string, revokeOtherSessions bool, currentSessionID string) error {
	if err := a.rateLimitUser(ctx, "change-password", userID, RuleSignIn); err != nil {
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
	ok, err := verifyPassword(currentPassword, current)
	if err != nil {
		a.log.ErrorContext(ctx, "auth: stored password hash is unreadable", "userId", userID, "err", err)
		return ErrInvalidPassword
	}
	if !ok {
		return ErrInvalidPassword
	}
	hash, err := hashPassword(newPassword, a.argon)
	if err != nil {
		return err
	}
	now := a.now()
	return a.store.tx(ctx, func(q querier) error {
		// Conditional on the hash just verified: of two concurrent changes
		// only the first wins, the other sees ErrInvalidPassword.
		if err := a.store.changePassword(ctx, q, userID, current, hash, now); err != nil {
			if errors.Is(err, errNotFound) {
				return ErrInvalidPassword
			}
			return err
		}
		if err := a.store.deleteResetTokens(ctx, q, userID); err != nil {
			return err
		}
		if err := a.store.deleteEmailChanges(ctx, q, userID); err != nil {
			return err
		}
		if revokeOtherSessions {
			if err := a.store.deleteTrustedDevices(ctx, q, userID); err != nil {
				return err
			}
			return a.store.deleteOtherSessions(ctx, q, userID, currentSessionID)
		}
		return nil
	})
}

// ChangeEmail starts Better Auth's change-email flow: a verification link
// (24 h) goes to newEmail and a notice to the current address. The email only
// changes, and becomes verified, when the link is opened (VerifyEmail). Only
// the newest link of a user works, once; a password change or reset, or
// revoking other sessions, cancels it.
//
// When ctx carries the caller's session (requests through Middleware) it must
// be fresh, created within Config.FreshAge (default 1 day), else
// ErrSessionNotFresh: a long-lived stolen cookie cannot move the account to
// another address. Attempts are limited per client IP and per user.
//
// A newEmail that belongs to another account returns ErrEmailTaken when email
// verification is off. When it is required (production) the call succeeds
// silently and sends nothing, like sign-up, so the endpoint does not reveal
// which addresses are registered. The address is checked again, atomically,
// when the link is opened.
func (a *Auth) ChangeEmail(ctx context.Context, userID, newEmail, callbackURL string) error {
	if err := a.rateLimitUser(ctx, "change-email", userID, RuleEmailSending); err != nil {
		return err
	}
	if _, sess, ok := a.userFrom(ctx); ok && sess.UserID == userID && a.now().Sub(sess.CreatedAt) >= a.cfg.FreshAge {
		return ErrSessionNotFresh
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

	now, nonce := a.now(), generateID(32)
	if err := a.store.startEmailChange(ctx, user.ID, nonce, now.Add(VerificationTokenTTL), now); err != nil {
		return err
	}
	link := a.verificationLink(a.signChangeEmailToken(user.ID, user.Email, newEmail, nonce, VerificationTokenTTL), callbackURL)
	a.sendMail(ctx, "change-email", newEmail, func(ctx context.Context) error {
		return a.mailer.SendVerificationEmail(ctx, newEmail, link)
	})
	a.sendMail(ctx, "change-email-notice", user.Email, func(ctx context.Context) error {
		return a.mailer.SendEmailChangeNotice(ctx, user.Email, newEmail)
	})
	return nil
}

// confirmEmailChange applies a change-email link: the pending change is
// consumed, the account moves to the new address (verified) and the opener is
// signed in, keeping their session when it already belongs to that account.
// A browser signed in as someone else gets ErrInvalidUser instead of being
// switched into this account.
func (a *Auth) confirmEmailChange(ctx context.Context, c verificationClaims) (User, Session, error) {
	if current, _, ok := a.userFrom(ctx); ok && current.ID != c.UserID {
		return User{}, Session{}, ErrInvalidUser
	}
	if c.UserID == "" || c.Nonce == "" {
		return User{}, Session{}, ErrInvalidToken
	}
	user, err := a.store.changeEmail(ctx, c.UserID, c.Nonce, c.Email, c.UpdateTo, a.now())
	if errors.Is(err, errNotFound) {
		return User{}, Session{}, ErrInvalidToken // used, cancelled, superseded, or the email changed since
	}
	if isUniqueViolation(err) {
		return User{}, Session{}, ErrEmailTaken
	}
	if err != nil {
		return User{}, Session{}, err
	}
	if current, sess, ok := a.userFrom(ctx); ok && current.ID == user.ID {
		return user, sess, nil
	}
	if needs, err := a.needsSecondFactor(ctx, user.ID); err != nil || needs {
		return user, Session{}, err // an email link is no second factor: sign in normally
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
// currentSessionID and cancels pending email changes, which another session
// may have started.
func (a *Auth) RevokeOtherSessions(ctx context.Context, userID, currentSessionID string) error {
	return a.store.tx(ctx, func(q querier) error {
		if err := a.store.deleteOtherSessions(ctx, q, userID, currentSessionID); err != nil {
			return err
		}
		return a.store.deleteEmailChanges(ctx, q, userID)
	})
}

// rateLimitUser applies rule per client IP and per user, so neither many
// accounts from one address nor one account from many addresses get more
// attempts.
func (a *Auth) rateLimitUser(ctx context.Context, endpoint, userID string, rule RateRule) error {
	if err := a.rateLimit(ctx, endpoint, rule); err != nil {
		return err
	}
	retry, err := a.limiter.Allow(ctx, "user:"+userID+"|"+endpoint, rule)
	if err != nil {
		a.log.ErrorContext(ctx, "auth: rate limiter failed", "err", err)
		return nil
	}
	if retry > 0 {
		return &RateLimitError{RetryAfter: retry}
	}
	return nil
}
