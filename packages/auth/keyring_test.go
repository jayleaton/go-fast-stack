package auth

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// testGrace is short so the clock can pass it without expiring sessions.
const testGrace = 2 * time.Hour

var (
	secretA = strings.Repeat("a", 40)
	secretB = strings.Repeat("b", 40)
	secretC = strings.Repeat("c", 40)
)

// rotationDB is a migrated database of its own: rotating keys on the shared
// test database would rewrap the data keys under other tests.
func rotationDB(test *testing.T) *pgxpool.Pool {
	test.Helper()
	return newDatabase(test)
}

// instance is a 2FA-enabled Auth on pool with the given secrets, sharing clock.
func instance(test *testing.T, pool *pgxpool.Pool, clock *testClock, secret string, previous ...string) *harness {
	test.Helper()
	return instanceWith(test, pool, clock, Secrets{Current: secret, Previous: previous})
}

func instanceWith(test *testing.T, pool *pgxpool.Pool, clock *testClock, secrets Secrets) *harness {
	test.Helper()
	logBuf := &bytes.Buffer{}
	cfg := Config{
		Secret: secrets.Current, PreviousSecrets: secrets.Previous, RevokedSecrets: secrets.Revoked, SecretGrace: testGrace, BaseURL: testBase, RateLimiter: allowAll{},
		Logger: slog.New(slog.NewTextHandler(&lockedWriter{writer: logBuf}, nil)),
	}
	with2FA(false)(&cfg)
	mail := &captureMailer{}
	auth, err := New(cfg, pool, mail)
	if err != nil {
		test.Fatal(err)
	}
	auth.now = clock.Now
	return &harness{test: test, auth: auth, handler: auth.Handler(), mail: mail, clock: clock, logBuf: logBuf}
}

// signedWith reports whether a signed (possibly URL-encoded) value carries
// secret's HMAC.
func signedWith(test *testing.T, signed, secret string) bool {
	test.Helper()
	signed, _ = url.PathUnescape(signed)
	index := strings.LastIndexByte(signed, '.')
	return index > 0 && signed[index+1:] == base64.StdEncoding.EncodeToString(hmacSum([]byte(secret), signed[:index]))
}

func cookieValue(test *testing.T, header string) string {
	test.Helper()
	_, value, ok := strings.Cut(header, "=")
	if !ok {
		test.Fatalf("not a cookie: %q", header)
	}
	return value
}

func TestRotationKeepsSessionsLinksAndTwoFactor(test *testing.T) {
	test.Parallel()
	pool := rotationDB(test)
	clock := &testClock{moment: time.Now()}
	harness := instance(test, pool, clock, secretA)
	ctx := context.Background()

	// Before the rotation: a session, a 2FA enrolment, a verification link.
	enrolled := uniqueEmail(test)
	setup, _ := harness.enrol(enrolled)
	other := uniqueEmail(test)
	oldCookie := cookieHeader(test, harness.signUp(other, "password123"))
	// The link gets its own user: verifying an address signs that user out everywhere.
	linkUser := uniqueEmail(test)
	linkCookie := cookieHeader(test, harness.signUp(linkUser, "password123"))
	wantStatus(test, harness.do("POST", BasePath+"/send-verification-email", map[string]any{"email": linkUser, "callbackURL": "/done"}, hdr{"Cookie": linkCookie, "Origin": testBase}), http.StatusOK)
	link := harness.mails()[0].url
	if !signedWith(test, cookieValue(test, oldCookie), secretA) {
		test.Fatal("session cookie not signed with A")
	}

	if err := harness.auth.SetSecrets(ctx, Secrets{Current: secretB, Previous: []string{secretA}}); err != nil {
		test.Fatal(err)
	}

	// The old cookie still works, and comes back re-signed with B.
	rec := harness.do("GET", BasePath+"/get-session", nil, hdr{"Cookie": oldCookie})
	if wantStatus(test, rec, http.StatusOK); strings.TrimSpace(rec.Body.String()) == "null" {
		test.Fatal("A-signed session rejected right after the rotation")
	}
	resigned := cookieHeader(test, rec)
	if !signedWith(test, cookieValue(test, resigned), secretB) {
		test.Fatalf("cookie not re-signed with B: %s", resigned)
	}
	// New sessions are signed with B.
	if header := cookieHeader(test, harness.signIn(other, "password123")); !signedWith(test, cookieValue(test, header), secretB) {
		test.Fatal("new session not signed with B")
	}
	// A signed bearer token from before gets a fresh one in Set-Auth-Token.
	oldBearer := cookieValue(test, oldCookie)
	rec = harness.do("GET", BasePath+"/get-session", nil, hdr{"Authorization": "Bearer " + oldBearer})
	if got := rec.Header().Get("Set-Auth-Token"); strings.TrimSpace(rec.Body.String()) == "null" || !signedWith(test, got, secretB) {
		test.Fatalf("bearer: body %s, Set-Auth-Token %q", rec.Body, got)
	}

	// 2FA keeps working: the data keys were rewrapped, not lost.
	harness.tick()
	pending := hdr{"Cookie": harness.challenge(enrolled, "password123"), "Origin": testBase}
	wantStatus(test, harness.do("POST", BasePath+"/two-factor/verify-totp", map[string]any{"code": harness.code(setup.Secret)}, pending), http.StatusOK)
	pending = hdr{"Cookie": harness.challenge(enrolled, "password123"), "Origin": testBase}
	wantStatus(test, harness.do("POST", BasePath+"/two-factor/verify-backup-code", map[string]any{"code": setup.BackupCodes[0]}, pending), http.StatusOK)
	var kid string
	if err := pool.QueryRow(ctx, `SELECT "kid" FROM "authKey" WHERE "id" = 'data:better-auth:two-factor-secret'`).Scan(&kid); err != nil || kid != kidOf([]byte(secretB)) {
		test.Fatalf("data key kid = %s (%v), want B's", kid, err)
	}

	// A link mailed before the rotation still verifies.
	if rec := harness.do("GET", pathOf(test, link), nil, nil); rec.Code != http.StatusFound || rec.Header().Get("Location") != "/done" {
		test.Fatalf("pre-rotation link: %d %s", rec.Code, rec.Header().Get("Location"))
	}

	// After the grace period, A is rejected; B sessions are unaffected.
	harness.clock.Advance(testGrace + time.Minute)
	if harness.getSession(hdr{"Cookie": oldCookie}) != nil {
		test.Fatal("A-signed cookie accepted after the grace period")
	}
	if harness.getSession(hdr{"Cookie": resigned}) == nil {
		test.Fatal("re-signed cookie rejected")
	}
	// Listing A again does not bring it back.
	if err := harness.auth.SetSecrets(ctx, Secrets{Current: secretB, Previous: []string{secretA}}); err != nil {
		test.Fatal(err)
	}
	if harness.getSession(hdr{"Cookie": oldCookie}) != nil {
		test.Fatal("expired secret resurrected by listing it again")
	}
}

func TestGraceWindowIsSharedAcrossInstances(test *testing.T) {
	test.Parallel()
	pool := rotationDB(test)
	clock := &testClock{moment: time.Now()}
	first := instance(test, pool, clock, secretA)
	oldCookie := cookieHeader(test, first.signUp(uniqueEmail(test), "password123"))
	if err := first.auth.SetSecrets(context.Background(), Secrets{Current: secretB, Previous: []string{secretA}}); err != nil {
		test.Fatal(err)
	}

	// A replica started late in the window (from the provider's current
	// values) ends the window when the first one does, not grace after its boot.
	clock.Advance(testGrace - time.Hour)
	late := instance(test, pool, clock, secretB, secretA)
	if late.getSession(hdr{"Cookie": oldCookie}) == nil {
		test.Fatal("late replica rejects A inside the window")
	}
	clock.Advance(2 * time.Hour)
	if late.getSession(hdr{"Cookie": oldCookie}) != nil {
		test.Fatal("late replica accepts A after the shared window")
	}
}

func TestRotationWithoutPreviousRevokesAtOnceButKeepsTwoFactor(test *testing.T) {
	test.Parallel()
	pool := rotationDB(test)
	clock := &testClock{moment: time.Now()}
	harness := instance(test, pool, clock, secretA)
	email := uniqueEmail(test)
	setup, _ := harness.enrol(email)
	oldCookie := cookieHeader(test, harness.signUp(uniqueEmail(test), "password123"))

	// Emergency revoke: B with nothing previous.
	if err := harness.auth.SetSecrets(context.Background(), Secrets{Current: secretB}); err != nil {
		test.Fatal(err)
	}
	if harness.getSession(hdr{"Cookie": oldCookie}) != nil {
		test.Fatal("unlisted old secret still accepted")
	}
	if !strings.Contains(harness.logBuf.String(), "does not list the old value") || strings.Contains(harness.logBuf.String(), secretA) {
		test.Fatalf("log: %s", harness.logBuf.String())
	}
	// The data keys were rewrapped from memory: 2FA still works, here and in
	// a replica that only knows B.
	harness.tick()
	pending := hdr{"Cookie": harness.challenge(email, "password123"), "Origin": testBase}
	wantStatus(test, harness.do("POST", BasePath+"/two-factor/verify-totp", map[string]any{"code": harness.code(setup.Secret)}, pending), http.StatusOK)
	replica := instance(test, pool, clock, secretB)
	replica.tick()
	pending = hdr{"Cookie": replica.challenge(email, "password123"), "Origin": testBase}
	wantStatus(test, replica.do("POST", BasePath+"/two-factor/verify-totp", map[string]any{"code": replica.code(setup.Secret)}, pending), http.StatusOK)
	// Invalid secrets change nothing.
	if err := harness.auth.SetSecrets(context.Background(), Secrets{Current: "short"}); err == nil {
		test.Fatal("short secret accepted")
	}
	if err := harness.auth.SetSecrets(context.Background(), Secrets{Current: secretC, Previous: []string{"short"}}); err == nil {
		test.Fatal("short previous secret accepted")
	}
	if harness.auth.ring.Load().current.kid != kidOf([]byte(secretB)) {
		test.Fatal("a rejected SetSecrets changed the keyring")
	}
}

func TestDefaultSecretGraceCoversEverySignedLifetime(test *testing.T) {
	test.Parallel()
	if duration := DefaultSecretGrace(Config{}); duration != DefaultSessionTTL {
		test.Fatalf("default = %s, want the session lifetime", duration)
	}
	if duration := DefaultSecretGrace(Config{SessionTTL: time.Hour}); duration != VerificationTokenTTL {
		test.Fatalf("short sessions = %s, want the verification link lifetime", duration)
	}
	if duration := DefaultSecretGrace(Config{TwoFactor: &TwoFactorConfig{}}); duration != DefaultTrustDeviceTTL {
		test.Fatalf("with 2FA = %s, want the trust-device lifetime", duration)
	}
}

func TestDataKeysOfExistingTwoFactorRowsAreKept(test *testing.T) {
	test.Parallel()
	pool := rotationDB(test)
	ctx := context.Background()
	// A database from before "authKey": a 2FA row sealed with the key derived
	// from A, and no key rows.
	legacy := unitAuthWith(secretA)
	sealed, err := legacy.sealTOTPKey("u1", []byte("totp-key-bytes-0123"))
	if err != nil {
		test.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO "user" ("id", "name", "email") VALUES ('u1', 'U', 'u1@example.com')`); err != nil {
		test.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO "twoFactor" ("id", "secret", "backupCodes", "userId") VALUES ('t1', $1, '[]', 'u1')`, sealed); err != nil {
		test.Fatal(err)
	}

	// First boot of this version already rotating (B current, A previous):
	// the legacy secret is found, and the row still opens.
	clock := &testClock{moment: time.Now()}
	harness := instance(test, pool, clock, secretB, secretA)
	if key, err := harness.auth.openTOTPKey("u1", sealed); err != nil || string(key) != "totp-key-bytes-0123" {
		test.Fatalf("legacy row: %q %v", key, err)
	}
	if got := harness.auth.hashBackupCode("u1", "abcde-12345"); got != legacy.hashBackupCode("u1", "abcde-12345") {
		test.Fatal("backup-code key changed")
	}

	// A secret that never wrapped the data keys can't boot: it would lose 2FA.
	logBuf := &bytes.Buffer{}
	_, err = New(Config{Secret: secretC, BaseURL: testBase, Logger: slog.New(slog.NewTextHandler(logBuf, nil))}, pool, &captureMailer{})
	if err == nil || !strings.Contains(err.Error(), "AUTH_SECRET_PREVIOUS") {
		test.Fatalf("New with an unknown secret: %v", err)
	}
	// A fresh database starts from the key the previous release would derive,
	// so replicas still on it during a rolling deploy seal compatible rows.
	fresh := instance(test, rotationDB(test), clock, secretA)
	if key := fresh.auth.dataKey(purposeTOTP); !bytes.Equal(key, legacyDataKey([]byte(secretA), purposeTOTP)) {
		test.Fatal("fresh data key is not the previous release's key")
	}
	if _, err := fresh.auth.openTOTPKey("u2", sealed); !errors.Is(err, errBadTOTPKey) {
		test.Fatalf("a row bound to another user opened: %v", err)
	}
}

func TestRevokedSecretNeverVerifiesButUnwraps(test *testing.T) {
	test.Parallel()
	pool := rotationDB(test)
	ctx := context.Background()
	clock := &testClock{moment: time.Now()}
	harness := instance(test, pool, clock, secretA)
	email := uniqueEmail(test)
	setup, _ := harness.enrol(email)
	oldCookie := cookieHeader(test, harness.signUp(uniqueEmail(test), "password123"))

	// Emergency rotation through a redeploy (env provider): no running
	// process saw B, so a new one can only unwrap via AUTH_SECRET_REVOKED.
	if _, err := New(Config{Secret: secretB, BaseURL: testBase, Logger: slog.New(slog.DiscardHandler)}, pool, &captureMailer{}); err == nil || !strings.Contains(err.Error(), "AUTH_SECRET_REVOKED") {
		test.Fatalf("boot with B only: %v", err)
	}
	replica := instanceWith(test, pool, clock, Secrets{Current: secretB, Revoked: []string{secretA}})
	if replica.getSession(hdr{"Cookie": oldCookie}) != nil {
		test.Fatal("revoked secret verified a cookie")
	}
	replica.tick()
	pending := hdr{"Cookie": replica.challenge(email, "password123"), "Origin": testBase}
	wantStatus(test, replica.do("POST", BasePath+"/two-factor/verify-totp", map[string]any{"code": replica.code(setup.Secret)}, pending), http.StatusOK)

	// Listing it as previous later does not revive it (its window is over).
	if err := replica.auth.SetSecrets(ctx, Secrets{Current: secretB, Previous: []string{secretA}}); err != nil {
		test.Fatal(err)
	}
	if replica.getSession(hdr{"Cookie": oldCookie}) != nil {
		test.Fatal("revoked secret revived by listing it as previous")
	}
	// A replica still configured with the leaked secret can't come back.
	if _, err := New(Config{Secret: secretA, BaseURL: testBase, Logger: slog.New(slog.DiscardHandler)}, pool, &captureMailer{}); err == nil {
		test.Fatal("a replica booted with the revoked secret as current")
	}
}

func TestResignFollowsWhereTheTokenCameFrom(test *testing.T) {
	test.Parallel()
	pool := rotationDB(test)
	clock := &testClock{moment: time.Now()}
	harness := instance(test, pool, clock, secretA)
	oldCookie := cookieHeader(test, harness.signUp(uniqueEmail(test), "password123"))
	if err := harness.auth.SetSecrets(context.Background(), Secrets{Current: secretB, Previous: []string{secretA}}); err != nil {
		test.Fatal(err)
	}
	// A cookie plus a non-bearer Authorization (a basic-auth proxy): the
	// cookie is re-signed as a cookie; its token never goes into a header.
	rec := harness.do("GET", BasePath+"/get-session", nil, hdr{"Cookie": oldCookie, "Authorization": "Basic dXNlcjpwYXNz"})
	if strings.TrimSpace(rec.Body.String()) == "null" || rec.Header().Get("Set-Auth-Token") != "" {
		test.Fatalf("body %s, Set-Auth-Token %q", rec.Body, rec.Header().Get("Set-Auth-Token"))
	}
	if !signedWith(test, cookieValue(test, cookieHeader(test, rec)), secretB) {
		test.Fatal("cookie not re-signed")
	}
	// A bearer token is re-issued in the header browsers may read cross-origin.
	rec = harness.do("GET", BasePath+"/get-session", nil, hdr{"Authorization": "Bearer " + cookieValue(test, oldCookie)})
	if !strings.Contains(strings.ToLower(rec.Header().Get("Access-Control-Expose-Headers")), "set-auth-token") || sessionCookie(rec) != nil {
		test.Fatalf("bearer re-sign headers: %v", rec.Header())
	}
}

func TestFailedKeyStorageStillAppliesTheKeyring(test *testing.T) {
	test.Parallel()
	pool := rotationDB(test)
	clock := &testClock{moment: time.Now()}
	harness := instance(test, pool, clock, secretA)
	email := uniqueEmail(test)
	oldCookie := cookieHeader(test, harness.signUp(email, "password123"))
	down, cancel := context.WithCancel(context.Background())
	cancel() // the database is unreachable for this rotation

	if err := harness.auth.SetSecrets(down, Secrets{Current: secretB, Previous: []string{secretA}}); err == nil {
		test.Fatal("SetSecrets succeeded without a database")
	}
	if header := cookieHeader(test, harness.signIn(email, "password123")); !signedWith(test, cookieValue(test, header), secretB) {
		test.Fatal("new keyring not applied in memory")
	}
	if harness.getSession(hdr{"Cookie": oldCookie}) == nil {
		test.Fatal("previous secret not accepted in memory")
	}
	var retired *time.Time // nothing half-written: A is not retired in the database
	if err := pool.QueryRow(context.Background(), `SELECT "retiredAt" FROM "authKey" WHERE "id" = $1`, "secret:"+kidOf([]byte(secretA))).Scan(&retired); err != nil || retired != nil {
		test.Fatalf("A retiredAt = %v (%v), want NULL", retired, err)
	}
	// An emergency revoke takes effect even while the database is down.
	if err := harness.auth.SetSecrets(down, Secrets{Current: secretC}); err == nil {
		test.Fatal("SetSecrets succeeded without a database")
	}
	if harness.getSession(hdr{"Cookie": oldCookie}) != nil {
		test.Fatal("revoked keyring not applied while the database is down")
	}
	// The retry with the database back stores everything.
	if err := harness.auth.SetSecrets(context.Background(), Secrets{Current: secretC, Previous: []string{secretB}}); err != nil {
		test.Fatal(err)
	}
}
