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

func TestMigrateIsIdempotent(test *testing.T) {
	test.Parallel()
	pool := newDatabase(test) // already migrated (a clone of the template)
	ctx := context.Background()
	if err := Migrate(ctx, pool); err != nil {
		test.Fatal(err)
	}
	var version int64
	if err := pool.QueryRow(ctx, `SELECT max(version_id) FROM goose_auth`).Scan(&version); err != nil {
		test.Fatal(err)
	}
	files, _ := fs.Glob(Migrations, "*.sql")
	if version != int64(len(files)) {
		test.Fatalf("goose_auth version = %d, want %d", version, len(files))
	}
	for _, table := range []string{"user", "session", "account", "verification"} {
		var exists bool
		if err := pool.QueryRow(ctx, `SELECT to_regclass($1) IS NOT NULL`, `"`+table+`"`).Scan(&exists); err != nil || !exists {
			test.Fatalf("table %q missing (err %v)", table, err)
		}
	}
}

func TestNewValidatesConfig(test *testing.T) {
	test.Parallel()
	needDB(test)
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
			test.Errorf("%s: New succeeded, want error", name)
		}
	}
	if _, err := New(Config{Secret: testSecret, BaseURL: testBase}, testPool, nil); err == nil {
		test.Error("nil mailer accepted")
	}
	if _, err := New(Config{Secret: testSecret, BaseURL: testBase}, nil, &captureMailer{}); err == nil {
		test.Error("nil pool accepted")
	}
}

func TestCookieSessionLifecycle(test *testing.T) {
	test.Parallel()
	harness := newHarness(test, nil)
	email := uniqueEmail(test)

	rec := harness.signUp(email, "correct horse")
	wantStatus(test, rec, http.StatusOK)
	body := jsonBody(test, rec)
	user := body["user"].(map[string]any)
	if body["token"] == nil || user["email"] != email || user["emailVerified"] != false {
		test.Fatalf("sign-up body = %v", body)
	}
	issued := sessionCookie(rec)
	if issued == nil || issued.Name != "better-auth.session_token" || !issued.HttpOnly || issued.Secure || issued.SameSite != http.SameSiteLaxMode || issued.Path != "/" {
		test.Fatalf("session cookie = %+v", issued)
	}
	if issued.MaxAge < int((7*24*time.Hour).Seconds())-5 {
		test.Fatalf("cookie MaxAge = %d, want ~7 days", issued.MaxAge)
	}
	if rec.Header().Get("Set-Auth-Token") == "" {
		test.Fatal("set-auth-token header missing")
	}
	cookie := cookieHeader(test, rec)

	got := harness.getSession(hdr{"Cookie": cookie})
	if got == nil || got["user"].(map[string]any)["email"] != email {
		test.Fatalf("get-session = %v", got)
	}
	sess := got["session"].(map[string]any)
	if sess["token"] != body["token"] || sess["userId"] != user["id"] || sess["ipAddress"] != "192.0.2.1" {
		test.Fatalf("session = %v", sess)
	}

	// Sign-out with cookies needs a trusted origin.
	rec = harness.do("POST", BasePath+"/sign-out", nil, hdr{"Cookie": cookie, "Origin": testBase})
	wantStatus(test, rec, http.StatusOK)
	if jsonBody(test, rec)["success"] != true {
		test.Fatalf("sign-out body = %s", rec.Body)
	}
	if cookie := sessionCookie(rec); cookie == nil || cookie.MaxAge >= 0 {
		test.Fatalf("sign-out did not clear the cookie: %+v", cookie)
	}
	// The old cookie is dead immediately (no cookie cache).
	if got := harness.getSession(hdr{"Cookie": cookie}); got != nil {
		test.Fatalf("session survived sign-out: %v", got)
	}
	var count int
	_ = testPool.QueryRow(context.Background(), `SELECT count(*) FROM "session" WHERE "token" = $1`, body["token"]).Scan(&count)
	if count != 0 {
		test.Fatalf("session row still present")
	}

	// Sign back in with the same password.
	rec = harness.signIn(email, "correct horse")
	wantStatus(test, rec, http.StatusOK)
	if body := jsonBody(test, rec); body["redirect"] != false || body["token"] == "" {
		test.Fatalf("sign-in body = %v", body)
	}
}

func TestBearerSession(test *testing.T) {
	test.Parallel()
	harness := newHarness(test, nil)
	email := uniqueEmail(test)
	wantStatus(test, harness.signUp(email, "password123"), http.StatusOK)

	rec := harness.signIn(email, "password123")
	wantStatus(test, rec, http.StatusOK)
	token := jsonBody(test, rec)["token"].(string)
	signed := rec.Header().Get("Set-Auth-Token")
	if !strings.HasPrefix(signed, token+".") {
		test.Fatalf("set-auth-token = %q, want signed %q", signed, token)
	}

	for name, value := range map[string]string{
		"raw token":         "Bearer " + token,
		"signed token":      "Bearer " + signed,
		"url-encoded":       "Bearer " + url.QueryEscape(signed),
		"lowercase scheme":  "bearer " + token,
		"surrounding space": "Bearer   " + token + " ",
	} {
		if got := harness.getSession(hdr{"Authorization": value}); got == nil {
			test.Errorf("%s: no session", name)
		}
	}
	for name, value := range map[string]string{
		"tampered signature": "Bearer " + token + ".AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=",
		"unknown token":      "Bearer " + strings.Repeat("x", 32),
		"basic scheme":       "Basic " + token,
		"empty":              "Bearer ",
	} {
		if got := harness.getSession(hdr{"Authorization": value}); got != nil {
			test.Errorf("%s: got a session", name)
		}
	}

	// A bearer request carries no cookies, so sign-out needs no origin.
	rec = harness.do("POST", BasePath+"/sign-out", nil, hdr{"Authorization": "Bearer " + token})
	wantStatus(test, rec, http.StatusOK)
	if got := harness.getSession(hdr{"Authorization": "Bearer " + token}); got != nil {
		test.Fatal("bearer session survived sign-out")
	}
}

func TestSignInRejectsBadCredentials(test *testing.T) {
	test.Parallel()
	harness := newHarness(test, nil)
	email := uniqueEmail(test)
	wantStatus(test, harness.signUp(email, "password123"), http.StatusOK)

	wantCode(test, harness.signIn(email, "wrong-password"), http.StatusUnauthorized, "INVALID_EMAIL_OR_PASSWORD")
	wantCode(test, harness.signIn(uniqueEmail(test), "password123"), http.StatusUnauthorized, "INVALID_EMAIL_OR_PASSWORD")
	wantCode(test, harness.signIn(strings.ToUpper(email), "wrong-password"), http.StatusUnauthorized, "INVALID_EMAIL_OR_PASSWORD")
	wantStatus(test, harness.signIn(strings.ToUpper(email), "password123"), http.StatusOK) // emails are case-insensitive
	wantCode(test, harness.signIn("not-an-email", "password123"), http.StatusBadRequest, "INVALID_EMAIL")
	wantCode(test, harness.signIn(email, strings.Repeat("a", 129)), http.StatusBadRequest, "PASSWORD_TOO_LONG")
}

func TestSignUpValidation(test *testing.T) {
	test.Parallel()
	harness := newHarness(test, nil)
	wantCode(test, harness.signUp(uniqueEmail(test), "short"), http.StatusBadRequest, "PASSWORD_TOO_SHORT")
	wantCode(test, harness.signUp(uniqueEmail(test), strings.Repeat("é", 129)), http.StatusBadRequest, "PASSWORD_TOO_LONG")
	wantStatus(test, harness.signUp(uniqueEmail(test), strings.Repeat("é", 128)), http.StatusOK)
	for _, bad := range []string{"", "plain", "a@b", "Name <a@example.com>", "a@example.com\n"} {
		wantCode(test, harness.signUp(bad, "password123"), http.StatusBadRequest, "INVALID_EMAIL")
	}

	rec := harness.do("POST", BasePath+"/sign-up/email", nil, hdr{"Content-Type": "text/plain"})
	wantCode(test, rec, http.StatusUnsupportedMediaType, "UNSUPPORTED_MEDIA_TYPE")
	req := harness.do("POST", BasePath+"/sign-up/email", "not an object", nil)
	wantCode(test, req, http.StatusBadRequest, "INVALID_REQUEST_BODY")

	email := uniqueEmail(test)
	wantStatus(test, harness.signUp(email, "password123"), http.StatusOK)
	wantCode(test, harness.signUp(email, "password123"), http.StatusUnprocessableEntity, "USER_ALREADY_EXISTS_USE_ANOTHER_EMAIL")
	wantCode(test, harness.signUp(strings.ToUpper(email), "password123"), http.StatusUnprocessableEntity, "USER_ALREADY_EXISTS_USE_ANOTHER_EMAIL")
}

func TestEmailVerificationFlow(test *testing.T) {
	test.Parallel()
	harness := newHarness(test, func(config *Config) { config.RequireEmailVerification = true })
	email := uniqueEmail(test)

	rec := harness.do("POST", BasePath+"/sign-up/email", map[string]any{
		"name": "V", "email": email, "password": "password123", "callbackURL": "/auth/verify-email/success",
	}, nil)
	wantStatus(test, rec, http.StatusOK)
	if body := jsonBody(test, rec); body["token"] != nil || sessionCookie(rec) != nil {
		test.Fatalf("sign-up signed in before verification: %v", body)
	}
	mails := harness.mails()
	if len(mails) != 1 || mails[0].kind != "verification" || mails[0].to != email {
		test.Fatalf("mails = %+v", mails)
	}
	link := mails[0].url
	if !strings.HasPrefix(link, testBase+"/api/auth/verify-email?token=") || !strings.Contains(link, "callbackURL=%2Fauth%2Fverify-email%2Fsuccess") {
		test.Fatalf("verification link = %s", link)
	}

	// Unverified: right password is blocked, wrong password stays generic.
	wantCode(test, harness.signIn(email, "password123"), http.StatusForbidden, "EMAIL_NOT_VERIFIED")
	wantCode(test, harness.signIn(email, "wrong-password"), http.StatusUnauthorized, "INVALID_EMAIL_OR_PASSWORD")

	// A duplicate sign-up of the unverified account gets the same generic
	// response; it resets the password and sends a new link that replaces
	// the first one.
	rec = harness.do("POST", BasePath+"/sign-up/email", map[string]any{
		"name": "V", "email": email, "password": "password123", "callbackURL": "/auth/verify-email/success",
	}, nil)
	wantStatus(test, rec, http.StatusOK)
	if body := jsonBody(test, rec); body["token"] != nil {
		test.Fatalf("duplicate sign-up = %v", body)
	}
	if mails = harness.mails(); len(mails) != 2 {
		test.Fatalf("mails = %+v", mails)
	}
	wantCode(test, harness.do("GET", BasePath+"/verify-email?token="+mustQuery(test, link, "token"), nil, nil), http.StatusUnauthorized, "INVALID_TOKEN")
	link = mails[1].url

	// Following the link verifies, signs in and redirects to the callback.
	rec = harness.do("GET", pathOf(test, link), nil, nil)
	wantStatus(test, rec, http.StatusFound)
	if loc := rec.Header().Get("Location"); loc != "/auth/verify-email/success" {
		test.Fatalf("redirect = %q", loc)
	}
	cookie := cookieHeader(test, rec)
	got := harness.getSession(hdr{"Cookie": cookie})
	if got == nil || got["user"].(map[string]any)["emailVerified"] != true {
		test.Fatalf("after verification get-session = %v", got)
	}

	// The used link no longer signs anyone in.
	rec = harness.do("GET", pathOf(test, link), nil, nil)
	wantStatus(test, rec, http.StatusFound)
	if sessionCookie(rec) != nil {
		test.Fatal("reused verification link issued a session")
	}
	wantStatus(test, harness.signIn(email, "password123"), http.StatusOK)
}

func TestVerifyEmailRejectsBadTokens(test *testing.T) {
	test.Parallel()
	harness := newHarness(test, func(config *Config) { config.RequireEmailVerification = true })
	email := uniqueEmail(test)
	wantStatus(test, harness.signUp(email, "password123"), http.StatusOK)
	link := harness.mails()[0].url
	token := mustQuery(test, link, "token")

	wantCode(test, harness.do("GET", BasePath+"/verify-email?token="+token+"x", nil, nil), http.StatusUnauthorized, "INVALID_TOKEN")
	wantCode(test, harness.do("GET", BasePath+"/verify-email", nil, nil), http.StatusUnauthorized, "INVALID_TOKEN")
	other := unitAuthWith(strings.Repeat("z", 40))
	wantCode(test, harness.do("GET", BasePath+"/verify-email?token="+other.signVerificationToken(email, "", time.Hour), nil, nil), http.StatusUnauthorized, "INVALID_TOKEN")
	wantCode(test, harness.do("GET", BasePath+"/verify-email?token="+harness.auth.signVerificationToken(uniqueEmail(test), "", time.Hour), nil, nil), http.StatusUnauthorized, "USER_NOT_FOUND")
	wantCode(test, harness.do("GET", BasePath+"/verify-email?token="+token+"&callbackURL=https://evil.test/", nil, nil), http.StatusForbidden, "INVALID_CALLBACK_URL")

	harness.clock.Advance(VerificationTokenTTL + time.Minute)
	wantCode(test, harness.do("GET", BasePath+"/verify-email?token="+token, nil, nil), http.StatusUnauthorized, "TOKEN_EXPIRED")
	rec := harness.do("GET", pathOf(test, link), nil, nil)
	wantStatus(test, rec, http.StatusFound)
	if loc := rec.Header().Get("Location"); loc != "/?error=TOKEN_EXPIRED" {
		test.Fatalf("expired redirect = %q", loc)
	}
	if sessionCookie(rec) != nil {
		test.Fatal("expired token issued a session")
	}
	wantCode(test, harness.signIn(email, "password123"), http.StatusForbidden, "EMAIL_NOT_VERIFIED")
}

func TestSendVerificationEmail(test *testing.T) {
	test.Parallel()
	harness := newHarness(test, func(config *Config) { config.RequireEmailVerification = true })
	unverified := uniqueEmail(test)
	wantStatus(test, harness.signUp(unverified, "password123"), http.StatusOK)
	send := func(email string, headers hdr) int {
		return harness.do("POST", BasePath+"/send-verification-email", map[string]any{"email": email, "callbackURL": "/done"}, headers).Code
	}

	if code := send(unverified, nil); code != http.StatusOK {
		test.Fatalf("resend status %d", code)
	}
	if code := send(uniqueEmail(test), nil); code != http.StatusOK {
		test.Fatalf("unknown email status %d", code)
	}
	mails := harness.mails()
	// Sends run in the background, so look the resend up rather than rely on order.
	resent := slices.IndexFunc(mails, func(mail sentMail) bool { return strings.Contains(mail.url, "callbackURL=%2Fdone") })
	if len(mails) != 2 || resent < 0 || mails[resent].to != unverified {
		test.Fatalf("mails = %+v", mails)
	}

	// Verify, then a signed-in resend is refused and a signed-out one is silent.
	rec := harness.do("GET", pathOf(test, mails[resent].url), nil, nil)
	cookie := cookieHeader(test, rec)
	wantCode(test, harness.do("POST", BasePath+"/send-verification-email", map[string]any{"email": unverified}, hdr{"Cookie": cookie, "Origin": testBase}), http.StatusBadRequest, "EMAIL_ALREADY_VERIFIED")
	wantCode(test, harness.do("POST", BasePath+"/send-verification-email", map[string]any{"email": uniqueEmail(test)}, hdr{"Cookie": cookie, "Origin": testBase}), http.StatusBadRequest, "EMAIL_MISMATCH")
	if code := send(unverified, nil); code != http.StatusOK {
		test.Fatalf("verified resend status %d", code)
	}
	if got := len(harness.mails()); got != 2 {
		test.Fatalf("verified user got another email (%d total)", got)
	}
}

func TestPasswordResetFlow(test *testing.T) {
	test.Parallel()
	harness := newHarness(test, nil)
	email := uniqueEmail(test)
	wantStatus(test, harness.signUp(email, "old-password"), http.StatusOK)
	cookieA := cookieHeader(test, harness.signIn(email, "old-password"))
	tokenB := jsonBody(test, harness.signIn(email, "old-password"))["token"].(string)

	request := func(email string) map[string]any {
		rec := harness.do("POST", BasePath+"/request-password-reset", map[string]any{"email": email, "redirectTo": "/auth/reset-password"}, nil)
		wantStatus(test, rec, http.StatusOK)
		return jsonBody(test, rec)
	}
	known, unknown := request(email), request(uniqueEmail(test))
	if known["status"] != true || known["message"] != unknown["message"] || unknown["status"] != true {
		test.Fatalf("responses differ: %v vs %v", known, unknown)
	}
	mails := harness.mails()
	if len(mails) != 1 || mails[0].kind != "reset" || mails[0].to != email {
		test.Fatalf("mails = %+v", mails)
	}
	// A relative redirectTo is linked directly, with the token.
	link := mails[0].url
	token := mustQuery(test, link, "token")
	if !strings.HasPrefix(link, testBase+"/auth/reset-password?token=") || token == "" {
		test.Fatalf("reset link = %s", link)
	}
	// The API callback still accepts the token (absolute and deep-link targets use it).
	apiLink := BasePath + "/reset-password/" + token + "?callbackURL=/auth/reset-password"
	if loc := harness.do("GET", apiLink, nil, nil).Header().Get("Location"); loc != "/auth/reset-password?token="+token {
		test.Fatalf("callback redirect = %q", loc)
	}

	wantCode(test, harness.do("POST", BasePath+"/reset-password", map[string]any{"token": token, "newPassword": "short"}, nil), http.StatusBadRequest, "PASSWORD_TOO_SHORT")
	rec := harness.do("POST", BasePath+"/reset-password", map[string]any{"token": token, "newPassword": "new-password"}, nil)
	wantStatus(test, rec, http.StatusOK)

	// Every existing session is revoked, the old password is dead.
	if harness.getSession(hdr{"Cookie": cookieA}) != nil || harness.getSession(hdr{"Authorization": "Bearer " + tokenB}) != nil {
		test.Fatal("sessions survived the password reset")
	}
	wantCode(test, harness.signIn(email, "old-password"), http.StatusUnauthorized, "INVALID_EMAIL_OR_PASSWORD")
	wantStatus(test, harness.signIn(email, "new-password"), http.StatusOK)

	// Single use: the token and its link are spent.
	wantCode(test, harness.do("POST", BasePath+"/reset-password", map[string]any{"token": token, "newPassword": "third-password"}, nil), http.StatusBadRequest, "INVALID_TOKEN")
	rec = harness.do("GET", apiLink, nil, nil)
	if loc := rec.Header().Get("Location"); loc != "/auth/reset-password?error=INVALID_TOKEN" {
		test.Fatalf("spent link redirect = %q", loc)
	}
	wantCode(test, harness.do("POST", BasePath+"/reset-password", map[string]any{"newPassword": "third-password"}, nil), http.StatusBadRequest, "INVALID_TOKEN")
	wantCode(test, harness.do("POST", BasePath+"/reset-password?token=bad%20token", map[string]any{"newPassword": "third-password"}, nil), http.StatusBadRequest, "INVALID_TOKEN")
}

func TestPasswordResetTokenExpires(test *testing.T) {
	test.Parallel()
	harness := newHarness(test, nil)
	email := uniqueEmail(test)
	wantStatus(test, harness.signUp(email, "old-password"), http.StatusOK)
	if err := harness.auth.RequestPasswordReset(context.Background(), email, testBase+"/reset"); err != nil {
		test.Fatal(err)
	}
	link := harness.mails()[0].url
	token := strings.TrimPrefix(pathOf(test, link), BasePath+"/reset-password/")
	token = token[:strings.IndexByte(token, '?')]

	harness.clock.Advance(ResetPasswordTokenTTL + time.Second)
	rec := harness.do("GET", pathOf(test, link), nil, nil)
	if loc := rec.Header().Get("Location"); loc != testBase+"/reset?error=INVALID_TOKEN" {
		test.Fatalf("expired link redirect = %q", loc)
	}
	if err := harness.auth.ResetPassword(context.Background(), token, "new-password"); !errors.Is(err, ErrInvalidToken) {
		test.Fatalf("expired reset err = %v", err)
	}
	wantStatus(test, harness.signIn(email, "old-password"), http.StatusOK)
}

func TestPasswordResetTokenIsSingleUseUnderConcurrency(test *testing.T) {
	test.Parallel()
	harness := newHarness(test, nil)
	email := uniqueEmail(test)
	wantStatus(test, harness.signUp(email, "old-password"), http.StatusOK)
	if err := harness.auth.RequestPasswordReset(context.Background(), email, "/reset"); err != nil {
		test.Fatal(err)
	}
	token := mustQuery(test, harness.mails()[0].url, "token")

	var wg sync.WaitGroup
	results := make([]error, 8)
	for index := range results {
		wg.Go(func() { results[index] = harness.auth.ResetPassword(context.Background(), token, "new-password") })
	}
	wg.Wait()
	ok := 0
	for _, err := range results {
		if err == nil {
			ok++
		} else if !errors.Is(err, ErrInvalidToken) {
			test.Fatalf("unexpected error: %v", err)
		}
	}
	if ok != 1 {
		test.Fatalf("%d resets succeeded with one token, want 1", ok)
	}
}

func TestRedirectTargetsMustBeTrusted(test *testing.T) {
	test.Parallel()
	harness := newHarness(test, nil)
	email := uniqueEmail(test)
	wantStatus(test, harness.signUp(email, "password123"), http.StatusOK)

	for _, target := range []string{"https://evil.test/reset", "//evil.test/reset", "/\\evil.test", "javascript:alert(1)"} {
		rec := harness.do("POST", BasePath+"/request-password-reset", map[string]any{"email": email, "redirectTo": target}, nil)
		wantCode(test, rec, http.StatusForbidden, "INVALID_REDIRECT_URL")
		rec = harness.do("POST", BasePath+"/sign-in/email", map[string]any{"email": email, "password": "password123", "callbackURL": target}, nil)
		wantCode(test, rec, http.StatusForbidden, "INVALID_CALLBACK_URL")
	}
	if got := len(harness.mails()); got != 0 {
		test.Fatalf("untrusted redirect sent %d mails", got)
	}
	rec := harness.do("GET", BasePath+"/reset-password/abc?callbackURL=https://evil.test", nil, nil)
	wantCode(test, rec, http.StatusForbidden, "INVALID_CALLBACK_URL")
	rec = harness.do("GET", BasePath+"/reset-password/abc", nil, nil)
	wantCode(test, rec, http.StatusBadRequest, "INVALID_TOKEN")

	rec = harness.do("POST", BasePath+"/sign-in/email", map[string]any{"email": email, "password": "password123", "callbackURL": "betterstack://home"}, nil)
	wantStatus(test, rec, http.StatusOK)
	if body := jsonBody(test, rec); body["redirect"] != true || body["url"] != "betterstack://home" || rec.Header().Get("Location") != "betterstack://home" {
		test.Fatalf("sign-in with callback = %v", body)
	}
}

func TestOriginCheck(test *testing.T) {
	test.Parallel()
	harness := newHarness(test, nil)
	email := uniqueEmail(test)
	cookie := cookieHeader(test, harness.signUp(email, "password123"))
	signOut := func(headers hdr) int { return harness.do("POST", BasePath+"/sign-out", nil, headers).Code }

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
	for _, testCase := range cases {
		if got := signOut(testCase.headers); got != testCase.want {
			test.Errorf("%s: status %d, want %d", testCase.name, got, testCase.want)
		}
	}

	// exp:// is only trusted outside production.
	prod := newHarness(test, func(config *Config) { config.TrustedOrigins = MobileTrustedOrigins(true) })
	if got := prod.do("POST", BasePath+"/sign-out", nil, hdr{"Cookie": cookie, "Origin": "exp://192.168.1.20:8081"}).Code; got != http.StatusForbidden {
		test.Errorf("production accepted exp:// (status %d)", got)
	}

	// Sign-in/up without cookies: browsers are identified by Fetch Metadata.
	body := map[string]any{"email": email, "password": "password123"}
	signIn := func(headers hdr) *httpResult {
		rec := harness.do("POST", BasePath+"/sign-in/email", body, headers)
		return &httpResult{rec.Code, rec.Body.String()}
	}
	if result := signIn(hdr{"Sec-Fetch-Site": "cross-site", "Sec-Fetch-Mode": "navigate", "Origin": testBase}); result.code != http.StatusForbidden || !strings.Contains(result.body, "CROSS_SITE_NAVIGATION_LOGIN_BLOCKED") {
		test.Errorf("cross-site navigation: %+v", result)
	}
	if result := signIn(hdr{"Sec-Fetch-Site": "cross-site", "Sec-Fetch-Mode": "cors", "Origin": "https://evil.test"}); result.code != http.StatusForbidden || !strings.Contains(result.body, "INVALID_ORIGIN") {
		test.Errorf("cross-site fetch: %+v", result)
	}
	if result := signIn(hdr{"Sec-Fetch-Site": "same-origin", "Sec-Fetch-Mode": "cors"}); result.code != http.StatusForbidden || !strings.Contains(result.body, "MISSING_OR_NULL_ORIGIN") {
		test.Errorf("browser without origin: %+v", result)
	}
	if result := signIn(hdr{"Sec-Fetch-Site": "same-origin", "Sec-Fetch-Mode": "cors", "Origin": testBase}); result.code != http.StatusOK {
		test.Errorf("same-origin fetch: %+v", result)
	}
	if result := signIn(hdr{"Cookie": "unrelated=1", "Origin": "https://evil.test"}); result.code != http.StatusForbidden {
		test.Errorf("sign-in with cookies from evil origin: %+v", result)
	}
	if result := signIn(nil); result.code != http.StatusOK {
		test.Errorf("native client: %+v", result)
	}
}

type httpResult struct {
	code int
	body string
}

func TestSessionRefreshAndExpiry(test *testing.T) {
	test.Parallel()
	harness := newHarness(test, nil)
	rec := harness.signUp(uniqueEmail(test), "password123")
	cookie := cookieHeader(test, rec)
	token := jsonBody(test, rec)["token"].(string)
	expiresAt := func() time.Time {
		var at time.Time
		if err := testPool.QueryRow(context.Background(), `SELECT "expiresAt" FROM "session" WHERE "token" = $1`, token).Scan(&at); err != nil {
			test.Fatal(err)
		}
		return at
	}
	initial := expiresAt()

	// Within the update age nothing changes.
	harness.clock.Advance(23 * time.Hour)
	rec = harness.do("GET", BasePath+"/get-session", nil, hdr{"Cookie": cookie})
	if sessionCookie(rec) != nil || !expiresAt().Equal(initial) {
		test.Fatal("session refreshed before the update age")
	}

	// After a day of use the session is extended to now + 7 days.
	harness.clock.Advance(2 * time.Hour)
	rec = harness.do("GET", BasePath+"/get-session", nil, hdr{"Cookie": cookie})
	if sessionCookie(rec) == nil {
		test.Fatal("refreshed session did not reset the cookie")
	}
	if want := harness.clock.Now().Add(DefaultSessionTTL); expiresAt().Sub(want).Abs() > time.Millisecond {
		test.Fatalf("expiresAt = %v, want %v", expiresAt(), want)
	}
	// Bearer refreshes too, without setting cookies.
	harness.clock.Advance(25 * time.Hour)
	rec = harness.do("GET", BasePath+"/get-session", nil, hdr{"Authorization": "Bearer " + token})
	if sessionCookie(rec) != nil {
		test.Fatal("bearer request got a cookie")
	}

	// Past expiry the session is gone and its row deleted.
	harness.clock.Advance(DefaultSessionTTL + time.Second)
	rec = harness.do("GET", BasePath+"/get-session", nil, hdr{"Cookie": cookie})
	if strings.TrimSpace(rec.Body.String()) != "null" {
		test.Fatalf("expired session returned %s", rec.Body)
	}
	if cookie := sessionCookie(rec); cookie == nil || cookie.MaxAge >= 0 {
		test.Fatal("expired session cookie not cleared")
	}
	var count int
	_ = testPool.QueryRow(context.Background(), `SELECT count(*) FROM "session" WHERE "token" = $1`, token).Scan(&count)
	if count != 0 {
		test.Fatal("expired session row not deleted")
	}
	if rec.Header().Get("Cache-Control") != "no-store" {
		test.Fatal("get-session is cacheable")
	}
}

func TestRequireUserAndUserFrom(test *testing.T) {
	test.Parallel()
	harness := newHarness(test, func(config *Config) { config.LoginPath = "/sign-in" })
	email := uniqueEmail(test)
	cookie := cookieHeader(test, harness.signUp(email, "password123"))

	protected := harness.auth.RequireUser(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		user, sess, ok := UserFrom(request.Context())
		if !ok || user.Email != email || sess.UserID != user.ID {
			test.Errorf("UserFrom = %v %v %v", user, sess, ok)
		}
		writer.WriteHeader(http.StatusNoContent)
	}))

	rec := harness.serve(protected, "GET", "/api/core/v1/users", nil, nil)
	wantStatus(test, rec, http.StatusUnauthorized)
	if strings.TrimSpace(rec.Body.String()) != `{"data":null,"error":"Unauthorized"}` {
		test.Fatalf("401 body = %s", rec.Body)
	}
	rec = harness.serve(protected, "GET", "/dashboard", nil, hdr{"Accept": "text/html,application/xhtml+xml"})
	wantStatus(test, rec, http.StatusSeeOther)
	if rec.Header().Get("Location") != "/sign-in" {
		test.Fatalf("redirect = %q", rec.Header().Get("Location"))
	}
	wantStatus(test, harness.serve(protected, "GET", "/dashboard", nil, hdr{"Cookie": cookie}), http.StatusNoContent)
	// Middleware applied twice loads the session once and still works.
	wantStatus(test, harness.serve(harness.auth.Middleware(protected), "GET", "/dashboard", nil, hdr{"Cookie": cookie}), http.StatusNoContent)

	if _, _, ok := UserFrom(context.Background()); ok {
		test.Fatal("UserFrom on an empty context reported a user")
	}
	// A forged cookie is ignored (and not even looked up).
	wantStatus(test, harness.serve(protected, "GET", "/x", nil, hdr{"Cookie": sessionCookieName + "=forged.value"}), http.StatusUnauthorized)
}

func TestServiceMethodsForHTMLTransport(test *testing.T) {
	test.Parallel()
	harness := newHarness(test, nil)
	ctx := context.Background()
	email := uniqueEmail(test)

	user, sess, err := harness.auth.SignUpEmail(ctx, SignUpInput{Name: "HTML", Email: email, Password: "password123"})
	if err != nil || sess.Token == "" || user.Email != email {
		test.Fatalf("SignUpEmail = %v %v %v", user, sess, err)
	}
	if _, _, err := harness.auth.SignInEmail(ctx, SignInInput{Email: email, Password: "nope-nope"}); !errors.Is(err, ErrInvalidEmailOrPassword) {
		test.Fatalf("wrong password err = %v", err)
	}
	_, sess, err = harness.auth.SignInEmail(ctx, SignInInput{Email: email, Password: "password123"})
	if err != nil {
		test.Fatal(err)
	}
	rec := harness.serve(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) { harness.auth.SetSessionCookie(writer, sess) }), "GET", "/", nil, nil)
	cookie := cookieHeader(test, rec)
	if harness.getSession(hdr{"Cookie": cookie}) == nil {
		test.Fatal("SetSessionCookie cookie not accepted")
	}
	if err := harness.auth.SignOut(ctx, sess.Token); err != nil {
		test.Fatal(err)
	}
	if err := harness.auth.SignOut(ctx, ""); err != nil {
		test.Fatal(err)
	}
	if harness.getSession(hdr{"Cookie": cookie}) != nil {
		test.Fatal("SignOut did not revoke")
	}
	rec = harness.serve(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) { harness.auth.ClearSessionCookie(writer) }), "GET", "/", nil, nil)
	if cookie := sessionCookie(rec); cookie == nil || cookie.MaxAge >= 0 {
		test.Fatal("ClearSessionCookie did not expire the cookie")
	}
	// An already-expired session clears instead of setting.
	rec = harness.serve(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		harness.auth.SetSessionCookie(writer, Session{Token: "x", ExpiresAt: harness.clock.Now().Add(-time.Minute)})
	}), "GET", "/", nil, nil)
	if cookie := sessionCookie(rec); cookie == nil || cookie.MaxAge >= 0 {
		test.Fatal("expired session set a live cookie")
	}
	if _, _, err := harness.auth.VerifyEmail(ctx, "garbage"); !errors.Is(err, ErrInvalidToken) {
		test.Fatalf("VerifyEmail garbage err = %v", err)
	}
}

// HostOnlyCookies: on https every cookie of the instance is __Host- (Path=/,
// no Domain, Secure), and the __Secure- name is not read.
func TestHostOnlyCookies(test *testing.T) {
	test.Parallel()
	harness := newHarness(test, func(config *Config) {
		config.BaseURL = "https://admin.test"
		config.CookiePrefix = "admin"
		config.HostOnlyCookies = true
	})
	rec := harness.signUp(uniqueEmail(test), "password123")
	cookie := sessionCookie(rec)
	if cookie == nil || cookie.Name != "__Host-admin.session_token" || !cookie.Secure || !cookie.HttpOnly || cookie.Path != "/" || cookie.Domain != "" {
		test.Fatalf("cookie = %+v", cookie)
	}
	if harness.getSession(hdr{"Cookie": cookie.Name + "=" + cookie.Value}) == nil {
		test.Fatal("own __Host- cookie rejected")
	}
	if harness.getSession(hdr{"Cookie": "__Secure-admin.session_token=" + cookie.Value}) != nil {
		test.Fatal("__Secure- cookie name accepted with HostOnlyCookies")
	}
	// Plain http (dev) keeps the bare name: prefixes need https.
	dev := newHarness(test, func(config *Config) { config.CookiePrefix = "admin"; config.HostOnlyCookies = true })
	if cookie := sessionCookie(dev.signUp(uniqueEmail(test), "password123")); cookie == nil || cookie.Name != "admin.session_token" {
		test.Fatalf("http cookie = %+v", cookie)
	}
}

func TestSecureCookieOnHTTPS(test *testing.T) {
	test.Parallel()
	harness := newHarness(test, func(config *Config) { config.BaseURL = "https://app.test" })
	rec := harness.signUp(uniqueEmail(test), "password123")
	cookie := sessionCookie(rec)
	if cookie == nil || cookie.Name != "__Secure-better-auth.session_token" || !cookie.Secure || !cookie.HttpOnly {
		test.Fatalf("cookie = %+v", cookie)
	}
	// The unprefixed cookie name is not read on https.
	if harness.getSession(hdr{"Cookie": sessionCookieName + "=" + cookie.Value}) != nil {
		test.Fatal("non-secure cookie name accepted on https")
	}
	if harness.getSession(hdr{"Cookie": cookie.Name + "=" + cookie.Value}) == nil {
		test.Fatal("secure cookie rejected")
	}
}

func TestRateLimitedEndpoints(test *testing.T) {
	test.Parallel()
	harness := newHarness(test, func(config *Config) { config.RateLimiter = NewMemoryRateLimiter() })
	email := uniqueEmail(test)
	for index := range 3 {
		if code := harness.signIn(email, "password123").Code; code != http.StatusUnauthorized {
			test.Fatalf("attempt %d: status %d", index, code)
		}
	}
	rec := harness.signIn(email, "password123")
	wantCode(test, rec, http.StatusTooManyRequests, "TOO_MANY_REQUESTS")
	if ra := rec.Header().Get("X-Retry-After"); ra == "" || ra == "0" {
		test.Fatalf("X-Retry-After = %q", ra)
	}
	// Other endpoints and other clients have their own budget.
	wantStatus(test, harness.signUp(email, "password123"), http.StatusOK)
	rec = harness.do("POST", BasePath+"/sign-in/email", map[string]any{"email": email, "password": "password123"}, nil)
	wantStatus(test, rec, http.StatusTooManyRequests)
	for range 3 {
		wantStatus(test, harness.do("POST", BasePath+"/request-password-reset", map[string]any{"email": email, "redirectTo": "/r"}, nil), http.StatusOK)
	}
	wantStatus(test, harness.do("POST", BasePath+"/request-password-reset", map[string]any{"email": email, "redirectTo": "/r"}, nil), http.StatusTooManyRequests)

	// Service calls outside Middleware have no client IP and are not limited.
	for range 5 {
		if _, _, err := harness.auth.SignInEmail(context.Background(), SignInInput{Email: email, Password: "password123"}); err != nil {
			test.Fatal(err)
		}
	}
}

func TestClientIPFromTrustedProxyHeader(test *testing.T) {
	test.Parallel()
	harness := newHarness(test, func(config *Config) { config.ClientIPHeader = "X-Forwarded-For" })
	rec := harness.do("POST", BasePath+"/sign-up/email", map[string]any{"name": "ip", "email": uniqueEmail(test), "password": "password123"},
		hdr{"X-Forwarded-For": "6.6.6.6, 203.0.113.9", "User-Agent": "test-agent"})
	wantStatus(test, rec, http.StatusOK)
	sess := harness.getSession(hdr{"Cookie": cookieHeader(test, rec)})["session"].(map[string]any)
	if sess["ipAddress"] != "203.0.113.9" || sess["userAgent"] != "test-agent" {
		test.Fatalf("session client = %v / %v", sess["ipAddress"], sess["userAgent"])
	}
}

func TestOKEndpoint(test *testing.T) {
	test.Parallel()
	harness := newHarness(test, nil)
	rec := harness.do("GET", BasePath+"/ok", nil, nil)
	wantStatus(test, rec, http.StatusOK)
	wantStatus(test, harness.do("GET", BasePath+"/sign-in/email", nil, nil), http.StatusMethodNotAllowed)
}

func mustQuery(test *testing.T, link, key string) string {
	test.Helper()
	address, err := url.Parse(link)
	if err != nil {
		test.Fatal(err)
	}
	return address.Query().Get(key)
}

// betterAuthHash is hashPassword("hunter2hunter2") from @better-auth/utils
// (scrypt N=16384 r=16 p=1, salt "0123…cdef"), generated with Node.
const betterAuthHash = "0123456789abcdef0123456789abcdef:79729cb5bb1f0806ff13d495dfbe112886f07473fd22165fdad4415930961e224a5c3911b65af1d06f9d38cca6b0eb1a39717181f0abf4f30ff2ddb3bfc5d985"

func TestImportedBetterAuthUserSignsInAndIsRehashed(test *testing.T) {
	test.Parallel()
	harness := newHarness(test, nil)
	ctx := context.Background()
	email := uniqueEmail(test)
	id := generateID(32)
	// Rows as the original app's Better Auth wrote them.
	if _, err := testPool.Exec(ctx, `INSERT INTO "user" ("id","name","email","emailVerified") VALUES ($1,'Imported',$2,true)`, id, email); err != nil {
		test.Fatal(err)
	}
	if _, err := testPool.Exec(ctx, `INSERT INTO "account" ("id","accountId","providerId","userId","password") VALUES ($1,$2,'credential',$2,$3)`, generateID(32), id, betterAuthHash); err != nil {
		test.Fatal(err)
	}

	wantCode(test, harness.signIn(email, "hunter2hunter3"), http.StatusUnauthorized, "INVALID_EMAIL_OR_PASSWORD")
	wantStatus(test, harness.signIn(email, "hunter2hunter2"), http.StatusOK)

	var stored string
	if err := testPool.QueryRow(ctx, `SELECT "password" FROM "account" WHERE "userId" = $1`, id).Scan(&stored); err != nil {
		test.Fatal(err)
	}
	if !strings.HasPrefix(stored, "$argon2id$") {
		test.Fatalf("hash not upgraded: %q", stored)
	}
	wantStatus(test, harness.signIn(email, "hunter2hunter2"), http.StatusOK)
}

func TestSquattedSignUpIsReclaimedByEmailOwner(test *testing.T) {
	test.Parallel()
	harness := newHarness(test, func(config *Config) { config.RequireEmailVerification = true })
	victim := uniqueEmail(test)
	link := func(index int) string { return pathOf(test, harness.mails()[index].url) }

	// An attacker registers the victim's address first; the link goes to the victim.
	wantStatus(test, harness.signUp(victim, "attacker-pass"), http.StatusOK)
	attackerLink := link(0)
	// The victim signs up too: same generic response, their password wins,
	// and they get a fresh link.
	wantStatus(test, harness.signUp(victim, "victim-pass"), http.StatusOK)
	if count := len(harness.mails()); count != 2 {
		test.Fatalf("mails = %d, want 2", count)
	}
	// The link issued for the attacker's password is void.
	if rec := harness.do("GET", attackerLink, nil, nil); rec.Header().Get("Location") != "/?error=INVALID_TOKEN" || sessionCookie(rec) != nil {
		test.Fatalf("attacker link: %d %q", rec.Code, rec.Header().Get("Location"))
	}
	victimLink := ""
	for _, testCase := range harness.mails()[1:] {
		victimLink = pathOf(test, testCase.url)
	}
	rec := harness.do("GET", victimLink, nil, nil)
	wantStatus(test, rec, http.StatusFound)
	if sessionCookie(rec) == nil {
		test.Fatal("verification did not sign the owner in")
	}
	wantCode(test, harness.signIn(victim, "attacker-pass"), http.StatusUnauthorized, "INVALID_EMAIL_OR_PASSWORD")
	wantStatus(test, harness.signIn(victim, "victim-pass"), http.StatusOK)

	// Once verified, a sign-up with the address changes nothing.
	wantStatus(test, harness.signUp(victim, "attacker-again"), http.StatusOK)
	wantCode(test, harness.signIn(victim, "attacker-again"), http.StatusUnauthorized, "INVALID_EMAIL_OR_PASSWORD")
	if count := len(harness.mails()); count != 2 {
		test.Fatalf("verified duplicate sent mail (%d total)", count)
	}
}

func TestResetRevokesOtherResetLinks(test *testing.T) {
	test.Parallel()
	harness := newHarness(test, nil)
	email := uniqueEmail(test)
	wantStatus(test, harness.signUp(email, "old-password"), http.StatusOK)
	ctx := context.Background()
	for range 2 {
		if err := harness.auth.RequestPasswordReset(ctx, email, "/r"); err != nil {
			test.Fatal(err)
		}
	}
	var tokens []string
	for _, mail := range harness.mails() {
		tokens = append(tokens, mustQuery(test, mail.url, "token"))
	}
	if err := harness.auth.ResetPassword(ctx, tokens[0], "new-password"); err != nil {
		test.Fatal(err)
	}
	if err := harness.auth.ResetPassword(ctx, tokens[1], "attacker-password"); !errors.Is(err, ErrInvalidToken) {
		test.Fatalf("second outstanding link still works: %v", err)
	}
	if err := harness.auth.RequestPasswordReset(ctx, email, ""); !errors.Is(err, ErrInvalidRedirectURL) {
		test.Fatalf("empty redirectTo err = %v", err)
	}
}

func TestSignOutWithBearerAndCookieRevokesBoth(test *testing.T) {
	test.Parallel()
	harness := newHarness(test, nil)
	email := uniqueEmail(test)
	cookie := cookieHeader(test, harness.signUp(email, "password123"))
	token := jsonBody(test, harness.signIn(email, "password123"))["token"].(string)
	rec := harness.do("POST", BasePath+"/sign-out", nil, hdr{"Authorization": "Bearer " + token, "Cookie": cookie, "Origin": testBase})
	wantStatus(test, rec, http.StatusOK)
	if harness.getSession(hdr{"Cookie": cookie}) != nil || harness.getSession(hdr{"Authorization": "Bearer " + token}) != nil {
		test.Fatal("a session survived sign-out")
	}
}
