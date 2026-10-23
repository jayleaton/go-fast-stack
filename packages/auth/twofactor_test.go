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
	return func(config *Config) { config.TwoFactor = &TwoFactorConfig{Required: required}; config.AppName = "Acme" }
}

// code returns the current TOTP code for a base32 secret at the harness clock.
func (harness *harness) code(secret string) string {
	harness.test.Helper()
	key, err := b32.DecodeString(secret)
	if err != nil {
		harness.test.Fatal(err)
	}
	return totpCode(key, totpStep(harness.clock.Now()))
}

// wrongCode returns a 6-digit code that is guaranteed invalid for secret at
// the harness clock: a fixed "000000" is valid in 3 of every million windows,
// which makes a test fail at random once in a while.
func (harness *harness) wrongCode(secret string) string {
	harness.test.Helper()
	key, err := b32.DecodeString(secret)
	if err != nil {
		harness.test.Fatal(err)
	}
	step := totpStep(harness.clock.Now())
	valid := map[string]bool{}
	for digit := int64(-totpWindow - 1); digit <= totpWindow+1; digit++ {
		valid[totpCode(key, step+digit)] = true
	}
	for index := 0; ; index++ {
		if code := fmt.Sprintf("%06d", index); !valid[code] {
			return code
		}
	}
}

// tick moves the clock to the next TOTP step so a fresh code is accepted.
func (harness *harness) tick() { harness.clock.Advance(totpPeriod) }

// cookies joins the Set-Cookie values of rec (minus expired ones) with base.
func cookies(rec *httptest.ResponseRecorder, base ...string) string {
	parts := append([]string(nil), base...)
	for _, cookie := range rec.Result().Cookies() {
		if cookie.MaxAge >= 0 && cookie.Value != "" {
			parts = append(parts, cookie.Name+"="+cookie.Value)
		}
	}
	return strings.Join(parts, "; ")
}

// enrol signs email up, enables and confirms TOTP over the JSON API, and
// returns the setup and the signed-in headers.
func (harness *harness) enrol(email string) (TOTPSetup, hdr) {
	harness.test.Helper()
	signedIn := hdr{"Cookie": cookieHeader(harness.test, harness.signUp(email, "password123")), "Origin": testBase}
	rec := harness.do("POST", BasePath+"/two-factor/enable", map[string]any{"password": "password123"}, signedIn)
	wantStatus(harness.test, rec, http.StatusOK)
	body := jsonBody(harness.test, rec)
	setup := TOTPSetup{Secret: body["secret"].(string), URI: body["totpURI"].(string)}
	for _, value := range body["backupCodes"].([]any) {
		setup.BackupCodes = append(setup.BackupCodes, value.(string))
	}
	wantStatus(harness.test, harness.do("POST", BasePath+"/two-factor/verify-totp", map[string]any{"code": harness.code(setup.Secret)}, signedIn), http.StatusOK)
	harness.tick()
	return setup, signedIn
}

// challenge signs in and expects a 2FA challenge; it returns the cookie header.
func (harness *harness) challenge(email, password string, extra ...string) string {
	harness.test.Helper()
	headers := hdr{}
	if len(extra) > 0 {
		headers["Cookie"], headers["Origin"] = strings.Join(extra, "; "), testBase
	}
	rec := harness.do("POST", BasePath+"/sign-in/email", map[string]any{"email": email, "password": password}, headers)
	wantStatus(harness.test, rec, http.StatusOK)
	body := jsonBody(harness.test, rec)
	if body["twoFactorRedirect"] != true || sessionCookie(rec) != nil {
		harness.test.Fatalf("sign-in did not ask for 2FA: %v", body)
	}
	return cookies(rec, extra...)
}

func TestTwoFactorEnrolmentAndSignIn(test *testing.T) {
	test.Parallel()
	harness := newHarness(test, with2FA(false))
	email := uniqueEmail(test)
	signedIn := hdr{"Cookie": cookieHeader(test, harness.signUp(email, "password123")), "Origin": testBase}

	wantCode(test, harness.do("POST", BasePath+"/two-factor/enable", map[string]any{"password": "wrong-password"}, signedIn), http.StatusBadRequest, "INVALID_PASSWORD")
	wantCode(test, harness.do("POST", BasePath+"/two-factor/enable", map[string]any{"password": "password123"}, nil), http.StatusUnauthorized, "UNAUTHORIZED")
	rec := harness.do("POST", BasePath+"/two-factor/enable", map[string]any{"password": "password123"}, signedIn)
	wantStatus(test, rec, http.StatusOK)
	body := jsonBody(test, rec)
	secret, uri := body["secret"].(string), body["totpURI"].(string)
	if len(body["backupCodes"].([]any)) != backupCodeCount || !strings.HasPrefix(uri, "otpauth://totp/Acme:") || !strings.Contains(uri, "secret="+secret) {
		test.Fatalf("enable = %v", body)
	}
	link, _ := url.Parse(uri)
	if link.Query().Get("issuer") != "Acme" || link.Query().Get("digits") != "6" {
		test.Fatalf("uri = %s", uri)
	}

	// Not active until confirmed: sign-in still gives a session.
	wantStatus(test, harness.signIn(email, "password123"), http.StatusOK)
	// Someone else's pending two_factor cookie does not hijack the confirm step.
	stranger := uniqueEmail(test)
	harness.enrol(stranger)
	foreign := strings.Split(harness.challenge(stranger, "password123"), "; ")
	signedIn["Cookie"] += "; " + foreign[len(foreign)-1]
	wantCode(test, harness.do("POST", BasePath+"/two-factor/verify-totp", map[string]any{"code": harness.wrongCode(secret)}, signedIn), http.StatusUnauthorized, "INVALID_CODE")
	confirm := harness.code(secret)
	wantStatus(test, harness.do("POST", BasePath+"/two-factor/verify-totp", map[string]any{"code": confirm}, signedIn), http.StatusOK)
	if on, _ := harness.auth.TwoFactorEnabled(context.Background(), harness.userID(signedIn)); !on {
		test.Fatal("2FA not enabled after confirmation")
	}
	wantCode(test, harness.do("POST", BasePath+"/two-factor/enable", map[string]any{"password": "password123"}, signedIn), http.StatusBadRequest, "TOTP_ALREADY_ENABLED")
	rec = harness.do("POST", BasePath+"/two-factor/get-totp-uri", map[string]any{"password": "password123"}, signedIn)
	if wantStatus(test, rec, http.StatusOK); jsonBody(test, rec)["totpURI"] != uri {
		test.Fatalf("get-totp-uri = %s", rec.Body)
	}
	wantCode(test, harness.do("POST", BasePath+"/two-factor/get-totp-uri", map[string]any{"password": "wrong-password"}, signedIn), http.StatusBadRequest, "INVALID_PASSWORD")
	wantCode(test, harness.do("POST", BasePath+"/two-factor/get-totp-uri", map[string]any{"password": "password123"}, nil), http.StatusUnauthorized, "UNAUTHORIZED")

	// Sign-in now stops at the second factor.
	rec = harness.do("POST", BasePath+"/sign-in/email", map[string]any{"email": email, "password": "password123"}, nil)
	wantStatus(test, rec, http.StatusOK)
	body = jsonBody(test, rec)
	if body["twoFactorRedirect"] != true || len(body["twoFactorMethods"].([]any)) != 2 || body["twoFactorSetupRequired"] != false || sessionCookie(rec) != nil || body["token"] != nil {
		test.Fatalf("sign-in = %v", body)
	}
	pending := hdr{"Cookie": cookies(rec), "Origin": testBase}
	if !strings.Contains(pending["Cookie"], "better-auth.two_factor=") {
		test.Fatalf("no two_factor cookie: %v", pending)
	}

	// The code used to confirm cannot be replayed in its window.
	wantCode(test, harness.do("POST", BasePath+"/two-factor/verify-totp", map[string]any{"code": confirm}, pending), http.StatusUnauthorized, "INVALID_CODE")
	wantCode(test, harness.do("POST", BasePath+"/two-factor/verify-totp", map[string]any{"code": harness.wrongCode(secret)}, pending), http.StatusUnauthorized, "INVALID_CODE")
	harness.tick()
	code := harness.code(secret)
	rec = harness.do("POST", BasePath+"/two-factor/verify-totp", map[string]any{"code": code}, pending)
	wantStatus(test, rec, http.StatusOK)
	if jsonBody(test, rec)["token"] == "" || harness.getSession(hdr{"Cookie": cookieHeader(test, rec)}) == nil {
		test.Fatal("verify-totp did not sign in")
	}
	// The challenge is spent, and the same code cannot open another one.
	wantCode(test, harness.do("POST", BasePath+"/two-factor/verify-totp", map[string]any{"code": code}, pending), http.StatusUnauthorized, "INVALID_TWO_FACTOR_COOKIE")
	again := hdr{"Cookie": harness.challenge(email, "password123"), "Origin": testBase}
	wantCode(test, harness.do("POST", BasePath+"/two-factor/verify-totp", map[string]any{"code": code}, again), http.StatusUnauthorized, "INVALID_CODE")

	// A wrong password never reaches the second factor.
	wantCode(test, harness.signIn(email, "wrong-password"), http.StatusUnauthorized, "INVALID_EMAIL_OR_PASSWORD")
	// Without a challenge cookie or session there is nothing to verify.
	wantCode(test, harness.do("POST", BasePath+"/two-factor/verify-totp", map[string]any{"code": code}, nil), http.StatusUnauthorized, "INVALID_TWO_FACTOR_COOKIE")
}

func TestBackupCodesAreSingleUse(test *testing.T) {
	test.Parallel()
	harness := newHarness(test, with2FA(false))
	email := uniqueEmail(test)
	setup, signedIn := harness.enrol(email)

	pending := hdr{"Cookie": harness.challenge(email, "password123"), "Origin": testBase}
	code := strings.ToUpper(setup.BackupCodes[0]) // case and dash insensitive
	wantStatus(test, harness.do("POST", BasePath+"/two-factor/verify-backup-code", map[string]any{"code": strings.ReplaceAll(code, "-", "")}, pending), http.StatusOK)
	pending = hdr{"Cookie": harness.challenge(email, "password123"), "Origin": testBase}
	wantCode(test, harness.do("POST", BasePath+"/two-factor/verify-backup-code", map[string]any{"code": setup.BackupCodes[0]}, pending), http.StatusUnauthorized, "INVALID_BACKUP_CODE")
	wantStatus(test, harness.do("POST", BasePath+"/two-factor/verify-backup-code", map[string]any{"code": setup.BackupCodes[1]}, pending), http.StatusOK)

	// Regenerating needs the password and voids the old codes.
	wantCode(test, harness.do("POST", BasePath+"/two-factor/generate-backup-codes", map[string]any{"password": "nope-nope"}, signedIn), http.StatusBadRequest, "INVALID_PASSWORD")
	rec := harness.do("POST", BasePath+"/two-factor/generate-backup-codes", map[string]any{"password": "password123"}, signedIn)
	wantStatus(test, rec, http.StatusOK)
	fresh := jsonBody(test, rec)["backupCodes"].([]any)
	pending = hdr{"Cookie": harness.challenge(email, "password123"), "Origin": testBase}
	wantCode(test, harness.do("POST", BasePath+"/two-factor/verify-backup-code", map[string]any{"code": setup.BackupCodes[2]}, pending), http.StatusUnauthorized, "INVALID_BACKUP_CODE")
	wantStatus(test, harness.do("POST", BasePath+"/two-factor/verify-backup-code", map[string]any{"code": fresh[0]}, pending), http.StatusOK)

	// Another user's code is not accepted.
	other := uniqueEmail(test)
	otherSetup, _ := harness.enrol(other)
	pending = hdr{"Cookie": harness.challenge(email, "password123"), "Origin": testBase}
	wantCode(test, harness.do("POST", BasePath+"/two-factor/verify-backup-code", map[string]any{"code": otherSetup.BackupCodes[0]}, pending), http.StatusUnauthorized, "INVALID_BACKUP_CODE")
}

func TestTrustedDeviceAndDisable(test *testing.T) {
	test.Parallel()
	harness := newHarness(test, with2FA(false))
	email := uniqueEmail(test)
	setup, signedIn := harness.enrol(email)

	pending := hdr{"Cookie": harness.challenge(email, "password123"), "Origin": testBase}
	rec := harness.do("POST", BasePath+"/two-factor/verify-totp", map[string]any{"code": harness.code(setup.Secret), "trustDevice": true}, pending)
	wantStatus(test, rec, http.StatusOK)
	var trust string
	for _, cookie := range rec.Result().Cookies() {
		if cookie.Name == "better-auth.trust_device" && cookie.MaxAge > 0 {
			trust = cookie.Name + "=" + cookie.Value
		}
	}
	if trust == "" {
		test.Fatal("no trust_device cookie")
	}
	// The trusted device skips 2FA; another device does not.
	rec = harness.do("POST", BasePath+"/sign-in/email", map[string]any{"email": email, "password": "password123"}, hdr{"Cookie": trust, "Origin": testBase})
	wantStatus(test, rec, http.StatusOK)
	if sessionCookie(rec) == nil {
		test.Fatalf("trusted device still challenged: %s", rec.Body)
	}
	harness.challenge(email, "password123")
	// A trust cookie is bound to its user.
	other := uniqueEmail(test)
	harness.enrol(other)
	harness.challenge(other, "password123", trust)

	// Disabling needs the password, turns 2FA off and forgets trusted devices.
	wantCode(test, harness.do("POST", BasePath+"/two-factor/disable", map[string]any{"password": "wrong-password"}, signedIn), http.StatusBadRequest, "INVALID_PASSWORD")
	wantStatus(test, harness.do("POST", BasePath+"/two-factor/disable", map[string]any{"password": "password123"}, signedIn), http.StatusOK)
	wantCode(test, harness.do("POST", BasePath+"/two-factor/disable", map[string]any{"password": "password123"}, signedIn), http.StatusBadRequest, "TWO_FACTOR_NOT_ENABLED")
	wantStatus(test, harness.signIn(email, "password123"), http.StatusOK)
	if harness.auth.trustedDevice(context.WithValue(context.Background(), ctxKey{}, &requestMeta{trustToken: strings.SplitN(trust, "=", 2)[1]}), harness.userID(signedIn)) {
		test.Fatal("trusted device survived disable")
	}
}

func TestTwoFactorLockoutExpiryAndOff(test *testing.T) {
	test.Parallel()
	harness := newHarness(test, with2FA(false))
	ctx := context.Background()
	email := uniqueEmail(test)
	setup, signedIn := harness.enrol(email)
	id := harness.userID(signedIn)

	// Ten wrong codes lock the second factor for 15 minutes, even for the right code.
	for range twoFactorMaxFailures {
		headers := hdr{"Cookie": harness.challenge(email, "password123"), "Origin": testBase}
		wantCode(test, harness.do("POST", BasePath+"/two-factor/verify-totp", map[string]any{"code": harness.wrongCode(setup.Secret)}, headers), http.StatusUnauthorized, "INVALID_CODE")
	}
	locked := hdr{"Cookie": harness.challenge(email, "password123"), "Origin": testBase}
	wantCode(test, harness.do("POST", BasePath+"/two-factor/verify-totp", map[string]any{"code": harness.code(setup.Secret)}, locked), http.StatusTooManyRequests, "ACCOUNT_TEMPORARILY_LOCKED")
	harness.clock.Advance(twoFactorLockout + time.Minute)
	// After the lock runs out one typo does not lock again: counting restarts.
	typo := hdr{"Cookie": harness.challenge(email, "password123"), "Origin": testBase}
	wantCode(test, harness.do("POST", BasePath+"/two-factor/verify-totp", map[string]any{"code": harness.wrongCode(setup.Secret)}, typo), http.StatusUnauthorized, "INVALID_CODE")
	unlocked := hdr{"Cookie": harness.challenge(email, "password123"), "Origin": testBase}
	wantStatus(test, harness.do("POST", BasePath+"/two-factor/verify-totp", map[string]any{"code": harness.code(setup.Secret)}, unlocked), http.StatusOK)

	// A challenge expires after 10 minutes.
	harness.tick()
	stale := hdr{"Cookie": harness.challenge(email, "password123"), "Origin": testBase}
	harness.clock.Advance(challengeTTL + time.Second)
	wantCode(test, harness.do("POST", BasePath+"/two-factor/verify-totp", map[string]any{"code": harness.code(setup.Secret)}, stale), http.StatusUnauthorized, "INVALID_TWO_FACTOR_COOKIE")

	// Over-long and malformed codes are rejected without a lookup surprise.
	if _, err := harness.auth.VerifyTwoFactor(ctx, VerifyTwoFactorInput{ChallengeToken: "not a token", Code: "1"}); !errors.Is(err, ErrInvalidTwoFactorCookie) {
		test.Fatalf("bad token err = %v", err)
	}
	if err := harness.auth.ConfirmTwoFactor(ctx, id, strings.Repeat("1", 64)); !errors.Is(err, ErrInvalidCode) {
		test.Fatalf("long code err = %v", err)
	}
	if _, err := harness.auth.GenerateBackupCodes(ctx, generateID(32), "password123"); !errors.Is(err, ErrCredentialAccountNotFound) {
		test.Fatalf("unknown user err = %v", err)
	}

	// An instance without TwoFactor serves no 2FA endpoints and ignores enrolments.
	off := newHarness(test, nil)
	wantCode(test, off.do("POST", BasePath+"/two-factor/enable", map[string]any{"password": "password123"}, signedIn), http.StatusNotFound, "NOT_FOUND")
	wantStatus(test, off.signIn(email, "password123"), http.StatusOK)
}

func TestRequiredTwoFactorEnrolsBeforeTheFirstSession(test *testing.T) {
	test.Parallel()
	harness := newHarness(test, with2FA(true))
	email := uniqueEmail(test)

	// Sign-up gives no session: the user must enrol first.
	rec := harness.signUp(email, "password123")
	wantStatus(test, rec, http.StatusOK)
	body := jsonBody(test, rec)
	if body["twoFactorRedirect"] != true || body["twoFactorSetupRequired"] != true || sessionCookie(rec) != nil {
		test.Fatalf("sign-up = %v", body)
	}
	pending := hdr{"Cookie": cookies(rec), "Origin": testBase}
	// Backup codes cannot stand in for enrolment.
	wantCode(test, harness.do("POST", BasePath+"/two-factor/verify-totp", map[string]any{"code": "000000"}, pending), http.StatusBadRequest, "TWO_FACTOR_NOT_ENABLED")
	rec = harness.do("POST", BasePath+"/two-factor/enable", map[string]any{}, pending)
	wantStatus(test, rec, http.StatusOK)
	secret := jsonBody(test, rec)["secret"].(string)
	rec = harness.do("POST", BasePath+"/two-factor/verify-totp", map[string]any{"code": harness.code(secret)}, pending)
	wantStatus(test, rec, http.StatusOK)
	if harness.getSession(hdr{"Cookie": cookieHeader(test, rec)}) == nil {
		test.Fatal("enrolment did not sign in")
	}
	// From now on sign-in asks for the code.
	harness.tick()
	harness.challenge(email, "password123")
}
