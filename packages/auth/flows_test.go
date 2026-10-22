package auth

import (
	"context"
	"errors"
	"io/fs"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestMigrateIsIdempotent(t *testing.T) {
	needDB(t)
	ctx := context.Background()
	if err := Migrate(ctx, testPool); err != nil {
		t.Fatal(err)
	}
	var version int64
	if err := testPool.QueryRow(ctx, `SELECT max(version_id) FROM goose_auth`).Scan(&version); err != nil {
		t.Fatal(err)
	}
	files, _ := fs.Glob(Migrations, "*.sql")
	if version != int64(len(files)) {
		t.Fatalf("goose_auth version = %d, want %d", version, len(files))
	}
	for _, table := range []string{"user", "session", "account", "verification"} {
		var exists bool
		if err := testPool.QueryRow(ctx, `SELECT to_regclass($1) IS NOT NULL`, `"`+table+`"`).Scan(&exists); err != nil || !exists {
			t.Fatalf("table %q missing (err %v)", table, err)
		}
	}
}

func TestNewValidatesConfig(t *testing.T) {
	needDB(t)
	cases := map[string]Config{
		"short secret":          {Secret: "short", BaseURL: testBase},
		"no base url":           {Secret: testSecret},
		"relative base":         {Secret: testSecret, BaseURL: "/app"},
		"ftp base":              {Secret: testSecret, BaseURL: "ftp://app.test"},
		"verify path relative":  {Secret: testSecret, BaseURL: testBase, VerifyEmailPath: "verify"},
		"verify path absolute":  {Secret: testSecret, BaseURL: testBase, VerifyEmailPath: "https://evil.test/verify"},
		"verify path protocol":  {Secret: testSecret, BaseURL: testBase, VerifyEmailPath: "//evil.test/verify"},
		"verify path backslash": {Secret: testSecret, BaseURL: testBase, VerifyEmailPath: "/\\evil.test"},
		"verify path query":     {Secret: testSecret, BaseURL: testBase, VerifyEmailPath: "/verify?x=1"},
	}
	for name, cfg := range cases {
		if _, err := New(cfg, testPool, &captureMailer{}); err == nil {
			t.Errorf("%s: New succeeded, want error", name)
		}
	}
	if _, err := New(Config{Secret: testSecret, BaseURL: testBase}, testPool, nil); err == nil {
		t.Error("nil mailer accepted")
	}
	if _, err := New(Config{Secret: testSecret, BaseURL: testBase}, nil, &captureMailer{}); err == nil {
		t.Error("nil pool accepted")
	}
}

func TestCookieSessionLifecycle(t *testing.T) {
	h := newHarness(t, nil)
	email := uniqueEmail(t)

	rec := h.signUp(email, "correct horse")
	wantStatus(t, rec, http.StatusOK)
	body := jsonBody(t, rec)
	user := body["user"].(map[string]any)
	if body["token"] == nil || user["email"] != email || user["emailVerified"] != false {
		t.Fatalf("sign-up body = %v", body)
	}
	c := sessionCookie(rec)
	if c == nil || c.Name != "better-auth.session_token" || !c.HttpOnly || c.Secure || c.SameSite != http.SameSiteLaxMode || c.Path != "/" {
		t.Fatalf("session cookie = %+v", c)
	}
	if c.MaxAge < int((7*24*time.Hour).Seconds())-5 {
		t.Fatalf("cookie MaxAge = %d, want ~7 days", c.MaxAge)
	}
	if rec.Header().Get("Set-Auth-Token") == "" {
		t.Fatal("set-auth-token header missing")
	}
	cookie := cookieHeader(t, rec)

	got := h.getSession(hdr{"Cookie": cookie})
	if got == nil || got["user"].(map[string]any)["email"] != email {
		t.Fatalf("get-session = %v", got)
	}
	sess := got["session"].(map[string]any)
	if sess["token"] != body["token"] || sess["userId"] != user["id"] || sess["ipAddress"] != "192.0.2.1" {
		t.Fatalf("session = %v", sess)
	}

	// Sign-out with cookies needs a trusted origin.
	rec = h.do("POST", BasePath+"/sign-out", nil, hdr{"Cookie": cookie, "Origin": testBase})
	wantStatus(t, rec, http.StatusOK)
	if jsonBody(t, rec)["success"] != true {
		t.Fatalf("sign-out body = %s", rec.Body)
	}
	if c := sessionCookie(rec); c == nil || c.MaxAge >= 0 {
		t.Fatalf("sign-out did not clear the cookie: %+v", c)
	}
	// The old cookie is dead immediately (no cookie cache).
	if got := h.getSession(hdr{"Cookie": cookie}); got != nil {
		t.Fatalf("session survived sign-out: %v", got)
	}
	var n int
	_ = testPool.QueryRow(context.Background(), `SELECT count(*) FROM "session" WHERE "token" = $1`, body["token"]).Scan(&n)
	if n != 0 {
		t.Fatalf("session row still present")
	}

	// Sign back in with the same password.
	rec = h.signIn(email, "correct horse")
	wantStatus(t, rec, http.StatusOK)
	if b := jsonBody(t, rec); b["redirect"] != false || b["token"] == "" {
		t.Fatalf("sign-in body = %v", b)
	}
}

func TestBearerSession(t *testing.T) {
	h := newHarness(t, nil)
	email := uniqueEmail(t)
	wantStatus(t, h.signUp(email, "password123"), http.StatusOK)

	rec := h.signIn(email, "password123")
	wantStatus(t, rec, http.StatusOK)
	token := jsonBody(t, rec)["token"].(string)
	signed := rec.Header().Get("Set-Auth-Token")
	if !strings.HasPrefix(signed, token+".") {
		t.Fatalf("set-auth-token = %q, want signed %q", signed, token)
	}

	for name, value := range map[string]string{
		"raw token":         "Bearer " + token,
		"signed token":      "Bearer " + signed,
		"url-encoded":       "Bearer " + url.QueryEscape(signed),
		"lowercase scheme":  "bearer " + token,
		"surrounding space": "Bearer   " + token + " ",
	} {
		if got := h.getSession(hdr{"Authorization": value}); got == nil {
			t.Errorf("%s: no session", name)
		}
	}
	for name, value := range map[string]string{
		"tampered signature": "Bearer " + token + ".AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=",
		"unknown token":      "Bearer " + strings.Repeat("x", 32),
		"basic scheme":       "Basic " + token,
		"empty":              "Bearer ",
	} {
		if got := h.getSession(hdr{"Authorization": value}); got != nil {
			t.Errorf("%s: got a session", name)
		}
	}

	// A bearer request carries no cookies, so sign-out needs no origin.
	rec = h.do("POST", BasePath+"/sign-out", nil, hdr{"Authorization": "Bearer " + token})
	wantStatus(t, rec, http.StatusOK)
	if got := h.getSession(hdr{"Authorization": "Bearer " + token}); got != nil {
		t.Fatal("bearer session survived sign-out")
	}
}

func TestSignInRejectsBadCredentials(t *testing.T) {
	h := newHarness(t, nil)
	email := uniqueEmail(t)
	wantStatus(t, h.signUp(email, "password123"), http.StatusOK)

	wantCode(t, h.signIn(email, "wrong-password"), http.StatusUnauthorized, "INVALID_EMAIL_OR_PASSWORD")
	wantCode(t, h.signIn(uniqueEmail(t), "password123"), http.StatusUnauthorized, "INVALID_EMAIL_OR_PASSWORD")
	wantCode(t, h.signIn(strings.ToUpper(email), "wrong-password"), http.StatusUnauthorized, "INVALID_EMAIL_OR_PASSWORD")
	wantStatus(t, h.signIn(strings.ToUpper(email), "password123"), http.StatusOK) // emails are case-insensitive
	wantCode(t, h.signIn("not-an-email", "password123"), http.StatusBadRequest, "INVALID_EMAIL")
	wantCode(t, h.signIn(email, strings.Repeat("a", 129)), http.StatusBadRequest, "PASSWORD_TOO_LONG")
}

func TestSignUpValidation(t *testing.T) {
	h := newHarness(t, nil)
	wantCode(t, h.signUp(uniqueEmail(t), "short"), http.StatusBadRequest, "PASSWORD_TOO_SHORT")
	wantCode(t, h.signUp(uniqueEmail(t), strings.Repeat("é", 129)), http.StatusBadRequest, "PASSWORD_TOO_LONG")
	wantStatus(t, h.signUp(uniqueEmail(t), strings.Repeat("é", 128)), http.StatusOK)
	for _, bad := range []string{"", "plain", "a@b", "Name <a@example.com>", "a@example.com\n"} {
		wantCode(t, h.signUp(bad, "password123"), http.StatusBadRequest, "INVALID_EMAIL")
	}

	rec := h.do("POST", BasePath+"/sign-up/email", nil, hdr{"Content-Type": "text/plain"})
	wantCode(t, rec, http.StatusUnsupportedMediaType, "UNSUPPORTED_MEDIA_TYPE")
	req := h.do("POST", BasePath+"/sign-up/email", "not an object", nil)
	wantCode(t, req, http.StatusBadRequest, "INVALID_REQUEST_BODY")

	email := uniqueEmail(t)
	wantStatus(t, h.signUp(email, "password123"), http.StatusOK)
	wantCode(t, h.signUp(email, "password123"), http.StatusUnprocessableEntity, "USER_ALREADY_EXISTS_USE_ANOTHER_EMAIL")
	wantCode(t, h.signUp(strings.ToUpper(email), "password123"), http.StatusUnprocessableEntity, "USER_ALREADY_EXISTS_USE_ANOTHER_EMAIL")
}

func TestEmailVerificationFlow(t *testing.T) {
	h := newHarness(t, func(c *Config) { c.RequireEmailVerification = true })
	email := uniqueEmail(t)

	rec := h.do("POST", BasePath+"/sign-up/email", map[string]any{
		"name": "V", "email": email, "password": "password123", "callbackURL": "/auth/verify-email/success",
	}, nil)
	wantStatus(t, rec, http.StatusOK)
	if b := jsonBody(t, rec); b["token"] != nil || sessionCookie(rec) != nil {
		t.Fatalf("sign-up signed in before verification: %v", b)
	}
	mails := h.mails()
	if len(mails) != 1 || mails[0].kind != "verification" || mails[0].to != email {
		t.Fatalf("mails = %+v", mails)
	}
	link := mails[0].url
	if !strings.HasPrefix(link, testBase+"/api/auth/verify-email?token=") || !strings.Contains(link, "callbackURL=%2Fauth%2Fverify-email%2Fsuccess") {
		t.Fatalf("verification link = %s", link)
	}

	// Unverified: right password is blocked, wrong password stays generic.
	wantCode(t, h.signIn(email, "password123"), http.StatusForbidden, "EMAIL_NOT_VERIFIED")
	wantCode(t, h.signIn(email, "wrong-password"), http.StatusUnauthorized, "INVALID_EMAIL_OR_PASSWORD")

	// A duplicate sign-up of the unverified account gets the same generic
	// response; it resets the password and sends a new link that replaces
	// the first one.
	rec = h.do("POST", BasePath+"/sign-up/email", map[string]any{
		"name": "V", "email": email, "password": "password123", "callbackURL": "/auth/verify-email/success",
	}, nil)
	wantStatus(t, rec, http.StatusOK)
	if b := jsonBody(t, rec); b["token"] != nil {
		t.Fatalf("duplicate sign-up = %v", b)
	}
	if mails = h.mails(); len(mails) != 2 {
		t.Fatalf("mails = %+v", mails)
	}
	wantCode(t, h.do("GET", BasePath+"/verify-email?token="+mustQuery(t, link, "token"), nil, nil), http.StatusUnauthorized, "INVALID_TOKEN")
	link = mails[1].url

	// Following the link verifies, signs in and redirects to the callback.
	rec = h.do("GET", pathOf(t, link), nil, nil)
	wantStatus(t, rec, http.StatusFound)
	if loc := rec.Header().Get("Location"); loc != "/auth/verify-email/success" {
		t.Fatalf("redirect = %q", loc)
	}
	cookie := cookieHeader(t, rec)
	got := h.getSession(hdr{"Cookie": cookie})
	if got == nil || got["user"].(map[string]any)["emailVerified"] != true {
		t.Fatalf("after verification get-session = %v", got)
	}

	// The used link no longer signs anyone in.
	rec = h.do("GET", pathOf(t, link), nil, nil)
	wantStatus(t, rec, http.StatusFound)
	if sessionCookie(rec) != nil {
		t.Fatal("reused verification link issued a session")
	}
	wantStatus(t, h.signIn(email, "password123"), http.StatusOK)
}

func TestVerifyEmailRejectsBadTokens(t *testing.T) {
	h := newHarness(t, func(c *Config) { c.RequireEmailVerification = true })
	email := uniqueEmail(t)
	wantStatus(t, h.signUp(email, "password123"), http.StatusOK)
	link := h.mails()[0].url
	token := mustQuery(t, link, "token")

	wantCode(t, h.do("GET", BasePath+"/verify-email?token="+token+"x", nil, nil), http.StatusUnauthorized, "INVALID_TOKEN")
	wantCode(t, h.do("GET", BasePath+"/verify-email", nil, nil), http.StatusUnauthorized, "INVALID_TOKEN")
	other := &Auth{cfg: Config{Secret: strings.Repeat("z", 40)}, now: time.Now}
	wantCode(t, h.do("GET", BasePath+"/verify-email?token="+other.signVerificationToken(email, "", time.Hour), nil, nil), http.StatusUnauthorized, "INVALID_TOKEN")
	wantCode(t, h.do("GET", BasePath+"/verify-email?token="+h.a.signVerificationToken(uniqueEmail(t), "", time.Hour), nil, nil), http.StatusUnauthorized, "USER_NOT_FOUND")
	wantCode(t, h.do("GET", BasePath+"/verify-email?token="+token+"&callbackURL=https://evil.test/", nil, nil), http.StatusForbidden, "INVALID_CALLBACK_URL")

	h.clock.Advance(VerificationTokenTTL + time.Minute)
	wantCode(t, h.do("GET", BasePath+"/verify-email?token="+token, nil, nil), http.StatusUnauthorized, "TOKEN_EXPIRED")
	rec := h.do("GET", pathOf(t, link), nil, nil)
	wantStatus(t, rec, http.StatusFound)
	if loc := rec.Header().Get("Location"); loc != "/?error=TOKEN_EXPIRED" {
		t.Fatalf("expired redirect = %q", loc)
	}
	if sessionCookie(rec) != nil {
		t.Fatal("expired token issued a session")
	}
	wantCode(t, h.signIn(email, "password123"), http.StatusForbidden, "EMAIL_NOT_VERIFIED")
}

func TestSendVerificationEmail(t *testing.T) {
	h := newHarness(t, func(c *Config) { c.RequireEmailVerification = true })
	unverified := uniqueEmail(t)
	wantStatus(t, h.signUp(unverified, "password123"), http.StatusOK)
	send := func(email string, headers hdr) int {
		return h.do("POST", BasePath+"/send-verification-email", map[string]any{"email": email, "callbackURL": "/done"}, headers).Code
	}

	if code := send(unverified, nil); code != http.StatusOK {
		t.Fatalf("resend status %d", code)
	}
	if code := send(uniqueEmail(t), nil); code != http.StatusOK {
		t.Fatalf("unknown email status %d", code)
	}
	mails := h.mails()
	// Sends run in the background, so look the resend up rather than rely on order.
	resent := slices.IndexFunc(mails, func(m sentMail) bool { return strings.Contains(m.url, "callbackURL=%2Fdone") })
	if len(mails) != 2 || resent < 0 || mails[resent].to != unverified {
		t.Fatalf("mails = %+v", mails)
	}

	// Verify, then a signed-in resend is refused and a signed-out one is silent.
	rec := h.do("GET", pathOf(t, mails[resent].url), nil, nil)
	cookie := cookieHeader(t, rec)
	wantCode(t, h.do("POST", BasePath+"/send-verification-email", map[string]any{"email": unverified}, hdr{"Cookie": cookie, "Origin": testBase}), http.StatusBadRequest, "EMAIL_ALREADY_VERIFIED")
	wantCode(t, h.do("POST", BasePath+"/send-verification-email", map[string]any{"email": uniqueEmail(t)}, hdr{"Cookie": cookie, "Origin": testBase}), http.StatusBadRequest, "EMAIL_MISMATCH")
	if code := send(unverified, nil); code != http.StatusOK {
		t.Fatalf("verified resend status %d", code)
	}
	if got := len(h.mails()); got != 2 {
		t.Fatalf("verified user got another email (%d total)", got)
	}
}

func TestPasswordResetFlow(t *testing.T) {
	h := newHarness(t, nil)
	email := uniqueEmail(t)
	wantStatus(t, h.signUp(email, "old-password"), http.StatusOK)
	cookieA := cookieHeader(t, h.signIn(email, "old-password"))
	tokenB := jsonBody(t, h.signIn(email, "old-password"))["token"].(string)

	request := func(email string) map[string]any {
		rec := h.do("POST", BasePath+"/request-password-reset", map[string]any{"email": email, "redirectTo": "/auth/reset-password"}, nil)
		wantStatus(t, rec, http.StatusOK)
		return jsonBody(t, rec)
	}
	known, unknown := request(email), request(uniqueEmail(t))
	if known["status"] != true || known["message"] != unknown["message"] || unknown["status"] != true {
		t.Fatalf("responses differ: %v vs %v", known, unknown)
	}
	mails := h.mails()
	if len(mails) != 1 || mails[0].kind != "reset" || mails[0].to != email {
		t.Fatalf("mails = %+v", mails)
	}
	// A relative redirectTo is linked directly, with the token.
	link := mails[0].url
	token := mustQuery(t, link, "token")
	if !strings.HasPrefix(link, testBase+"/auth/reset-password?token=") || token == "" {
		t.Fatalf("reset link = %s", link)
	}
	// The API callback still accepts the token (absolute and deep-link targets use it).
	apiLink := BasePath + "/reset-password/" + token + "?callbackURL=/auth/reset-password"
	if loc := h.do("GET", apiLink, nil, nil).Header().Get("Location"); loc != "/auth/reset-password?token="+token {
		t.Fatalf("callback redirect = %q", loc)
	}

	wantCode(t, h.do("POST", BasePath+"/reset-password", map[string]any{"token": token, "newPassword": "short"}, nil), http.StatusBadRequest, "PASSWORD_TOO_SHORT")
	rec := h.do("POST", BasePath+"/reset-password", map[string]any{"token": token, "newPassword": "new-password"}, nil)
	wantStatus(t, rec, http.StatusOK)

	// Every existing session is revoked, the old password is dead.
	if h.getSession(hdr{"Cookie": cookieA}) != nil || h.getSession(hdr{"Authorization": "Bearer " + tokenB}) != nil {
		t.Fatal("sessions survived the password reset")
	}
	wantCode(t, h.signIn(email, "old-password"), http.StatusUnauthorized, "INVALID_EMAIL_OR_PASSWORD")
	wantStatus(t, h.signIn(email, "new-password"), http.StatusOK)

	// Single use: the token and its link are spent.
	wantCode(t, h.do("POST", BasePath+"/reset-password", map[string]any{"token": token, "newPassword": "third-password"}, nil), http.StatusBadRequest, "INVALID_TOKEN")
	rec = h.do("GET", apiLink, nil, nil)
	if loc := rec.Header().Get("Location"); loc != "/auth/reset-password?error=INVALID_TOKEN" {
		t.Fatalf("spent link redirect = %q", loc)
	}
	wantCode(t, h.do("POST", BasePath+"/reset-password", map[string]any{"newPassword": "third-password"}, nil), http.StatusBadRequest, "INVALID_TOKEN")
	wantCode(t, h.do("POST", BasePath+"/reset-password?token=bad%20token", map[string]any{"newPassword": "third-password"}, nil), http.StatusBadRequest, "INVALID_TOKEN")
}

func TestPasswordResetTokenExpires(t *testing.T) {
	h := newHarness(t, nil)
	email := uniqueEmail(t)
	wantStatus(t, h.signUp(email, "old-password"), http.StatusOK)
	if err := h.a.RequestPasswordReset(context.Background(), email, testBase+"/reset"); err != nil {
		t.Fatal(err)
	}
	link := h.mails()[0].url
	token := strings.TrimPrefix(pathOf(t, link), BasePath+"/reset-password/")
	token = token[:strings.IndexByte(token, '?')]

	h.clock.Advance(ResetPasswordTokenTTL + time.Second)
	rec := h.do("GET", pathOf(t, link), nil, nil)
	if loc := rec.Header().Get("Location"); loc != testBase+"/reset?error=INVALID_TOKEN" {
		t.Fatalf("expired link redirect = %q", loc)
	}
	if err := h.a.ResetPassword(context.Background(), token, "new-password"); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("expired reset err = %v", err)
	}
	wantStatus(t, h.signIn(email, "old-password"), http.StatusOK)
}

func TestPasswordResetTokenIsSingleUseUnderConcurrency(t *testing.T) {
	h := newHarness(t, nil)
	email := uniqueEmail(t)
	wantStatus(t, h.signUp(email, "old-password"), http.StatusOK)
	if err := h.a.RequestPasswordReset(context.Background(), email, "/reset"); err != nil {
		t.Fatal(err)
	}
	token := mustQuery(t, h.mails()[0].url, "token")

	var wg sync.WaitGroup
	results := make([]error, 8)
	for i := range results {
		wg.Go(func() { results[i] = h.a.ResetPassword(context.Background(), token, "new-password") })
	}
	wg.Wait()
	ok := 0
	for _, err := range results {
		if err == nil {
			ok++
		} else if !errors.Is(err, ErrInvalidToken) {
			t.Fatalf("unexpected error: %v", err)
		}
	}
	if ok != 1 {
		t.Fatalf("%d resets succeeded with one token, want 1", ok)
	}
}

func TestRedirectTargetsMustBeTrusted(t *testing.T) {
	h := newHarness(t, nil)
	email := uniqueEmail(t)
	wantStatus(t, h.signUp(email, "password123"), http.StatusOK)

	for _, target := range []string{"https://evil.test/reset", "//evil.test/reset", "/\\evil.test", "javascript:alert(1)"} {
		rec := h.do("POST", BasePath+"/request-password-reset", map[string]any{"email": email, "redirectTo": target}, nil)
		wantCode(t, rec, http.StatusForbidden, "INVALID_REDIRECT_URL")
		rec = h.do("POST", BasePath+"/sign-in/email", map[string]any{"email": email, "password": "password123", "callbackURL": target}, nil)
		wantCode(t, rec, http.StatusForbidden, "INVALID_CALLBACK_URL")
	}
	if got := len(h.mails()); got != 0 {
		t.Fatalf("untrusted redirect sent %d mails", got)
	}
	rec := h.do("GET", BasePath+"/reset-password/abc?callbackURL=https://evil.test", nil, nil)
	wantCode(t, rec, http.StatusForbidden, "INVALID_CALLBACK_URL")
	rec = h.do("GET", BasePath+"/reset-password/abc", nil, nil)
	wantCode(t, rec, http.StatusBadRequest, "INVALID_TOKEN")

	rec = h.do("POST", BasePath+"/sign-in/email", map[string]any{"email": email, "password": "password123", "callbackURL": "betterstack://home"}, nil)
	wantStatus(t, rec, http.StatusOK)
	if b := jsonBody(t, rec); b["redirect"] != true || b["url"] != "betterstack://home" || rec.Header().Get("Location") != "betterstack://home" {
		t.Fatalf("sign-in with callback = %v", b)
	}
}

func TestOriginCheck(t *testing.T) {
	h := newHarness(t, nil)
	email := uniqueEmail(t)
	cookie := cookieHeader(t, h.signUp(email, "password123"))
	signOut := func(headers hdr) int { return h.do("POST", BasePath+"/sign-out", nil, headers).Code }

	// Cookie-carrying requests need a trusted Origin (or Referer / expo-origin).
	cases := []struct {
		name    string
		headers hdr
		want    int
	}{
		{"missing origin", hdr{"Cookie": cookie}, http.StatusForbidden},
		{"null origin", hdr{"Cookie": cookie, "Origin": "null"}, http.StatusForbidden},
		{"evil origin", hdr{"Cookie": cookie, "Origin": "https://evil.test"}, http.StatusForbidden},
		{"lookalike origin", hdr{"Cookie": cookie, "Origin": "http://app.test.evil.test"}, http.StatusForbidden},
		{"evil referer", hdr{"Cookie": cookie, "Referer": "https://evil.test/app.test"}, http.StatusForbidden},
		{"other scheme", hdr{"Cookie": cookie, "Origin": "myapp://"}, http.StatusForbidden},
		{"base origin", hdr{"Cookie": cookie, "Origin": "http://APP.test:80"}, http.StatusOK},
		{"referer", hdr{"Cookie": cookie, "Referer": testBase + "/dashboard"}, http.StatusOK},
		{"mobile scheme", hdr{"Cookie": cookie, "Origin": "betterstack://"}, http.StatusOK},
		{"expo dev", hdr{"Cookie": cookie, "Expo-Origin": "exp://192.168.1.20:8081"}, http.StatusOK},
		{"no cookies", hdr{"Origin": "https://evil.test"}, http.StatusOK},
		{"null same-origin", hdr{"Cookie": cookie, "Origin": "null", "Sec-Fetch-Site": "same-origin"}, http.StatusOK},
		{"null cross-site", hdr{"Cookie": cookie, "Origin": "null", "Sec-Fetch-Site": "cross-site"}, http.StatusForbidden},
	}
	for _, c := range cases {
		if got := signOut(c.headers); got != c.want {
			t.Errorf("%s: status %d, want %d", c.name, got, c.want)
		}
	}

	// exp:// is only trusted outside production.
	prod := newHarness(t, func(c *Config) { c.TrustedOrigins = MobileTrustedOrigins(true) })
	if got := prod.do("POST", BasePath+"/sign-out", nil, hdr{"Cookie": cookie, "Origin": "exp://192.168.1.20:8081"}).Code; got != http.StatusForbidden {
		t.Errorf("production accepted exp:// (status %d)", got)
	}

	// Sign-in/up without cookies: browsers are identified by Fetch Metadata.
	body := map[string]any{"email": email, "password": "password123"}
	signIn := func(headers hdr) *httpResult {
		rec := h.do("POST", BasePath+"/sign-in/email", body, headers)
		return &httpResult{rec.Code, rec.Body.String()}
	}
	if r := signIn(hdr{"Sec-Fetch-Site": "cross-site", "Sec-Fetch-Mode": "navigate", "Origin": testBase}); r.code != http.StatusForbidden || !strings.Contains(r.body, "CROSS_SITE_NAVIGATION_LOGIN_BLOCKED") {
		t.Errorf("cross-site navigation: %+v", r)
	}
	if r := signIn(hdr{"Sec-Fetch-Site": "cross-site", "Sec-Fetch-Mode": "cors", "Origin": "https://evil.test"}); r.code != http.StatusForbidden || !strings.Contains(r.body, "INVALID_ORIGIN") {
		t.Errorf("cross-site fetch: %+v", r)
	}
	if r := signIn(hdr{"Sec-Fetch-Site": "same-origin", "Sec-Fetch-Mode": "cors"}); r.code != http.StatusForbidden || !strings.Contains(r.body, "MISSING_OR_NULL_ORIGIN") {
		t.Errorf("browser without origin: %+v", r)
	}
	if r := signIn(hdr{"Sec-Fetch-Site": "same-origin", "Sec-Fetch-Mode": "cors", "Origin": testBase}); r.code != http.StatusOK {
		t.Errorf("same-origin fetch: %+v", r)
	}
	if r := signIn(hdr{"Cookie": "unrelated=1", "Origin": "https://evil.test"}); r.code != http.StatusForbidden {
		t.Errorf("sign-in with cookies from evil origin: %+v", r)
	}
	if r := signIn(nil); r.code != http.StatusOK {
		t.Errorf("native client: %+v", r)
	}
}

type httpResult struct {
	code int
	body string
}

func TestSessionRefreshAndExpiry(t *testing.T) {
	h := newHarness(t, nil)
	rec := h.signUp(uniqueEmail(t), "password123")
	cookie := cookieHeader(t, rec)
	token := jsonBody(t, rec)["token"].(string)
	expiresAt := func() time.Time {
		var at time.Time
		if err := testPool.QueryRow(context.Background(), `SELECT "expiresAt" FROM "session" WHERE "token" = $1`, token).Scan(&at); err != nil {
			t.Fatal(err)
		}
		return at
	}
	initial := expiresAt()

	// Within the update age nothing changes.
	h.clock.Advance(23 * time.Hour)
	rec = h.do("GET", BasePath+"/get-session", nil, hdr{"Cookie": cookie})
	if sessionCookie(rec) != nil || !expiresAt().Equal(initial) {
		t.Fatal("session refreshed before the update age")
	}

	// After a day of use the session is extended to now + 7 days.
	h.clock.Advance(2 * time.Hour)
	rec = h.do("GET", BasePath+"/get-session", nil, hdr{"Cookie": cookie})
	if sessionCookie(rec) == nil {
		t.Fatal("refreshed session did not reset the cookie")
	}
	if want := h.clock.Now().Add(DefaultSessionTTL); expiresAt().Sub(want).Abs() > time.Millisecond {
		t.Fatalf("expiresAt = %v, want %v", expiresAt(), want)
	}
	// Bearer refreshes too, without setting cookies.
	h.clock.Advance(25 * time.Hour)
	rec = h.do("GET", BasePath+"/get-session", nil, hdr{"Authorization": "Bearer " + token})
	if sessionCookie(rec) != nil {
		t.Fatal("bearer request got a cookie")
	}

	// Past expiry the session is gone and its row deleted.
	h.clock.Advance(DefaultSessionTTL + time.Second)
	rec = h.do("GET", BasePath+"/get-session", nil, hdr{"Cookie": cookie})
	if strings.TrimSpace(rec.Body.String()) != "null" {
		t.Fatalf("expired session returned %s", rec.Body)
	}
	if c := sessionCookie(rec); c == nil || c.MaxAge >= 0 {
		t.Fatal("expired session cookie not cleared")
	}
	var n int
	_ = testPool.QueryRow(context.Background(), `SELECT count(*) FROM "session" WHERE "token" = $1`, token).Scan(&n)
	if n != 0 {
		t.Fatal("expired session row not deleted")
	}
	if rec.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("get-session is cacheable")
	}
}

func TestRequireUserAndUserFrom(t *testing.T) {
	h := newHarness(t, func(c *Config) { c.LoginPath = "/sign-in" })
	email := uniqueEmail(t)
	cookie := cookieHeader(t, h.signUp(email, "password123"))

	protected := h.a.RequireUser(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, sess, ok := UserFrom(r.Context())
		if !ok || user.Email != email || sess.UserID != user.ID {
			t.Errorf("UserFrom = %v %v %v", user, sess, ok)
		}
		w.WriteHeader(http.StatusNoContent)
	}))

	rec := h.serve(protected, "GET", "/api/core/v1/users", nil, nil)
	wantStatus(t, rec, http.StatusUnauthorized)
	if strings.TrimSpace(rec.Body.String()) != `{"data":null,"error":"Unauthorized"}` {
		t.Fatalf("401 body = %s", rec.Body)
	}
	rec = h.serve(protected, "GET", "/dashboard", nil, hdr{"Accept": "text/html,application/xhtml+xml"})
	wantStatus(t, rec, http.StatusSeeOther)
	if rec.Header().Get("Location") != "/sign-in" {
		t.Fatalf("redirect = %q", rec.Header().Get("Location"))
	}
	wantStatus(t, h.serve(protected, "GET", "/dashboard", nil, hdr{"Cookie": cookie}), http.StatusNoContent)
	// Middleware applied twice loads the session once and still works.
	wantStatus(t, h.serve(h.a.Middleware(protected), "GET", "/dashboard", nil, hdr{"Cookie": cookie}), http.StatusNoContent)

	if _, _, ok := UserFrom(context.Background()); ok {
		t.Fatal("UserFrom on an empty context reported a user")
	}
	// A forged cookie is ignored (and not even looked up).
	wantStatus(t, h.serve(protected, "GET", "/x", nil, hdr{"Cookie": sessionCookieName + "=forged.value"}), http.StatusUnauthorized)
}

func TestServiceMethodsForHTMLTransport(t *testing.T) {
	h := newHarness(t, nil)
	ctx := context.Background()
	email := uniqueEmail(t)

	user, sess, err := h.a.SignUpEmail(ctx, SignUpInput{Name: "HTML", Email: email, Password: "password123"})
	if err != nil || sess.Token == "" || user.Email != email {
		t.Fatalf("SignUpEmail = %v %v %v", user, sess, err)
	}
	if _, _, err := h.a.SignInEmail(ctx, SignInInput{Email: email, Password: "nope-nope"}); !errors.Is(err, ErrInvalidEmailOrPassword) {
		t.Fatalf("wrong password err = %v", err)
	}
	_, sess, err = h.a.SignInEmail(ctx, SignInInput{Email: email, Password: "password123"})
	if err != nil {
		t.Fatal(err)
	}
	rec := h.serve(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { h.a.SetSessionCookie(w, sess) }), "GET", "/", nil, nil)
	cookie := cookieHeader(t, rec)
	if h.getSession(hdr{"Cookie": cookie}) == nil {
		t.Fatal("SetSessionCookie cookie not accepted")
	}
	if err := h.a.SignOut(ctx, sess.Token); err != nil {
		t.Fatal(err)
	}
	if err := h.a.SignOut(ctx, ""); err != nil {
		t.Fatal(err)
	}
	if h.getSession(hdr{"Cookie": cookie}) != nil {
		t.Fatal("SignOut did not revoke")
	}
	rec = h.serve(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { h.a.ClearSessionCookie(w) }), "GET", "/", nil, nil)
	if c := sessionCookie(rec); c == nil || c.MaxAge >= 0 {
		t.Fatal("ClearSessionCookie did not expire the cookie")
	}
	// An already-expired session clears instead of setting.
	rec = h.serve(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		h.a.SetSessionCookie(w, Session{Token: "x", ExpiresAt: h.clock.Now().Add(-time.Minute)})
	}), "GET", "/", nil, nil)
	if c := sessionCookie(rec); c == nil || c.MaxAge >= 0 {
		t.Fatal("expired session set a live cookie")
	}
	if _, _, err := h.a.VerifyEmail(ctx, "garbage"); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("VerifyEmail garbage err = %v", err)
	}
}

// HostOnlyCookies: on https every cookie of the instance is __Host- (Path=/,
// no Domain, Secure), and the __Secure- name is not read.
func TestHostOnlyCookies(t *testing.T) {
	h := newHarness(t, func(c *Config) { c.BaseURL = "https://admin.test"; c.CookiePrefix = "admin"; c.HostOnlyCookies = true })
	rec := h.signUp(uniqueEmail(t), "password123")
	c := sessionCookie(rec)
	if c == nil || c.Name != "__Host-admin.session_token" || !c.Secure || !c.HttpOnly || c.Path != "/" || c.Domain != "" {
		t.Fatalf("cookie = %+v", c)
	}
	if h.getSession(hdr{"Cookie": c.Name + "=" + c.Value}) == nil {
		t.Fatal("own __Host- cookie rejected")
	}
	if h.getSession(hdr{"Cookie": "__Secure-admin.session_token=" + c.Value}) != nil {
		t.Fatal("__Secure- cookie name accepted with HostOnlyCookies")
	}
	// Plain http (dev) keeps the bare name: prefixes need https.
	dev := newHarness(t, func(c *Config) { c.CookiePrefix = "admin"; c.HostOnlyCookies = true })
	if c := sessionCookie(dev.signUp(uniqueEmail(t), "password123")); c == nil || c.Name != "admin.session_token" {
		t.Fatalf("http cookie = %+v", c)
	}
}

func TestSecureCookieOnHTTPS(t *testing.T) {
	h := newHarness(t, func(c *Config) { c.BaseURL = "https://app.test" })
	rec := h.signUp(uniqueEmail(t), "password123")
	c := sessionCookie(rec)
	if c == nil || c.Name != "__Secure-better-auth.session_token" || !c.Secure || !c.HttpOnly {
		t.Fatalf("cookie = %+v", c)
	}
	// The unprefixed cookie name is not read on https.
	if h.getSession(hdr{"Cookie": sessionCookieName + "=" + c.Value}) != nil {
		t.Fatal("non-secure cookie name accepted on https")
	}
	if h.getSession(hdr{"Cookie": c.Name + "=" + c.Value}) == nil {
		t.Fatal("secure cookie rejected")
	}
}

func TestRateLimitedEndpoints(t *testing.T) {
	h := newHarness(t, func(c *Config) { c.RateLimiter = NewMemoryRateLimiter() })
	email := uniqueEmail(t)
	for i := range 3 {
		if code := h.signIn(email, "password123").Code; code != http.StatusUnauthorized {
			t.Fatalf("attempt %d: status %d", i, code)
		}
	}
	rec := h.signIn(email, "password123")
	wantCode(t, rec, http.StatusTooManyRequests, "TOO_MANY_REQUESTS")
	if ra := rec.Header().Get("X-Retry-After"); ra == "" || ra == "0" {
		t.Fatalf("X-Retry-After = %q", ra)
	}
	// Other endpoints and other clients have their own budget.
	wantStatus(t, h.signUp(email, "password123"), http.StatusOK)
	rec = h.do("POST", BasePath+"/sign-in/email", map[string]any{"email": email, "password": "password123"}, nil)
	wantStatus(t, rec, http.StatusTooManyRequests)
	for range 3 {
		wantStatus(t, h.do("POST", BasePath+"/request-password-reset", map[string]any{"email": email, "redirectTo": "/r"}, nil), http.StatusOK)
	}
	wantStatus(t, h.do("POST", BasePath+"/request-password-reset", map[string]any{"email": email, "redirectTo": "/r"}, nil), http.StatusTooManyRequests)

	// Service calls outside Middleware have no client IP and are not limited.
	for range 5 {
		if _, _, err := h.a.SignInEmail(context.Background(), SignInInput{Email: email, Password: "password123"}); err != nil {
			t.Fatal(err)
		}
	}
}

func TestClientIPFromTrustedProxyHeader(t *testing.T) {
	h := newHarness(t, func(c *Config) { c.ClientIPHeader = "X-Forwarded-For" })
	rec := h.do("POST", BasePath+"/sign-up/email", map[string]any{"name": "ip", "email": uniqueEmail(t), "password": "password123"},
		hdr{"X-Forwarded-For": "6.6.6.6, 203.0.113.9", "User-Agent": "test-agent"})
	wantStatus(t, rec, http.StatusOK)
	sess := h.getSession(hdr{"Cookie": cookieHeader(t, rec)})["session"].(map[string]any)
	if sess["ipAddress"] != "203.0.113.9" || sess["userAgent"] != "test-agent" {
		t.Fatalf("session client = %v / %v", sess["ipAddress"], sess["userAgent"])
	}
}

func TestOKEndpoint(t *testing.T) {
	h := newHarness(t, nil)
	rec := h.do("GET", BasePath+"/ok", nil, nil)
	wantStatus(t, rec, http.StatusOK)
	wantStatus(t, h.do("GET", BasePath+"/sign-in/email", nil, nil), http.StatusMethodNotAllowed)
}

func mustQuery(t *testing.T, link, key string) string {
	t.Helper()
	u, err := url.Parse(link)
	if err != nil {
		t.Fatal(err)
	}
	return u.Query().Get(key)
}

// betterAuthHash is hashPassword("hunter2hunter2") from @better-auth/utils
// (scrypt N=16384 r=16 p=1, salt "0123…cdef"), generated with Node.
const betterAuthHash = "0123456789abcdef0123456789abcdef:79729cb5bb1f0806ff13d495dfbe112886f07473fd22165fdad4415930961e224a5c3911b65af1d06f9d38cca6b0eb1a39717181f0abf4f30ff2ddb3bfc5d985"

func TestImportedBetterAuthUserSignsInAndIsRehashed(t *testing.T) {
	h := newHarness(t, nil)
	ctx := context.Background()
	email := uniqueEmail(t)
	id := generateID(32)
	// Rows as the original app's Better Auth wrote them.
	if _, err := testPool.Exec(ctx, `INSERT INTO "user" ("id","name","email","emailVerified") VALUES ($1,'Imported',$2,true)`, id, email); err != nil {
		t.Fatal(err)
	}
	if _, err := testPool.Exec(ctx, `INSERT INTO "account" ("id","accountId","providerId","userId","password") VALUES ($1,$2,'credential',$2,$3)`, generateID(32), id, betterAuthHash); err != nil {
		t.Fatal(err)
	}

	wantCode(t, h.signIn(email, "hunter2hunter3"), http.StatusUnauthorized, "INVALID_EMAIL_OR_PASSWORD")
	wantStatus(t, h.signIn(email, "hunter2hunter2"), http.StatusOK)

	var stored string
	if err := testPool.QueryRow(ctx, `SELECT "password" FROM "account" WHERE "userId" = $1`, id).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(stored, "$argon2id$") {
		t.Fatalf("hash not upgraded: %q", stored)
	}
	wantStatus(t, h.signIn(email, "hunter2hunter2"), http.StatusOK)
}

func TestSquattedSignUpIsReclaimedByEmailOwner(t *testing.T) {
	h := newHarness(t, func(c *Config) { c.RequireEmailVerification = true })
	victim := uniqueEmail(t)
	link := func(i int) string { return pathOf(t, h.mails()[i].url) }

	// An attacker registers the victim's address first; the link goes to the victim.
	wantStatus(t, h.signUp(victim, "attacker-pass"), http.StatusOK)
	attackerLink := link(0)
	// The victim signs up too: same generic response, their password wins,
	// and they get a fresh link.
	wantStatus(t, h.signUp(victim, "victim-pass"), http.StatusOK)
	if n := len(h.mails()); n != 2 {
		t.Fatalf("mails = %d, want 2", n)
	}
	// The link issued for the attacker's password is void.
	if rec := h.do("GET", attackerLink, nil, nil); rec.Header().Get("Location") != "/?error=INVALID_TOKEN" || sessionCookie(rec) != nil {
		t.Fatalf("attacker link: %d %q", rec.Code, rec.Header().Get("Location"))
	}
	victimLink := ""
	for _, m := range h.mails()[1:] {
		victimLink = pathOf(t, m.url)
	}
	rec := h.do("GET", victimLink, nil, nil)
	wantStatus(t, rec, http.StatusFound)
	if sessionCookie(rec) == nil {
		t.Fatal("verification did not sign the owner in")
	}
	wantCode(t, h.signIn(victim, "attacker-pass"), http.StatusUnauthorized, "INVALID_EMAIL_OR_PASSWORD")
	wantStatus(t, h.signIn(victim, "victim-pass"), http.StatusOK)

	// Once verified, a sign-up with the address changes nothing.
	wantStatus(t, h.signUp(victim, "attacker-again"), http.StatusOK)
	wantCode(t, h.signIn(victim, "attacker-again"), http.StatusUnauthorized, "INVALID_EMAIL_OR_PASSWORD")
	if n := len(h.mails()); n != 2 {
		t.Fatalf("verified duplicate sent mail (%d total)", n)
	}
}

func TestResetRevokesOtherResetLinks(t *testing.T) {
	h := newHarness(t, nil)
	email := uniqueEmail(t)
	wantStatus(t, h.signUp(email, "old-password"), http.StatusOK)
	ctx := context.Background()
	for range 2 {
		if err := h.a.RequestPasswordReset(ctx, email, "/r"); err != nil {
			t.Fatal(err)
		}
	}
	var tokens []string
	for _, m := range h.mails() {
		tokens = append(tokens, mustQuery(t, m.url, "token"))
	}
	if err := h.a.ResetPassword(ctx, tokens[0], "new-password"); err != nil {
		t.Fatal(err)
	}
	if err := h.a.ResetPassword(ctx, tokens[1], "attacker-password"); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("second outstanding link still works: %v", err)
	}
	if err := h.a.RequestPasswordReset(ctx, email, ""); !errors.Is(err, ErrInvalidRedirectURL) {
		t.Fatalf("empty redirectTo err = %v", err)
	}
}

func TestSignOutWithBearerAndCookieRevokesBoth(t *testing.T) {
	h := newHarness(t, nil)
	email := uniqueEmail(t)
	cookie := cookieHeader(t, h.signUp(email, "password123"))
	token := jsonBody(t, h.signIn(email, "password123"))["token"].(string)
	rec := h.do("POST", BasePath+"/sign-out", nil, hdr{"Authorization": "Bearer " + token, "Cookie": cookie, "Origin": testBase})
	wantStatus(t, rec, http.StatusOK)
	if h.getSession(hdr{"Cookie": cookie}) != nil || h.getSession(hdr{"Authorization": "Bearer " + token}) != nil {
		t.Fatal("a session survived sign-out")
	}
}
