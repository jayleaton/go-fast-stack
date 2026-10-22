package auth

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"
)

// TwoFactorConfig turns on Better Auth's twoFactor plugin (TOTP + backup
// codes) for an Auth instance.
type TwoFactorConfig struct {
	Issuer         string        // otpauth issuer shown in authenticator apps (default Config.AppName)
	Required       bool          // users without 2FA must enrol before they get a session
	TrustDeviceTTL time.Duration // "trust this device" lifetime (default 30 days); negative disables it
}

// 2FA limits, as Better Auth: a pending sign-in lives 10 minutes and allows 5
// attempts; 10 failures in a row lock the user's second factor for 15 minutes.
const (
	challengeTTL           = 10 * time.Minute
	challengeAttempts      = 5
	twoFactorMaxFailures   = 10
	twoFactorLockout       = 15 * time.Minute
	TwoFactorMethodTOTP    = "totp"
	TwoFactorMethodBackup  = "backup_code"
	setupChallengePrefix   = "setup:"
	twoFactorCookieName    = "two_factor"
	trustDeviceCookieName  = "trust_device"
	maxTwoFactorCodeLength = 32
)

// TwoFactorChallenge is returned as the error of SignInEmail (and
// CompleteSetPassword) when the user must pass a second factor, or with
// SetupRequired enrol one, before getting a session. Browsers carry Token in
// the two_factor cookie (SetTwoFactorCookie).
type TwoFactorChallenge struct {
	Token         string    `json:"-"`
	Methods       []string  `json:"twoFactorMethods"`
	SetupRequired bool      `json:"twoFactorSetupRequired"`
	ExpiresAt     time.Time `json:"-"`
}

func (c *TwoFactorChallenge) Error() string { return "auth: two-factor verification required" }

// SetTwoFactorCookie stores the challenge token for the 2FA page.
func (a *Auth) SetTwoFactorCookie(w http.ResponseWriter, c *TwoFactorChallenge) {
	a.setSignedCookie(w, a.cookie(twoFactorCookieName), c.Token, int(c.ExpiresAt.Sub(a.now()).Seconds()))
}

func (a *Auth) clearTwoFactorCookie(w http.ResponseWriter) {
	a.setSignedCookie(w, a.cookie(twoFactorCookieName), "", 0)
}

// SetTrustDeviceCookie remembers this device so sign-in skips 2FA here for
// TwoFactorConfig.TrustDeviceTTL.
func (a *Auth) SetTrustDeviceCookie(w http.ResponseWriter, token string) {
	if token == "" || a.cfg.TwoFactor == nil {
		return
	}
	a.setSignedCookie(w, a.cookie(trustDeviceCookieName), token, int(a.cfg.TwoFactor.TrustDeviceTTL.Seconds()))
}

// TOTPSetup is what a user needs to enrol an authenticator app.
type TOTPSetup struct {
	Secret      string   `json:"secret"`      // base32, for manual entry
	URI         string   `json:"totpURI"`     // otpauth:// URI for a QR code
	BackupCodes []string `json:"backupCodes"` // shown once; stored hashed
}

// VerifyTwoFactorInput is a second-factor attempt for a pending sign-in.
type VerifyTwoFactorInput struct {
	Code           string
	Method         string // TwoFactorMethodTOTP (default) or TwoFactorMethodBackup
	TrustDevice    bool
	ChallengeToken string // empty: the two_factor cookie of the request (Middleware)
}

// TwoFactorResult is a completed sign-in.
type TwoFactorResult struct {
	User       User
	Session    Session
	TrustToken string // set when TrustDevice was asked for: SetTrustDeviceCookie
}

var errTwoFactorOff = &Error{http.StatusNotFound, "NOT_FOUND", "Two-factor authentication is not enabled on this server"}

func (a *Auth) twoFactorOn() error {
	if a.cfg.TwoFactor == nil {
		return errTwoFactorOff
	}
	return nil
}

// secondFactor decides what userID needs after proving their password (or
// an email link): nil when a session may be created now, or a challenge.
// A device the user trusted skips the challenge.
func (a *Auth) secondFactor(ctx context.Context, userID string) (*TwoFactorChallenge, error) {
	needs, err := a.needsSecondFactor(ctx, userID)
	if err != nil || !needs {
		return nil, err
	}
	enabled, err := a.store.twoFactorEnabled(ctx, userID)
	if err != nil {
		return nil, err
	}
	if err := a.checkBan(ctx, userID); err != nil {
		return nil, err // no challenge for banned users
	}
	value, methods := userID, []string{TwoFactorMethodTOTP, TwoFactorMethodBackup}
	if !enabled {
		value, methods = setupChallengePrefix+userID, []string{}
	}
	now := a.now()
	c := &TwoFactorChallenge{Token: generateID(32), Methods: methods, SetupRequired: !enabled, ExpiresAt: now.Add(challengeTTL)}
	if err := a.store.createVerification(ctx, challengePrefix+c.Token, value, c.ExpiresAt, now); err != nil {
		return nil, err
	}
	return c, nil
}

// needsSecondFactor reports whether userID must pass (or enrol) a second
// factor before getting a session on this request.
func (a *Auth) needsSecondFactor(ctx context.Context, userID string) (bool, error) {
	tf := a.cfg.TwoFactor
	if tf == nil {
		return false, nil
	}
	enabled, err := a.store.twoFactorEnabled(ctx, userID)
	if err != nil {
		return false, err
	}
	if enabled {
		return !a.trustedDevice(ctx, userID), nil
	}
	return tf.Required, nil
}

// trustedDevice reports whether the request carries a valid trust-device
// cookie for userID.
func (a *Auth) trustedDevice(ctx context.Context, userID string) bool {
	m := a.meta(ctx)
	if m == nil || m.trustToken == "" || a.cfg.TwoFactor.TrustDeviceTTL <= 0 || !isTokenString(m.trustToken) {
		return false
	}
	owner, err := a.store.verificationValue(ctx, trustDevicePrefix+m.trustToken, a.now())
	return err == nil && owner == userID
}

// challenge resolves a pending sign-in: the user id and whether it is an
// enrolment (setup) challenge.
func (a *Auth) challenge(ctx context.Context, token string) (string, bool, error) {
	if token == "" {
		if m := a.meta(ctx); m != nil {
			token = m.twoFactorToken
		}
	}
	if !isTokenString(token) {
		return "", false, ErrInvalidTwoFactorCookie
	}
	value, err := a.store.verificationValue(ctx, challengePrefix+token, a.now())
	if errors.Is(err, errNotFound) {
		return "", false, ErrInvalidTwoFactorCookie
	}
	if err != nil {
		return "", false, err
	}
	if id, ok := strings.CutPrefix(value, setupChallengePrefix); ok {
		return id, true, nil
	}
	return value, false, nil
}

func (a *Auth) challengeToken(ctx context.Context, token string) string {
	if token == "" {
		if m := a.meta(ctx); m != nil {
			return m.twoFactorToken
		}
	}
	return token
}

// pendingFor reports whether the request carries a live 2FA challenge for
// userID (a signed-in user signing in again), as opposed to none, a stale one
// or another user's.
func (a *Auth) pendingFor(ctx context.Context, userID string) bool {
	if m := a.meta(ctx); m == nil || m.twoFactorToken == "" {
		return false
	}
	id, _, err := a.challenge(ctx, "")
	return err == nil && id == userID
}

// VerifyTwoFactor completes a pending sign-in with a TOTP or backup code and
// creates the session. For an enrolment (SetupRequired) challenge the first
// valid TOTP code also turns 2FA on. A challenge allows 5 attempts and is
// spent on success; a TOTP code is accepted once.
func (a *Auth) VerifyTwoFactor(ctx context.Context, in VerifyTwoFactorInput) (TwoFactorResult, error) {
	res, att, err := a.verifyTwoFactor(ctx, in)
	if att.userID == "" {
		return res, err // no valid pending sign-in: nobody to report it about
	}
	if !att.accepted {
		a.emitFailure(ctx, EventTwoFactorFailed, att.userID, att.method, err)
		return res, err
	}
	a.emit(ctx, Event{Kind: EventTwoFactorSucceeded, UserID: att.userID, Method: att.method})
	if att.enabled {
		a.emit(ctx, Event{Kind: EventTwoFactorEnabled, UserID: att.userID})
	}
	if err != nil {
		// The factor was right but no session came of it (spent challenge,
		// ban, database error).
		a.emitFailure(ctx, EventSignInFailed, att.userID, SignInMethodTwoFactor, err)
		return res, err
	}
	a.emit(ctx, Event{Kind: EventSignIn, UserID: res.User.ID, Email: res.User.Email, SessionID: res.Session.ID,
		Method: SignInMethodTwoFactor, Details: map[string]string{"factor": att.method}})
	return res, nil
}

// twoFactorAttempt is what an attempt is known to be about, for events: the
// user from the pending sign-in (empty if the challenge was not valid),
// whether the code was accepted and whether that turned 2FA on.
type twoFactorAttempt struct {
	userID, method    string
	accepted, enabled bool
}

func (a *Auth) verifyTwoFactor(ctx context.Context, in VerifyTwoFactorInput) (_ TwoFactorResult, att twoFactorAttempt, _ error) {
	att.method = TwoFactorMethodTOTP
	if in.Method == TwoFactorMethodBackup {
		att.method = TwoFactorMethodBackup
	} else if in.Method != "" && in.Method != TwoFactorMethodTOTP {
		return TwoFactorResult{}, att, ErrInvalidCode
	}
	if err := a.twoFactorOn(); err != nil {
		return TwoFactorResult{}, att, err
	}
	if err := a.rateLimit(ctx, "two-factor", RuleSignIn); err != nil {
		return TwoFactorResult{}, att, err
	}
	token := a.challengeToken(ctx, in.ChallengeToken)
	userID, setup, err := a.challenge(ctx, token)
	if err != nil {
		return TwoFactorResult{}, att, err
	}
	att.userID = userID
	if retry, err := a.limiter.Allow(ctx, "2fa-challenge|"+token, RateRule{Window: challengeTTL, Max: challengeAttempts}); err == nil && retry > 0 {
		return TwoFactorResult{}, att, &RateLimitError{RetryAfter: retry}
	}
	if setup {
		att.method = TwoFactorMethodTOTP
	}
	method := att.method
	if att.enabled, err = a.checkSecondFactor(ctx, userID, method, in.Code, !setup); err != nil {
		return TwoFactorResult{}, att, err
	}
	att.accepted = true
	// Spend the challenge: of two concurrent successes only one gets a session.
	var consumed string
	if err := a.store.tx(ctx, func(q querier) error {
		consumed, err = a.store.consumeVerification(ctx, q, challengePrefix+token, a.now())
		return err
	}); err != nil || (consumed != userID && consumed != setupChallengePrefix+userID) {
		if method == TwoFactorMethodBackup {
			// The code bought nothing: give it back.
			if err := a.store.restoreBackupCode(ctx, userID, a.hashBackupCode(userID, in.Code), a.now()); err != nil {
				a.log.ErrorContext(ctx, "auth: restoring backup code failed", "userId", userID, "err", err)
			}
		}
		return TwoFactorResult{}, att, ErrInvalidTwoFactorCookie
	}
	user, err := a.store.userByID(ctx, a.store.pool, userID)
	if err != nil {
		return TwoFactorResult{}, att, err
	}
	sess, err := a.createSession(ctx, userID)
	if err != nil {
		return TwoFactorResult{}, att, err
	}
	res := TwoFactorResult{User: user, Session: sess}
	if in.TrustDevice && a.cfg.TwoFactor.TrustDeviceTTL > 0 {
		res.TrustToken = generateID(32)
		now := a.now()
		if err := a.store.createVerification(ctx, trustDevicePrefix+res.TrustToken, userID, now.Add(a.cfg.TwoFactor.TrustDeviceTTL), now); err != nil {
			// The sign-in stands; this device just is not remembered.
			a.log.ErrorContext(ctx, "auth: remembering trusted device failed", "userId", userID, "err", err)
			res.TrustToken = ""
		}
	}
	return res, att, nil
}

// checkSecondFactor verifies code for userID, enforcing the lockout. With
// requireVerified the enrolment must already be confirmed (sign-in);
// otherwise a TOTP code confirms it (enrolment).
func (a *Auth) checkSecondFactor(ctx context.Context, userID, method, code string, requireVerified bool) (enabled bool, _ error) {
	if len(code) > maxTwoFactorCodeLength {
		return false, ErrInvalidCode
	}
	row, err := a.store.twoFactor(ctx, userID)
	if errors.Is(err, errNotFound) || (err == nil && requireVerified && !row.verified) {
		return false, ErrTwoFactorNotEnabled
	}
	if err != nil {
		return false, err
	}
	now := a.now()
	if row.lockedUntil != nil {
		if row.lockedUntil.After(now) {
			return false, ErrTwoFactorLocked
		}
		// The lock has run out: start counting failures afresh.
		if err := a.store.resetTwoFactorFailures(ctx, userID, now); err != nil {
			return false, err
		}
	}
	fail := func(e error) error {
		if err := a.store.recordTwoFactorFailure(ctx, userID, twoFactorMaxFailures, twoFactorLockout, now); err != nil {
			return err
		}
		return e
	}
	switch method {
	case TwoFactorMethodTOTP:
		key, err := a.openTOTPKey(userID, row.secret)
		if err != nil {
			return false, err
		}
		step, ok := matchTOTP(key, code, now)
		if !ok {
			return false, fail(ErrInvalidCode)
		}
		on, err := a.store.acceptTOTPStep(ctx, userID, row.secret, step, now)
		if errors.Is(err, errNotFound) {
			return false, fail(ErrInvalidCode) // replayed, or the enrolment was replaced meanwhile
		} else if err != nil {
			return false, err
		}
		return on, nil
	case TwoFactorMethodBackup:
		if !row.verified {
			return false, ErrTwoFactorNotEnabled
		}
		if err := a.store.useBackupCode(ctx, userID, a.hashBackupCode(userID, code), now); errors.Is(err, errNotFound) {
			return false, fail(ErrInvalidBackupCode)
		} else if err != nil {
			return false, err
		}
		return false, nil
	default:
		return false, ErrInvalidCode
	}
}

// newSetup creates an unverified enrolment for user and returns what to show.
func (a *Auth) newSetup(ctx context.Context, user User) (TOTPSetup, error) {
	key := newTOTPKey()
	sealed, err := a.sealTOTPKey(user.ID, key)
	if err != nil {
		return TOTPSetup{}, err
	}
	codes := newBackupCodes()
	hashes := make([]string, len(codes))
	for i, c := range codes {
		hashes[i] = a.hashBackupCode(user.ID, c)
	}
	if err := a.store.startTwoFactor(ctx, user.ID, sealed, hashes, a.now()); errors.Is(err, errAlreadyEnabled) {
		return TOTPSetup{}, ErrTOTPAlreadyEnabled
	} else if err != nil {
		return TOTPSetup{}, err
	}
	return TOTPSetup{Secret: b32.EncodeToString(key), URI: totpURI(key, a.cfg.TwoFactor.Issuer, user.Email), BackupCodes: codes}, nil
}

// EnableTwoFactor starts enrolment for a signed-in user after checking their
// password. 2FA becomes active once ConfirmTwoFactor accepts a code; until
// then calling it again replaces the secret.
func (a *Auth) EnableTwoFactor(ctx context.Context, userID, password string) (TOTPSetup, error) {
	if err := a.twoFactorOn(); err != nil {
		return TOTPSetup{}, err
	}
	if err := a.rateLimitUser(ctx, "two-factor", userID, RuleSignIn); err != nil {
		return TOTPSetup{}, err
	}
	if err := a.checkUserPassword(ctx, userID, password); err != nil {
		return TOTPSetup{}, err
	}
	user, err := a.store.userByID(ctx, a.store.pool, userID)
	if err != nil {
		return TOTPSetup{}, err
	}
	return a.newSetup(ctx, user)
}

// StartTwoFactorSetup starts enrolment for a SetupRequired challenge (empty
// token: the request's two_factor cookie). Finish with VerifyTwoFactor.
func (a *Auth) StartTwoFactorSetup(ctx context.Context, challengeToken string) (TOTPSetup, error) {
	if err := a.twoFactorOn(); err != nil {
		return TOTPSetup{}, err
	}
	if err := a.rateLimit(ctx, "two-factor", RuleSignIn); err != nil {
		return TOTPSetup{}, err
	}
	userID, setup, err := a.challenge(ctx, challengeToken)
	if err != nil {
		return TOTPSetup{}, err
	}
	if !setup {
		return TOTPSetup{}, ErrTOTPAlreadyEnabled
	}
	user, err := a.store.userByID(ctx, a.store.pool, userID)
	if err != nil {
		return TOTPSetup{}, err
	}
	return a.newSetup(ctx, user)
}

// ConfirmTwoFactor turns 2FA on for a signed-in user whose enrolment
// (EnableTwoFactor) is pending, using the first code from their app.
func (a *Auth) ConfirmTwoFactor(ctx context.Context, userID, code string) error {
	if err := a.twoFactorOn(); err != nil {
		return err
	}
	if err := a.rateLimitUser(ctx, "two-factor", userID, RuleSignIn); err != nil {
		return err
	}
	enabled, err := a.checkSecondFactor(ctx, userID, TwoFactorMethodTOTP, code, false)
	if err != nil {
		a.emitFailure(ctx, EventTwoFactorFailed, userID, TwoFactorMethodTOTP, err)
		return err
	}
	a.emit(ctx, Event{Kind: EventTwoFactorSucceeded, UserID: userID, Method: TwoFactorMethodTOTP})
	if enabled {
		a.emit(ctx, Event{Kind: EventTwoFactorEnabled, UserID: userID})
	}
	return nil
}

// DisableTwoFactor turns 2FA off after checking the password, and forgets the
// user's trusted devices.
func (a *Auth) DisableTwoFactor(ctx context.Context, userID, password string) error {
	if err := a.twoFactorOn(); err != nil {
		return err
	}
	if err := a.rateLimitUser(ctx, "two-factor", userID, RuleSignIn); err != nil {
		return err
	}
	if err := a.checkUserPassword(ctx, userID, password); err != nil {
		return err
	}
	if _, err := a.store.twoFactor(ctx, userID); errors.Is(err, errNotFound) {
		return ErrTwoFactorNotEnabled
	} else if err != nil {
		return err
	}
	if err := a.store.disableTwoFactor(ctx, userID, a.now()); err != nil {
		return err
	}
	a.emit(ctx, Event{Kind: EventTwoFactorDisabled, UserID: userID})
	return nil
}

// GenerateBackupCodes replaces the user's backup codes after checking the
// password; the old codes stop working.
func (a *Auth) GenerateBackupCodes(ctx context.Context, userID, password string) ([]string, error) {
	if err := a.twoFactorOn(); err != nil {
		return nil, err
	}
	if err := a.rateLimitUser(ctx, "two-factor", userID, RuleSignIn); err != nil {
		return nil, err
	}
	if err := a.checkUserPassword(ctx, userID, password); err != nil {
		return nil, err
	}
	codes := newBackupCodes()
	hashes := make([]string, len(codes))
	for i, c := range codes {
		hashes[i] = a.hashBackupCode(userID, c)
	}
	if err := a.store.setBackupCodes(ctx, userID, hashes, a.now()); errors.Is(err, errNotFound) {
		return nil, ErrTwoFactorNotEnabled
	} else if err != nil {
		return nil, err
	}
	return codes, nil
}

// TwoFactorEnabled reports whether the user has 2FA turned on.
func (a *Auth) TwoFactorEnabled(ctx context.Context, userID string) (bool, error) {
	on, err := a.store.twoFactorEnabled(ctx, userID)
	if errors.Is(err, errNotFound) {
		return false, ErrUserNotFound
	}
	return on, err
}

// totpURIFor rebuilds the otpauth URI of a user's enrolment (get-totp-uri).
func (a *Auth) totpURIFor(ctx context.Context, user User) (string, error) {
	row, err := a.store.twoFactor(ctx, user.ID)
	if errors.Is(err, errNotFound) {
		return "", ErrTwoFactorNotEnabled
	}
	if err != nil {
		return "", err
	}
	key, err := a.openTOTPKey(user.ID, row.secret)
	if err != nil {
		return "", err
	}
	return totpURI(key, a.cfg.TwoFactor.Issuer, user.Email), nil
}

// checkUserPassword verifies a signed-in user's password for a sensitive
// change.
func (a *Auth) checkUserPassword(ctx context.Context, userID, password string) error {
	if len([]rune(password)) > a.cfg.MaxPasswordLength {
		return ErrInvalidPassword
	}
	hash, err := a.store.credentialHash(ctx, userID)
	if errors.Is(err, errNotFound) {
		return ErrCredentialAccountNotFound
	}
	if err != nil {
		return err
	}
	ok, err := verifyPassword(password, hash)
	if err != nil || !ok {
		return ErrInvalidPassword
	}
	return nil
}
