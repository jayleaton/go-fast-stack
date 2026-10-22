package auth

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

func with2FA(required bool) func(*Config) {
	return func(c *Config) { c.TwoFactor = &TwoFactorConfig{Required: required}; c.AppName = "Acme" }
}

// code returns the current TOTP code for a base32 secret at the harness clock.
func (h *harness) code(secret string) string {
	h.t.Helper()
	key, err := b32.DecodeString(secret)
	if err != nil {
		h.t.Fatal(err)
	}
	return totpCode(key, totpStep(h.clock.Now()))
}

// wrongCode returns a 6-digit code that is guaranteed invalid for secret at
// the harness clock: a fixed "000000" is valid in 3 of every million windows,
// which makes a test fail at random once in a while.
func (h *harness) wrongCode(secret string) string {
	h.t.Helper()
	key, err := b32.DecodeString(secret)
	if err != nil {
		h.t.Fatal(err)
	}
	step := totpStep(h.clock.Now())
	valid := map[string]bool{}
	for d := int64(-totpWindow - 1); d <= totpWindow+1; d++ {
		valid[totpCode(key, step+d)] = true
	}
	for i := 0; ; i++ {
		if c := fmt.Sprintf("%06d", i); !valid[c] {
			return c
		}
	}
}

// tick moves the clock to the next TOTP step so a fresh code is accepted.
func (h *harness) tick() { h.clock.Advance(totpPeriod) }

// cookies joins the Set-Cookie values of rec (minus expired ones) with base.
func cookies(rec *httptest.ResponseRecorder, base ...string) string {
	parts := append([]string(nil), base...)
	for _, c := range rec.Result().Cookies() {
		if c.MaxAge >= 0 && c.Value != "" {
			parts = append(parts, c.Name+"="+c.Value)
		}
	}
	return strings.Join(parts, "; ")
}

// enrol signs email up, enables and confirms TOTP over the JSON API, and
// returns the setup and the signed-in headers.
func (h *harness) enrol(email string) (TOTPSetup, hdr) {
	h.t.Helper()
	signedIn := hdr{"Cookie": cookieHeader(h.t, h.signUp(email, "password123")), "Origin": testBase}
	rec := h.do("POST", BasePath+"/two-factor/enable", map[string]any{"password": "password123"}, signedIn)
	wantStatus(h.t, rec, http.StatusOK)
	body := jsonBody(h.t, rec)
	setup := TOTPSetup{Secret: body["secret"].(string), URI: body["totpURI"].(string)}
	for _, c := range body["backupCodes"].([]any) {
		setup.BackupCodes = append(setup.BackupCodes, c.(string))
	}
	wantStatus(h.t, h.do("POST", BasePath+"/two-factor/verify-totp", map[string]any{"code": h.code(setup.Secret)}, signedIn), http.StatusOK)
	h.tick()
	return setup, signedIn
}

// challenge signs in and expects a 2FA challenge; it returns the cookie header.
func (h *harness) challenge(email, password string, extra ...string) string {
	h.t.Helper()
	headers := hdr{}
	if len(extra) > 0 {
		headers["Cookie"], headers["Origin"] = strings.Join(extra, "; "), testBase
	}
	rec := h.do("POST", BasePath+"/sign-in/email", map[string]any{"email": email, "password": password}, headers)
	wantStatus(h.t, rec, http.StatusOK)
	body := jsonBody(h.t, rec)
	if body["twoFactorRedirect"] != true || sessionCookie(rec) != nil {
		h.t.Fatalf("sign-in did not ask for 2FA: %v", body)
	}
	return cookies(rec, extra...)
}

func TestTwoFactorEnrolmentAndSignIn(t *testing.T) {
	t.Parallel()
	h := newHarness(t, with2FA(false))
	email := uniqueEmail(t)
	signedIn := hdr{"Cookie": cookieHeader(t, h.signUp(email, "password123")), "Origin": testBase}

	wantCode(t, h.do("POST", BasePath+"/two-factor/enable", map[string]any{"password": "wrong-password"}, signedIn), http.StatusBadRequest, "INVALID_PASSWORD")
	wantCode(t, h.do("POST", BasePath+"/two-factor/enable", map[string]any{"password": "password123"}, nil), http.StatusUnauthorized, "UNAUTHORIZED")
	rec := h.do("POST", BasePath+"/two-factor/enable", map[string]any{"password": "password123"}, signedIn)
	wantStatus(t, rec, http.StatusOK)
	body := jsonBody(t, rec)
	secret, uri := body["secret"].(string), body["totpURI"].(string)
	if len(body["backupCodes"].([]any)) != backupCodeCount || !strings.HasPrefix(uri, "otpauth://totp/Acme:") || !strings.Contains(uri, "secret="+secret) {
		t.Fatalf("enable = %v", body)
	}
	u, _ := url.Parse(uri)
	if u.Query().Get("issuer") != "Acme" || u.Query().Get("digits") != "6" {
		t.Fatalf("uri = %s", uri)
	}

	// Not active until confirmed: sign-in still gives a session.
	wantStatus(t, h.signIn(email, "password123"), http.StatusOK)
	// Someone else's pending two_factor cookie does not hijack the confirm step.
	stranger := uniqueEmail(t)
	h.enrol(stranger)
	foreign := strings.Split(h.challenge(stranger, "password123"), "; ")
	signedIn["Cookie"] += "; " + foreign[len(foreign)-1]
	wantCode(t, h.do("POST", BasePath+"/two-factor/verify-totp", map[string]any{"code": h.wrongCode(secret)}, signedIn), http.StatusUnauthorized, "INVALID_CODE")
	confirm := h.code(secret)
	wantStatus(t, h.do("POST", BasePath+"/two-factor/verify-totp", map[string]any{"code": confirm}, signedIn), http.StatusOK)
	if on, _ := h.a.TwoFactorEnabled(context.Background(), h.userID(signedIn)); !on {
		t.Fatal("2FA not enabled after confirmation")
	}
	wantCode(t, h.do("POST", BasePath+"/two-factor/enable", map[string]any{"password": "password123"}, signedIn), http.StatusBadRequest, "TOTP_ALREADY_ENABLED")
	rec = h.do("POST", BasePath+"/two-factor/get-totp-uri", map[string]any{"password": "password123"}, signedIn)
	if wantStatus(t, rec, http.StatusOK); jsonBody(t, rec)["totpURI"] != uri {
		t.Fatalf("get-totp-uri = %s", rec.Body)
	}
	wantCode(t, h.do("POST", BasePath+"/two-factor/get-totp-uri", map[string]any{"password": "wrong-password"}, signedIn), http.StatusBadRequest, "INVALID_PASSWORD")
	wantCode(t, h.do("POST", BasePath+"/two-factor/get-totp-uri", map[string]any{"password": "password123"}, nil), http.StatusUnauthorized, "UNAUTHORIZED")

	// Sign-in now stops at the second factor.
	rec = h.do("POST", BasePath+"/sign-in/email", map[string]any{"email": email, "password": "password123"}, nil)
	wantStatus(t, rec, http.StatusOK)
	body = jsonBody(t, rec)
	if body["twoFactorRedirect"] != true || len(body["twoFactorMethods"].([]any)) != 2 || body["twoFactorSetupRequired"] != false || sessionCookie(rec) != nil || body["token"] != nil {
		t.Fatalf("sign-in = %v", body)
	}
	pending := hdr{"Cookie": cookies(rec), "Origin": testBase}
	if !strings.Contains(pending["Cookie"], "better-auth.two_factor=") {
		t.Fatalf("no two_factor cookie: %v", pending)
	}

	// The code used to confirm cannot be replayed in its window.
	wantCode(t, h.do("POST", BasePath+"/two-factor/verify-totp", map[string]any{"code": confirm}, pending), http.StatusUnauthorized, "INVALID_CODE")
	wantCode(t, h.do("POST", BasePath+"/two-factor/verify-totp", map[string]any{"code": h.wrongCode(secret)}, pending), http.StatusUnauthorized, "INVALID_CODE")
	h.tick()
	code := h.code(secret)
	rec = h.do("POST", BasePath+"/two-factor/verify-totp", map[string]any{"code": code}, pending)
	wantStatus(t, rec, http.StatusOK)
	if jsonBody(t, rec)["token"] == "" || h.getSession(hdr{"Cookie": cookieHeader(t, rec)}) == nil {
		t.Fatal("verify-totp did not sign in")
	}
	// The challenge is spent, and the same code cannot open another one.
	wantCode(t, h.do("POST", BasePath+"/two-factor/verify-totp", map[string]any{"code": code}, pending), http.StatusUnauthorized, "INVALID_TWO_FACTOR_COOKIE")
	again := hdr{"Cookie": h.challenge(email, "password123"), "Origin": testBase}
	wantCode(t, h.do("POST", BasePath+"/two-factor/verify-totp", map[string]any{"code": code}, again), http.StatusUnauthorized, "INVALID_CODE")

	// A wrong password never reaches the second factor.
	wantCode(t, h.signIn(email, "wrong-password"), http.StatusUnauthorized, "INVALID_EMAIL_OR_PASSWORD")
	// Without a challenge cookie or session there is nothing to verify.
	wantCode(t, h.do("POST", BasePath+"/two-factor/verify-totp", map[string]any{"code": code}, nil), http.StatusUnauthorized, "INVALID_TWO_FACTOR_COOKIE")
}

func TestBackupCodesAreSingleUse(t *testing.T) {
	t.Parallel()
	h := newHarness(t, with2FA(false))
	email := uniqueEmail(t)
	setup, signedIn := h.enrol(email)

	pending := hdr{"Cookie": h.challenge(email, "password123"), "Origin": testBase}
	code := strings.ToUpper(setup.BackupCodes[0]) // case and dash insensitive
	wantStatus(t, h.do("POST", BasePath+"/two-factor/verify-backup-code", map[string]any{"code": strings.ReplaceAll(code, "-", "")}, pending), http.StatusOK)
	pending = hdr{"Cookie": h.challenge(email, "password123"), "Origin": testBase}
	wantCode(t, h.do("POST", BasePath+"/two-factor/verify-backup-code", map[string]any{"code": setup.BackupCodes[0]}, pending), http.StatusUnauthorized, "INVALID_BACKUP_CODE")
	wantStatus(t, h.do("POST", BasePath+"/two-factor/verify-backup-code", map[string]any{"code": setup.BackupCodes[1]}, pending), http.StatusOK)

	// Regenerating needs the password and voids the old codes.
	wantCode(t, h.do("POST", BasePath+"/two-factor/generate-backup-codes", map[string]any{"password": "nope-nope"}, signedIn), http.StatusBadRequest, "INVALID_PASSWORD")
	rec := h.do("POST", BasePath+"/two-factor/generate-backup-codes", map[string]any{"password": "password123"}, signedIn)
	wantStatus(t, rec, http.StatusOK)
	fresh := jsonBody(t, rec)["backupCodes"].([]any)
	pending = hdr{"Cookie": h.challenge(email, "password123"), "Origin": testBase}
	wantCode(t, h.do("POST", BasePath+"/two-factor/verify-backup-code", map[string]any{"code": setup.BackupCodes[2]}, pending), http.StatusUnauthorized, "INVALID_BACKUP_CODE")
	wantStatus(t, h.do("POST", BasePath+"/two-factor/verify-backup-code", map[string]any{"code": fresh[0]}, pending), http.StatusOK)

	// Another user's code is not accepted.
	other := uniqueEmail(t)
	otherSetup, _ := h.enrol(other)
	pending = hdr{"Cookie": h.challenge(email, "password123"), "Origin": testBase}
	wantCode(t, h.do("POST", BasePath+"/two-factor/verify-backup-code", map[string]any{"code": otherSetup.BackupCodes[0]}, pending), http.StatusUnauthorized, "INVALID_BACKUP_CODE")
}

func TestTrustedDeviceAndDisable(t *testing.T) {
	t.Parallel()
	h := newHarness(t, with2FA(false))
	email := uniqueEmail(t)
	setup, signedIn := h.enrol(email)

	pending := hdr{"Cookie": h.challenge(email, "password123"), "Origin": testBase}
	rec := h.do("POST", BasePath+"/two-factor/verify-totp", map[string]any{"code": h.code(setup.Secret), "trustDevice": true}, pending)
	wantStatus(t, rec, http.StatusOK)
	var trust string
	for _, c := range rec.Result().Cookies() {
		if c.Name == "better-auth.trust_device" && c.MaxAge > 0 {
			trust = c.Name + "=" + c.Value
		}
	}
	if trust == "" {
		t.Fatal("no trust_device cookie")
	}
	// The trusted device skips 2FA; another device does not.
	rec = h.do("POST", BasePath+"/sign-in/email", map[string]any{"email": email, "password": "password123"}, hdr{"Cookie": trust, "Origin": testBase})
	wantStatus(t, rec, http.StatusOK)
	if sessionCookie(rec) == nil {
		t.Fatalf("trusted device still challenged: %s", rec.Body)
	}
	h.challenge(email, "password123")
	// A trust cookie is bound to its user.
	other := uniqueEmail(t)
	h.enrol(other)
	h.challenge(other, "password123", trust)

	// Disabling needs the password, turns 2FA off and forgets trusted devices.
	wantCode(t, h.do("POST", BasePath+"/two-factor/disable", map[string]any{"password": "wrong-password"}, signedIn), http.StatusBadRequest, "INVALID_PASSWORD")
	wantStatus(t, h.do("POST", BasePath+"/two-factor/disable", map[string]any{"password": "password123"}, signedIn), http.StatusOK)
	wantCode(t, h.do("POST", BasePath+"/two-factor/disable", map[string]any{"password": "password123"}, signedIn), http.StatusBadRequest, "TWO_FACTOR_NOT_ENABLED")
	wantStatus(t, h.signIn(email, "password123"), http.StatusOK)
	if h.a.trustedDevice(context.WithValue(context.Background(), ctxKey{}, &requestMeta{trustToken: strings.SplitN(trust, "=", 2)[1]}), h.userID(signedIn)) {
		t.Fatal("trusted device survived disable")
	}
}

func TestTwoFactorLockoutExpiryAndOff(t *testing.T) {
	t.Parallel()
	h := newHarness(t, with2FA(false))
	ctx := context.Background()
	email := uniqueEmail(t)
	setup, signedIn := h.enrol(email)
	id := h.userID(signedIn)

	// Ten wrong codes lock the second factor for 15 minutes, even for the right code.
	for range twoFactorMaxFailures {
		c := hdr{"Cookie": h.challenge(email, "password123"), "Origin": testBase}
		wantCode(t, h.do("POST", BasePath+"/two-factor/verify-totp", map[string]any{"code": h.wrongCode(setup.Secret)}, c), http.StatusUnauthorized, "INVALID_CODE")
	}
	locked := hdr{"Cookie": h.challenge(email, "password123"), "Origin": testBase}
	wantCode(t, h.do("POST", BasePath+"/two-factor/verify-totp", map[string]any{"code": h.code(setup.Secret)}, locked), http.StatusTooManyRequests, "ACCOUNT_TEMPORARILY_LOCKED")
	h.clock.Advance(twoFactorLockout + time.Minute)
	// After the lock runs out one typo does not lock again: counting restarts.
	typo := hdr{"Cookie": h.challenge(email, "password123"), "Origin": testBase}
	wantCode(t, h.do("POST", BasePath+"/two-factor/verify-totp", map[string]any{"code": h.wrongCode(setup.Secret)}, typo), http.StatusUnauthorized, "INVALID_CODE")
	unlocked := hdr{"Cookie": h.challenge(email, "password123"), "Origin": testBase}
	wantStatus(t, h.do("POST", BasePath+"/two-factor/verify-totp", map[string]any{"code": h.code(setup.Secret)}, unlocked), http.StatusOK)

	// A challenge expires after 10 minutes.
	h.tick()
	stale := hdr{"Cookie": h.challenge(email, "password123"), "Origin": testBase}
	h.clock.Advance(challengeTTL + time.Second)
	wantCode(t, h.do("POST", BasePath+"/two-factor/verify-totp", map[string]any{"code": h.code(setup.Secret)}, stale), http.StatusUnauthorized, "INVALID_TWO_FACTOR_COOKIE")

	// Over-long and malformed codes are rejected without a lookup surprise.
	if _, err := h.a.VerifyTwoFactor(ctx, VerifyTwoFactorInput{ChallengeToken: "not a token", Code: "1"}); !errors.Is(err, ErrInvalidTwoFactorCookie) {
		t.Fatalf("bad token err = %v", err)
	}
	if err := h.a.ConfirmTwoFactor(ctx, id, strings.Repeat("1", 64)); !errors.Is(err, ErrInvalidCode) {
		t.Fatalf("long code err = %v", err)
	}
	if _, err := h.a.GenerateBackupCodes(ctx, generateID(32), "password123"); !errors.Is(err, ErrCredentialAccountNotFound) {
		t.Fatalf("unknown user err = %v", err)
	}

	// An instance without TwoFactor serves no 2FA endpoints and ignores enrolments.
	off := newHarness(t, nil)
	wantCode(t, off.do("POST", BasePath+"/two-factor/enable", map[string]any{"password": "password123"}, signedIn), http.StatusNotFound, "NOT_FOUND")
	wantStatus(t, off.signIn(email, "password123"), http.StatusOK)
}

func TestRequiredTwoFactorEnrolsBeforeTheFirstSession(t *testing.T) {
	t.Parallel()
	h := newHarness(t, with2FA(true))
	email := uniqueEmail(t)

	// Sign-up gives no session: the user must enrol first.
	rec := h.signUp(email, "password123")
	wantStatus(t, rec, http.StatusOK)
	body := jsonBody(t, rec)
	if body["twoFactorRedirect"] != true || body["twoFactorSetupRequired"] != true || sessionCookie(rec) != nil {
		t.Fatalf("sign-up = %v", body)
	}
	pending := hdr{"Cookie": cookies(rec), "Origin": testBase}
	// Backup codes cannot stand in for enrolment.
	wantCode(t, h.do("POST", BasePath+"/two-factor/verify-totp", map[string]any{"code": "000000"}, pending), http.StatusBadRequest, "TWO_FACTOR_NOT_ENABLED")
	rec = h.do("POST", BasePath+"/two-factor/enable", map[string]any{}, pending)
	wantStatus(t, rec, http.StatusOK)
	secret := jsonBody(t, rec)["secret"].(string)
	rec = h.do("POST", BasePath+"/two-factor/verify-totp", map[string]any{"code": h.code(secret)}, pending)
	wantStatus(t, rec, http.StatusOK)
	if h.getSession(hdr{"Cookie": cookieHeader(t, rec)}) == nil {
		t.Fatal("enrolment did not sign in")
	}
	// From now on sign-in asks for the code.
	h.tick()
	h.challenge(email, "password123")
}
