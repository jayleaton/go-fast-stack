package auth

import (
	"context"
	"crypto/hmac"
	"errors"
	"net/mail"
	"net/url"
	"strings"
)

// SignUpInput is the body of POST /sign-up/email.
type SignUpInput struct {
	Name        string  `json:"name"`
	Email       string  `json:"email"`
	Password    string  `json:"password"`
	Image       *string `json:"image,omitempty"`
	CallbackURL string  `json:"callbackURL,omitempty"` // where the verification link lands after verifying
}

// SignInInput is the body of POST /sign-in/email.
type SignInInput struct {
	Email       string `json:"email"`
	Password    string `json:"password"`
	CallbackURL string `json:"callbackURL,omitempty"`
}

// normalizeEmail lowercases a syntactically valid bare address.
func normalizeEmail(email string) (string, error) {
	if len(email) > 254 {
		return "", ErrInvalidEmail
	}
	addr, err := mail.ParseAddress(email)
	if err != nil || addr.Name != "" || addr.Address != email || !strings.Contains(addr.Address[strings.LastIndexByte(addr.Address, '@'):], ".") {
		return "", ErrInvalidEmail
	}
	return strings.ToLower(email), nil
}

// SignUpEmail creates a user with a credential account. Without required
// email verification it signs them in and returns the session. With it, it
// sends a verification link and returns a zero Session; signing up with an
// existing email then also succeeds with a synthetic user, so the response
// does not reveal which emails are registered. If that account is still
// unverified, nobody has proven they own it: the new password replaces the
// old one and a fresh link goes to the inbox. That stops a squatter who signed
// up with someone else's email from keeping access after the owner verifies.
func (a *Auth) SignUpEmail(ctx context.Context, in SignUpInput) (User, Session, error) {
	if err := a.rateLimit(ctx, "sign-up", RuleSignIn); err != nil {
		return User{}, Session{}, err
	}
	email, err := normalizeEmail(in.Email)
	if err != nil {
		return User{}, Session{}, err
	}
	if err := a.checkPasswordLength(in.Password); err != nil {
		return User{}, Session{}, err
	}
	if err := a.checkCallbackURL(in.CallbackURL, ErrInvalidCallbackURL); err != nil {
		return User{}, Session{}, err
	}

	now := a.now()
	user := User{ID: generateID(32), Name: in.Name, Email: email, Image: in.Image, CreatedAt: now, UpdatedAt: now}
	// Hash before looking up so both branches cost the same.
	hash, err := hashPassword(in.Password, a.argon)
	if err != nil {
		return User{}, Session{}, err
	}
	duplicate := func() (User, Session, error) {
		if !a.cfg.RequireEmailVerification {
			return User{}, Session{}, ErrUserAlreadyExists
		}
		err := a.store.replaceUnverifiedPassword(ctx, email, hash, now)
		if err == nil {
			a.sendVerification(ctx, email, a.credFingerprint(&hash), in.CallbackURL)
		} else if !errors.Is(err, errNotFound) {
			return User{}, Session{}, err
		}
		return user, Session{}, nil
	}
	if _, err := a.store.userByEmail(ctx, email); err == nil {
		return duplicate()
	} else if !errors.Is(err, errNotFound) {
		return User{}, Session{}, err
	}
	err = a.store.tx(ctx, func(q querier) error {
		if err := a.store.createUser(ctx, q, user); err != nil {
			return err
		}
		return a.store.setCredentialPassword(ctx, q, user.ID, hash, now)
	})
	if isUniqueViolation(err) {
		return duplicate()
	}
	if err != nil {
		return User{}, Session{}, err
	}

	if a.cfg.RequireEmailVerification {
		a.sendVerification(ctx, email, a.credFingerprint(&hash), in.CallbackURL)
		return user, Session{}, nil
	}
	if c, err := a.secondFactor(ctx, user.ID); err != nil {
		return User{}, Session{}, err
	} else if c != nil {
		return user, Session{}, c // TwoFactorConfig.Required: enrol before the first session
	}
	sess, err := a.createSession(ctx, user.ID)
	if err != nil {
		return User{}, Session{}, err
	}
	return user, sess, nil
}

// SignInEmail checks the email and password and starts a session. Unknown
// emails and wrong passwords both return ErrInvalidEmailOrPassword after the
// same amount of hashing work. With required verification, unverified users
// get ErrEmailNotVerified (only after the password matched).
func (a *Auth) SignInEmail(ctx context.Context, in SignInInput) (User, Session, error) {
	if err := a.rateLimit(ctx, "sign-in", RuleSignIn); err != nil {
		return User{}, Session{}, err
	}
	email, err := normalizeEmail(in.Email)
	if err != nil {
		return User{}, Session{}, err
	}
	if len([]rune(in.Password)) > a.cfg.MaxPasswordLength {
		return User{}, Session{}, ErrPasswordTooLong
	}
	if err := a.checkCallbackURL(in.CallbackURL, ErrInvalidCallbackURL); err != nil {
		return User{}, Session{}, err
	}

	user, hash, err := a.store.userWithPassword(ctx, email)
	if err != nil && !errors.Is(err, errNotFound) {
		return User{}, Session{}, err
	}
	if err != nil || hash == nil {
		_, _ = verifyPassword(in.Password, a.dummyHash)
		return User{}, Session{}, ErrInvalidEmailOrPassword
	}
	ok, err := verifyPassword(in.Password, *hash)
	if err != nil {
		a.log.ErrorContext(ctx, "auth: stored password hash is unreadable", "userId", user.ID, "err", err)
		return User{}, Session{}, ErrInvalidEmailOrPassword
	}
	if !ok {
		return User{}, Session{}, ErrInvalidEmailOrPassword
	}
	if isLegacyHash(*hash) {
		a.upgradeHash(ctx, user.ID, in.Password)
	}
	if a.cfg.RequireEmailVerification && !user.EmailVerified {
		return User{}, Session{}, ErrEmailNotVerified
	}
	if c, err := a.secondFactor(ctx, user.ID); err != nil {
		return User{}, Session{}, err
	} else if c != nil {
		return User{}, Session{}, c
	}
	sess, err := a.createSession(ctx, user.ID)
	if err != nil {
		return User{}, Session{}, err
	}
	return user, sess, nil
}

// upgradeHash replaces an imported Better Auth scrypt hash with argon2id after
// a successful sign-in. Failures are logged; the old hash keeps working.
func (a *Auth) upgradeHash(ctx context.Context, userID, password string) {
	hash, err := hashPassword(password, a.argon)
	if err == nil {
		err = a.store.setCredentialPassword(ctx, a.store.pool, userID, hash, a.now())
	}
	if err != nil {
		a.log.WarnContext(ctx, "auth: upgrading password hash failed", "userId", userID, "err", err)
	}
}

// SignOut deletes the session with sessionToken. Unknown tokens are ignored.
func (a *Auth) SignOut(ctx context.Context, sessionToken string) error {
	if sessionToken == "" {
		return nil
	}
	return a.store.deleteSession(ctx, sessionToken)
}

// RequestPasswordReset emails a single-use reset link valid for one hour if
// the email belongs to a user, and returns nil either way. The link points at
// GET /api/auth/reset-password/{token}, which redirects to redirectTo (a
// trusted URL or relative path, e.g. "/reset-password") with ?token=.
func (a *Auth) RequestPasswordReset(ctx context.Context, email, redirectTo string) error {
	if err := a.rateLimit(ctx, "request-password-reset", RuleEmailSending); err != nil {
		return err
	}
	if redirectTo == "" {
		return ErrInvalidRedirectURL // the emailed link would lead nowhere
	}
	if err := a.checkCallbackURL(redirectTo, ErrInvalidRedirectURL); err != nil {
		return err
	}
	email, err := normalizeEmail(email)
	if err != nil {
		return err
	}
	now := a.now()
	user, err := a.store.userByEmail(ctx, email)
	if errors.Is(err, errNotFound) {
		// Same database work as the found path; the email itself is sent in
		// the background either way.
		_, _ = a.store.verificationValue(ctx, "reset-password:dummy", now)
		return nil
	}
	if err != nil {
		return err
	}
	token := generateID(24)
	if err := a.store.createVerification(ctx, "reset-password:"+token, user.ID, now.Add(ResetPasswordTokenTTL), now); err != nil {
		return err
	}
	link := a.authURL("/reset-password/"+token) + "?callbackURL=" + url.QueryEscape(redirectTo)
	a.sendMail(ctx, "password-reset", user.Email, func(ctx context.Context) error {
		// Cleanup runs here, off the request path, so it adds no timing signal.
		if err := a.store.deleteExpiredVerifications(ctx, now); err != nil {
			a.log.WarnContext(ctx, "auth: cleaning verification rows failed", "err", err)
		}
		return a.mailer.SendPasswordResetEmail(ctx, user.Email, link)
	})
	return nil
}

// resetTokenValid reports whether a reset token exists and has not expired.
func (a *Auth) resetTokenValid(ctx context.Context, token string) (bool, error) {
	if !isTokenString(token) {
		return false, nil
	}
	_, err := a.store.verificationValue(ctx, "reset-password:"+token, a.now())
	if errors.Is(err, errNotFound) {
		return false, nil
	}
	return err == nil, err
}

// ResetPassword spends a reset token, sets the new password and revokes every
// session, every other outstanding reset link, any pending email change,
// pending 2FA sign-in and trusted device of the user, all in one transaction.
// Used, expired or unknown tokens return ErrInvalidToken. It does not sign in;
// CompleteSetPassword does.
func (a *Auth) ResetPassword(ctx context.Context, token, newPassword string) error {
	_, err := a.setPassword(ctx, token, newPassword, false)
	return err
}

// setPassword is ResetPassword returning the user id; verifyEmail also marks
// the email verified (invites: the link proves the inbox).
func (a *Auth) setPassword(ctx context.Context, token, newPassword string, verifyEmail bool) (string, error) {
	if err := a.rateLimit(ctx, "reset-password", RuleResetPassword); err != nil {
		return "", err
	}
	if !isTokenString(token) {
		return "", ErrInvalidToken
	}
	if err := a.checkPasswordLength(newPassword); err != nil {
		return "", err
	}
	hash, err := hashPassword(newPassword, a.argon)
	if err != nil {
		return "", err
	}
	now := a.now()
	var userID string
	err = a.store.tx(ctx, func(q querier) error {
		id, err := a.store.consumeVerification(ctx, q, "reset-password:"+token, now)
		if errors.Is(err, errNotFound) {
			return ErrInvalidToken
		}
		if err != nil {
			return err
		}
		userID = id
		if _, err := a.store.userByID(ctx, q, id); err != nil {
			if errors.Is(err, errNotFound) {
				return ErrUserNotFound
			}
			return err
		}
		for _, revoke := range []func(context.Context, querier, string) error{
			a.store.deleteUserSessions, a.store.deleteResetTokens, a.store.deleteEmailChanges,
			a.store.deleteChallenges, a.store.deleteTrustedDevices,
		} {
			if err := revoke(ctx, q, id); err != nil {
				return err
			}
		}
		if verifyEmail {
			if _, err := q.Exec(ctx, `UPDATE "user" SET "emailVerified" = true, "updatedAt" = $2 WHERE "id" = $1`, id, now); err != nil {
				return err
			}
		}
		return a.store.setCredentialPassword(ctx, q, id, hash, now)
	})
	return userID, err
}

// VerifyEmail checks an email verification token, marks the email verified
// and signs the user in (autoSignInAfterVerification). Tokens expire after 24
// hours (ErrTokenExpired), and are void (ErrInvalidToken) once the account's
// password changed after they were issued. Verifying revokes the account's
// other sessions. Once the email is verified the token no longer signs anyone
// in: it returns the user with a zero Session.
//
// Change-email links (see ChangeEmail) switch the account to the new address,
// mark it verified and sign in; they are single-use because the old address
// they name is gone afterwards.
func (a *Auth) VerifyEmail(ctx context.Context, token string) (User, Session, error) {
	user, sess, _, err := a.verifyEmail(ctx, token)
	return user, sess, err
}

// verifyEmail is VerifyEmail that also reports whether the link was an email
// change (Better Auth then returns the updated user).
func (a *Auth) verifyEmail(ctx context.Context, token string) (User, Session, bool, error) {
	claims, err := a.parseVerificationToken(token)
	if errors.Is(err, errTokenExpired) {
		return User{}, Session{}, false, ErrTokenExpired
	}
	if err != nil {
		return User{}, Session{}, false, err
	}
	if claims.RequestType == changeEmailRequest {
		user, sess, err := a.confirmEmailChange(ctx, claims)
		return user, sess, true, err
	}
	user, sess, err := a.verifyAddress(ctx, claims)
	return user, sess, false, err
}

// verifyAddress handles an ordinary verification link.
func (a *Auth) verifyAddress(ctx context.Context, claims verificationClaims) (User, Session, error) {
	email, cred := claims.Email, claims.Cred
	user, err := a.store.userByEmail(ctx, email)
	if errors.Is(err, errNotFound) {
		return User{}, Session{}, ErrUserNotFound
	}
	if err != nil {
		return User{}, Session{}, err
	}
	verified, err := a.store.verifyEmail(ctx, email, a.now(), func(hash *string) error {
		if !hmac.Equal([]byte(cred), []byte(a.credFingerprint(hash))) {
			return ErrInvalidToken
		}
		return nil
	})
	if errors.Is(err, errNotFound) {
		return user, Session{}, nil // already verified
	}
	if err != nil {
		return User{}, Session{}, err
	}
	user = verified
	if err := a.store.deleteUserSessions(ctx, a.store.pool, user.ID); err != nil {
		return User{}, Session{}, err
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

// SendVerificationEmail (re)sends the verification link. Signed in, the email
// must be the user's own and not yet verified. Signed out, it returns nil
// whether or not the email exists, and only unverified users get an email.
func (a *Auth) SendVerificationEmail(ctx context.Context, email, callbackURL string) error {
	if err := a.rateLimit(ctx, "send-verification-email", RuleEmailSending); err != nil {
		return err
	}
	if err := a.checkCallbackURL(callbackURL, ErrInvalidCallbackURL); err != nil {
		return err
	}
	email, err := normalizeEmail(email)
	if err != nil {
		return err
	}
	current, _, signedIn := a.userFrom(ctx)
	if signedIn {
		if current.Email != email {
			return ErrEmailMismatch
		}
		if current.EmailVerified {
			return ErrEmailAlreadyVerified
		}
	}
	user, hash, err := a.store.userWithPassword(ctx, email)
	if err != nil && !errors.Is(err, errNotFound) {
		return err
	}
	if err == nil && !user.EmailVerified {
		a.sendVerification(ctx, email, a.credFingerprint(hash), callbackURL)
	}
	return nil
}

// sendVerification emails a verification link bound to the credential
// fingerprint cred.
func (a *Auth) sendVerification(ctx context.Context, email, cred, callbackURL string) {
	if callbackURL == "" {
		callbackURL = "/"
	}
	link := a.authURL("/verify-email") + "?token=" + a.signVerificationToken(email, cred, VerificationTokenTTL) +
		"&callbackURL=" + url.QueryEscape(callbackURL)
	a.sendMail(ctx, "verification", email, func(ctx context.Context) error {
		return a.mailer.SendVerificationEmail(ctx, email, link)
	})
}
