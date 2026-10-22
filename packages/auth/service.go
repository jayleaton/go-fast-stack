package auth

import (
	"context"
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
// existing email then also succeeds with a synthetic user (and no email), so
// the response does not reveal which emails are registered.
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
	duplicate := func() (User, Session, error) {
		if a.cfg.RequireEmailVerification {
			return user, Session{}, nil
		}
		return User{}, Session{}, ErrUserAlreadyExists
	}
	// Hash before looking up so both branches cost the same.
	hash, err := hashPassword(in.Password, a.argon)
	if err != nil {
		return User{}, Session{}, err
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
		a.sendVerification(ctx, email, in.CallbackURL)
		return user, Session{}, nil
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
	if err := a.store.deleteExpiredVerifications(ctx, now); err != nil {
		a.log.WarnContext(ctx, "auth: cleaning verification rows failed", "err", err)
	}
	token := generateID(24)
	if err := a.store.createVerification(ctx, "reset-password:"+token, user.ID, now.Add(ResetPasswordTokenTTL), now); err != nil {
		return err
	}
	link := a.authURL("/reset-password/"+token) + "?callbackURL=" + url.QueryEscape(redirectTo)
	a.sendMail(ctx, "password-reset", user.Email, func(ctx context.Context) error {
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
// session of the user, all in one transaction. Used, expired or unknown
// tokens return ErrInvalidToken.
func (a *Auth) ResetPassword(ctx context.Context, token, newPassword string) error {
	if err := a.rateLimit(ctx, "reset-password", RuleResetPassword); err != nil {
		return err
	}
	if !isTokenString(token) {
		return ErrInvalidToken
	}
	if err := a.checkPasswordLength(newPassword); err != nil {
		return err
	}
	hash, err := hashPassword(newPassword, a.argon)
	if err != nil {
		return err
	}
	now := a.now()
	return a.store.tx(ctx, func(q querier) error {
		userID, err := a.store.consumeVerification(ctx, q, "reset-password:"+token, now)
		if errors.Is(err, errNotFound) {
			return ErrInvalidToken
		}
		if err != nil {
			return err
		}
		if _, err := a.store.userByID(ctx, q, userID); err != nil {
			if errors.Is(err, errNotFound) {
				return ErrUserNotFound
			}
			return err
		}
		if err := a.store.deleteUserSessions(ctx, q, userID); err != nil {
			return err
		}
		return a.store.setCredentialPassword(ctx, q, userID, hash, now)
	})
}

// VerifyEmail checks an email verification token, marks the email verified
// and signs the user in (autoSignInAfterVerification). Tokens expire after 24
// hours (ErrTokenExpired). Once the email is verified the token no longer
// signs anyone in: it returns the user with a zero Session.
func (a *Auth) VerifyEmail(ctx context.Context, token string) (User, Session, error) {
	email, err := a.parseVerificationToken(token)
	if errors.Is(err, errTokenExpired) {
		return User{}, Session{}, ErrTokenExpired
	}
	if err != nil {
		return User{}, Session{}, err
	}
	user, err := a.store.userByEmail(ctx, email)
	if errors.Is(err, errNotFound) {
		return User{}, Session{}, ErrUserNotFound
	}
	if err != nil {
		return User{}, Session{}, err
	}
	verified, err := a.store.markEmailVerified(ctx, email, a.now())
	if errors.Is(err, errNotFound) {
		return user, Session{}, nil // already verified
	}
	if err != nil {
		return User{}, Session{}, err
	}
	user = verified
	if current, sess, ok := UserFrom(ctx); ok && current.ID == user.ID {
		return user, sess, nil
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
	if current, _, ok := UserFrom(ctx); ok {
		if current.Email != email {
			return ErrEmailMismatch
		}
		if current.EmailVerified {
			return ErrEmailAlreadyVerified
		}
		a.sendVerification(ctx, email, callbackURL)
		return nil
	}
	user, err := a.store.userByEmail(ctx, email)
	if err != nil && !errors.Is(err, errNotFound) {
		return err
	}
	if err == nil && !user.EmailVerified {
		a.sendVerification(ctx, email, callbackURL)
	}
	return nil
}

func (a *Auth) sendVerification(ctx context.Context, email, callbackURL string) {
	if callbackURL == "" {
		callbackURL = "/"
	}
	link := a.authURL("/verify-email") + "?token=" + a.signVerificationToken(email, VerificationTokenTTL) +
		"&callbackURL=" + url.QueryEscape(callbackURL)
	a.sendMail(ctx, "verification", email, func(ctx context.Context) error {
		return a.mailer.SendVerificationEmail(ctx, email, link)
	})
}
