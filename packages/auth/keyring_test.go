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
func rotationDB(t *testing.T) *pgxpool.Pool {
	t.Helper()
	return newDatabase(t)
}

// instance is a 2FA-enabled Auth on pool with the given secrets, sharing clock.
func instance(t *testing.T, pool *pgxpool.Pool, clock *testClock, secret string, previous ...string) *harness {
	t.Helper()
	return instanceWith(t, pool, clock, Secrets{Current: secret, Previous: previous})
}

func instanceWith(t *testing.T, pool *pgxpool.Pool, clock *testClock, s Secrets) *harness {
	t.Helper()
	logBuf := &bytes.Buffer{}
	cfg := Config{
		Secret: s.Current, PreviousSecrets: s.Previous, RevokedSecrets: s.Revoked, SecretGrace: testGrace, BaseURL: testBase, RateLimiter: allowAll{},
		Logger: slog.New(slog.NewTextHandler(&lockedWriter{w: logBuf}, nil)),
	}
	with2FA(false)(&cfg)
	mail := &captureMailer{}
	a, err := New(cfg, pool, mail)
	if err != nil {
		t.Fatal(err)
	}
	a.now = clock.Now
	return &harness{t: t, a: a, h: a.Handler(), mail: mail, clock: clock, logBuf: logBuf}
}

// signedWith reports whether a signed (possibly URL-encoded) value carries
// secret's HMAC.
func signedWith(t *testing.T, signed, secret string) bool {
	t.Helper()
	signed, _ = url.PathUnescape(signed)
	i := strings.LastIndexByte(signed, '.')
	return i > 0 && signed[i+1:] == base64.StdEncoding.EncodeToString(hmacSum([]byte(secret), signed[:i]))
}

func cookieValue(t *testing.T, header string) string {
	t.Helper()
	_, v, ok := strings.Cut(header, "=")
	if !ok {
		t.Fatalf("not a cookie: %q", header)
	}
	return v
}

func TestRotationKeepsSessionsLinksAndTwoFactor(t *testing.T) {
	t.Parallel()
	pool := rotationDB(t)
	clock := &testClock{t: time.Now()}
	h := instance(t, pool, clock, secretA)
	ctx := context.Background()

	// Before the rotation: a session, a 2FA enrolment, a verification link.
	enrolled := uniqueEmail(t)
	setup, _ := h.enrol(enrolled)
	other := uniqueEmail(t)
	oldCookie := cookieHeader(t, h.signUp(other, "password123"))
	// The link gets its own user: verifying an address signs that user out everywhere.
	linkUser := uniqueEmail(t)
	linkCookie := cookieHeader(t, h.signUp(linkUser, "password123"))
	wantStatus(t, h.do("POST", BasePath+"/send-verification-email", map[string]any{"email": linkUser, "callbackURL": "/done"}, hdr{"Cookie": linkCookie, "Origin": testBase}), http.StatusOK)
	link := h.mails()[0].url
	if !signedWith(t, cookieValue(t, oldCookie), secretA) {
		t.Fatal("session cookie not signed with A")
	}

	if err := h.a.SetSecrets(ctx, Secrets{Current: secretB, Previous: []string{secretA}}); err != nil {
		t.Fatal(err)
	}

	// The old cookie still works, and comes back re-signed with B.
	rec := h.do("GET", BasePath+"/get-session", nil, hdr{"Cookie": oldCookie})
	if wantStatus(t, rec, http.StatusOK); strings.TrimSpace(rec.Body.String()) == "null" {
		t.Fatal("A-signed session rejected right after the rotation")
	}
	resigned := cookieHeader(t, rec)
	if !signedWith(t, cookieValue(t, resigned), secretB) {
		t.Fatalf("cookie not re-signed with B: %s", resigned)
	}
	// New sessions are signed with B.
	if c := cookieHeader(t, h.signIn(other, "password123")); !signedWith(t, cookieValue(t, c), secretB) {
		t.Fatal("new session not signed with B")
	}
	// A signed bearer token from before gets a fresh one in Set-Auth-Token.
	oldBearer := cookieValue(t, oldCookie)
	rec = h.do("GET", BasePath+"/get-session", nil, hdr{"Authorization": "Bearer " + oldBearer})
	if got := rec.Header().Get("Set-Auth-Token"); strings.TrimSpace(rec.Body.String()) == "null" || !signedWith(t, got, secretB) {
		t.Fatalf("bearer: body %s, Set-Auth-Token %q", rec.Body, got)
	}

	// 2FA keeps working: the data keys were rewrapped, not lost.
	h.tick()
	pending := hdr{"Cookie": h.challenge(enrolled, "password123"), "Origin": testBase}
	wantStatus(t, h.do("POST", BasePath+"/two-factor/verify-totp", map[string]any{"code": h.code(setup.Secret)}, pending), http.StatusOK)
	pending = hdr{"Cookie": h.challenge(enrolled, "password123"), "Origin": testBase}
	wantStatus(t, h.do("POST", BasePath+"/two-factor/verify-backup-code", map[string]any{"code": setup.BackupCodes[0]}, pending), http.StatusOK)
	var kid string
	if err := pool.QueryRow(ctx, `SELECT "kid" FROM "authKey" WHERE "id" = 'data:better-auth:two-factor-secret'`).Scan(&kid); err != nil || kid != kidOf([]byte(secretB)) {
		t.Fatalf("data key kid = %s (%v), want B's", kid, err)
	}

	// A link mailed before the rotation still verifies.
	if rec := h.do("GET", pathOf(t, link), nil, nil); rec.Code != http.StatusFound || rec.Header().Get("Location") != "/done" {
		t.Fatalf("pre-rotation link: %d %s", rec.Code, rec.Header().Get("Location"))
	}

	// After the grace period, A is rejected; B sessions are unaffected.
	h.clock.Advance(testGrace + time.Minute)
	if h.getSession(hdr{"Cookie": oldCookie}) != nil {
		t.Fatal("A-signed cookie accepted after the grace period")
	}
	if h.getSession(hdr{"Cookie": resigned}) == nil {
		t.Fatal("re-signed cookie rejected")
	}
	// Listing A again does not bring it back.
	if err := h.a.SetSecrets(ctx, Secrets{Current: secretB, Previous: []string{secretA}}); err != nil {
		t.Fatal(err)
	}
	if h.getSession(hdr{"Cookie": oldCookie}) != nil {
		t.Fatal("expired secret resurrected by listing it again")
	}
}

func TestGraceWindowIsSharedAcrossInstances(t *testing.T) {
	t.Parallel()
	pool := rotationDB(t)
	clock := &testClock{t: time.Now()}
	first := instance(t, pool, clock, secretA)
	oldCookie := cookieHeader(t, first.signUp(uniqueEmail(t), "password123"))
	if err := first.a.SetSecrets(context.Background(), Secrets{Current: secretB, Previous: []string{secretA}}); err != nil {
		t.Fatal(err)
	}

	// A replica started late in the window (from the provider's current
	// values) ends the window when the first one does, not grace after its boot.
	clock.Advance(testGrace - time.Hour)
	late := instance(t, pool, clock, secretB, secretA)
	if late.getSession(hdr{"Cookie": oldCookie}) == nil {
		t.Fatal("late replica rejects A inside the window")
	}
	clock.Advance(2 * time.Hour)
	if late.getSession(hdr{"Cookie": oldCookie}) != nil {
		t.Fatal("late replica accepts A after the shared window")
	}
}

func TestRotationWithoutPreviousRevokesAtOnceButKeepsTwoFactor(t *testing.T) {
	t.Parallel()
	pool := rotationDB(t)
	clock := &testClock{t: time.Now()}
	h := instance(t, pool, clock, secretA)
	email := uniqueEmail(t)
	setup, _ := h.enrol(email)
	oldCookie := cookieHeader(t, h.signUp(uniqueEmail(t), "password123"))

	// Emergency revoke: B with nothing previous.
	if err := h.a.SetSecrets(context.Background(), Secrets{Current: secretB}); err != nil {
		t.Fatal(err)
	}
	if h.getSession(hdr{"Cookie": oldCookie}) != nil {
		t.Fatal("unlisted old secret still accepted")
	}
	if !strings.Contains(h.logBuf.String(), "does not list the old value") || strings.Contains(h.logBuf.String(), secretA) {
		t.Fatalf("log: %s", h.logBuf.String())
	}
	// The data keys were rewrapped from memory: 2FA still works, here and in
	// a replica that only knows B.
	h.tick()
	pending := hdr{"Cookie": h.challenge(email, "password123"), "Origin": testBase}
	wantStatus(t, h.do("POST", BasePath+"/two-factor/verify-totp", map[string]any{"code": h.code(setup.Secret)}, pending), http.StatusOK)
	replica := instance(t, pool, clock, secretB)
	replica.tick()
	pending = hdr{"Cookie": replica.challenge(email, "password123"), "Origin": testBase}
	wantStatus(t, replica.do("POST", BasePath+"/two-factor/verify-totp", map[string]any{"code": replica.code(setup.Secret)}, pending), http.StatusOK)
	// Invalid secrets change nothing.
	if err := h.a.SetSecrets(context.Background(), Secrets{Current: "short"}); err == nil {
		t.Fatal("short secret accepted")
	}
	if err := h.a.SetSecrets(context.Background(), Secrets{Current: secretC, Previous: []string{"short"}}); err == nil {
		t.Fatal("short previous secret accepted")
	}
	if h.a.ring.Load().current.kid != kidOf([]byte(secretB)) {
		t.Fatal("a rejected SetSecrets changed the keyring")
	}
}

func TestDefaultSecretGraceCoversEverySignedLifetime(t *testing.T) {
	t.Parallel()
	if g := DefaultSecretGrace(Config{}); g != DefaultSessionTTL {
		t.Fatalf("default = %s, want the session lifetime", g)
	}
	if g := DefaultSecretGrace(Config{SessionTTL: time.Hour}); g != VerificationTokenTTL {
		t.Fatalf("short sessions = %s, want the verification link lifetime", g)
	}
	if g := DefaultSecretGrace(Config{TwoFactor: &TwoFactorConfig{}}); g != DefaultTrustDeviceTTL {
		t.Fatalf("with 2FA = %s, want the trust-device lifetime", g)
	}
}

func TestDataKeysOfExistingTwoFactorRowsAreKept(t *testing.T) {
	t.Parallel()
	pool := rotationDB(t)
	ctx := context.Background()
	// A database from before "authKey": a 2FA row sealed with the key derived
	// from A, and no key rows.
	legacy := unitAuthWith(secretA)
	sealed, err := legacy.sealTOTPKey("u1", []byte("totp-key-bytes-0123"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO "user" ("id", "name", "email") VALUES ('u1', 'U', 'u1@example.com')`); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO "twoFactor" ("id", "secret", "backupCodes", "userId") VALUES ('t1', $1, '[]', 'u1')`, sealed); err != nil {
		t.Fatal(err)
	}

	// First boot of this version already rotating (B current, A previous):
	// the legacy secret is found, and the row still opens.
	clock := &testClock{t: time.Now()}
	h := instance(t, pool, clock, secretB, secretA)
	if key, err := h.a.openTOTPKey("u1", sealed); err != nil || string(key) != "totp-key-bytes-0123" {
		t.Fatalf("legacy row: %q %v", key, err)
	}
	if got := h.a.hashBackupCode("u1", "abcde-12345"); got != legacy.hashBackupCode("u1", "abcde-12345") {
		t.Fatal("backup-code key changed")
	}

	// A secret that never wrapped the data keys can't boot: it would lose 2FA.
	logBuf := &bytes.Buffer{}
	_, err = New(Config{Secret: secretC, BaseURL: testBase, Logger: slog.New(slog.NewTextHandler(logBuf, nil))}, pool, &captureMailer{})
	if err == nil || !strings.Contains(err.Error(), "AUTH_SECRET_PREVIOUS") {
		t.Fatalf("New with an unknown secret: %v", err)
	}
	// A fresh database starts from the key the previous release would derive,
	// so replicas still on it during a rolling deploy seal compatible rows.
	fresh := instance(t, rotationDB(t), clock, secretA)
	if k := fresh.a.dataKey(purposeTOTP); !bytes.Equal(k, legacyDataKey([]byte(secretA), purposeTOTP)) {
		t.Fatal("fresh data key is not the previous release's key")
	}
	if _, err := fresh.a.openTOTPKey("u2", sealed); !errors.Is(err, errBadTOTPKey) {
		t.Fatalf("a row bound to another user opened: %v", err)
	}
}

func TestRevokedSecretNeverVerifiesButUnwraps(t *testing.T) {
	t.Parallel()
	pool := rotationDB(t)
	ctx := context.Background()
	clock := &testClock{t: time.Now()}
	h := instance(t, pool, clock, secretA)
	email := uniqueEmail(t)
	setup, _ := h.enrol(email)
	oldCookie := cookieHeader(t, h.signUp(uniqueEmail(t), "password123"))

	// Emergency rotation through a redeploy (env provider): no running
	// process saw B, so a new one can only unwrap via AUTH_SECRET_REVOKED.
	if _, err := New(Config{Secret: secretB, BaseURL: testBase, Logger: slog.New(slog.DiscardHandler)}, pool, &captureMailer{}); err == nil || !strings.Contains(err.Error(), "AUTH_SECRET_REVOKED") {
		t.Fatalf("boot with B only: %v", err)
	}
	replica := instanceWith(t, pool, clock, Secrets{Current: secretB, Revoked: []string{secretA}})
	if replica.getSession(hdr{"Cookie": oldCookie}) != nil {
		t.Fatal("revoked secret verified a cookie")
	}
	replica.tick()
	pending := hdr{"Cookie": replica.challenge(email, "password123"), "Origin": testBase}
	wantStatus(t, replica.do("POST", BasePath+"/two-factor/verify-totp", map[string]any{"code": replica.code(setup.Secret)}, pending), http.StatusOK)

	// Listing it as previous later does not revive it (its window is over).
	if err := replica.a.SetSecrets(ctx, Secrets{Current: secretB, Previous: []string{secretA}}); err != nil {
		t.Fatal(err)
	}
	if replica.getSession(hdr{"Cookie": oldCookie}) != nil {
		t.Fatal("revoked secret revived by listing it as previous")
	}
	// A replica still configured with the leaked secret can't come back.
	if _, err := New(Config{Secret: secretA, BaseURL: testBase, Logger: slog.New(slog.DiscardHandler)}, pool, &captureMailer{}); err == nil {
		t.Fatal("a replica booted with the revoked secret as current")
	}
}

func TestResignFollowsWhereTheTokenCameFrom(t *testing.T) {
	t.Parallel()
	pool := rotationDB(t)
	clock := &testClock{t: time.Now()}
	h := instance(t, pool, clock, secretA)
	oldCookie := cookieHeader(t, h.signUp(uniqueEmail(t), "password123"))
	if err := h.a.SetSecrets(context.Background(), Secrets{Current: secretB, Previous: []string{secretA}}); err != nil {
		t.Fatal(err)
	}
	// A cookie plus a non-bearer Authorization (a basic-auth proxy): the
	// cookie is re-signed as a cookie; its token never goes into a header.
	rec := h.do("GET", BasePath+"/get-session", nil, hdr{"Cookie": oldCookie, "Authorization": "Basic dXNlcjpwYXNz"})
	if strings.TrimSpace(rec.Body.String()) == "null" || rec.Header().Get("Set-Auth-Token") != "" {
		t.Fatalf("body %s, Set-Auth-Token %q", rec.Body, rec.Header().Get("Set-Auth-Token"))
	}
	if !signedWith(t, cookieValue(t, cookieHeader(t, rec)), secretB) {
		t.Fatal("cookie not re-signed")
	}
	// A bearer token is re-issued in the header browsers may read cross-origin.
	rec = h.do("GET", BasePath+"/get-session", nil, hdr{"Authorization": "Bearer " + cookieValue(t, oldCookie)})
	if !strings.Contains(strings.ToLower(rec.Header().Get("Access-Control-Expose-Headers")), "set-auth-token") || sessionCookie(rec) != nil {
		t.Fatalf("bearer re-sign headers: %v", rec.Header())
	}
}

func TestFailedKeyStorageStillAppliesTheKeyring(t *testing.T) {
	t.Parallel()
	pool := rotationDB(t)
	clock := &testClock{t: time.Now()}
	h := instance(t, pool, clock, secretA)
	email := uniqueEmail(t)
	oldCookie := cookieHeader(t, h.signUp(email, "password123"))
	down, cancel := context.WithCancel(context.Background())
	cancel() // the database is unreachable for this rotation

	if err := h.a.SetSecrets(down, Secrets{Current: secretB, Previous: []string{secretA}}); err == nil {
		t.Fatal("SetSecrets succeeded without a database")
	}
	if c := cookieHeader(t, h.signIn(email, "password123")); !signedWith(t, cookieValue(t, c), secretB) {
		t.Fatal("new keyring not applied in memory")
	}
	if h.getSession(hdr{"Cookie": oldCookie}) == nil {
		t.Fatal("previous secret not accepted in memory")
	}
	var retired *time.Time // nothing half-written: A is not retired in the database
	if err := pool.QueryRow(context.Background(), `SELECT "retiredAt" FROM "authKey" WHERE "id" = $1`, "secret:"+kidOf([]byte(secretA))).Scan(&retired); err != nil || retired != nil {
		t.Fatalf("A retiredAt = %v (%v), want NULL", retired, err)
	}
	// An emergency revoke takes effect even while the database is down.
	if err := h.a.SetSecrets(down, Secrets{Current: secretC}); err == nil {
		t.Fatal("SetSecrets succeeded without a database")
	}
	if h.getSession(hdr{"Cookie": oldCookie}) != nil {
		t.Fatal("revoked keyring not applied while the database is down")
	}
	// The retry with the database back stores everything.
	if err := h.a.SetSecrets(context.Background(), Secrets{Current: secretC, Previous: []string{secretB}}); err != nil {
		t.Fatal(err)
	}
}
