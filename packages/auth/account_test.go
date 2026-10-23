package auth

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// session signs email in and returns its bearer header and session id.
func (harness *harness) session(email, password string) (hdr, string) {
	harness.test.Helper()
	rec := harness.signIn(email, password)
	wantStatus(harness.test, rec, http.StatusOK)
	bearer := hdr{"Authorization": "Bearer " + jsonBody(harness.test, rec)["token"].(string)}
	got := harness.getSession(bearer)
	return bearer, got["session"].(map[string]any)["id"].(string)
}

func (harness *harness) userID(headers hdr) string {
	harness.test.Helper()
	return harness.getSession(headers)["user"].(map[string]any)["id"].(string)
}

func TestChangePassword(test *testing.T) {
	test.Parallel()
	harness := newHarness(test, nil)
	email := uniqueEmail(test)
	cookie := hdr{"Cookie": cookieHeader(test, harness.signUp(email, "old-password")), "Origin": testBase}
	other, _ := harness.session(email, "old-password")
	change := func(headers hdr, body map[string]any) *httpResult {
		rec := harness.do("POST", BasePath+"/change-password", body, headers)
		return &httpResult{rec.Code, rec.Body.String()}
	}
	body := func(current, next string, revoke bool) map[string]any {
		return map[string]any{"currentPassword": current, "newPassword": next, "revokeOtherSessions": revoke}
	}

	for name, testCase := range map[string]struct {
		headers hdr
		body    map[string]any
		code    int
		errCode string
	}{
		"no session":       {nil, body("old-password", "new-password", false), 401, "UNAUTHORIZED"},
		"untrusted origin": {hdr{"Cookie": cookie["Cookie"], "Origin": "https://evil.test"}, body("old-password", "new-password", false), 403, "INVALID_ORIGIN"},
		"wrong current":    {cookie, body("wrong-password", "new-password", false), 400, "INVALID_PASSWORD"},
		"too short":        {cookie, body("old-password", "short", false), 400, "PASSWORD_TOO_SHORT"},
	} {
		if result := change(testCase.headers, testCase.body); result.code != testCase.code || !strings.Contains(result.body, testCase.errCode) {
			test.Errorf("%s: %d %s", name, result.code, result.body)
		}
	}
	wantStatus(test, harness.signIn(email, "old-password"), http.StatusOK) // nothing changed yet

	// An outstanding reset link dies with the password change.
	if err := harness.auth.RequestPasswordReset(context.Background(), email, "/r"); err != nil {
		test.Fatal(err)
	}
	resetToken := mustQuery(test, harness.mails()[0].url, "token")

	if result := change(cookie, body("old-password", "new-password", false)); result.code != 200 || !strings.Contains(result.body, `"token":null`) {
		test.Fatalf("change: %+v", result)
	}
	wantCode(test, harness.signIn(email, "old-password"), http.StatusUnauthorized, "INVALID_EMAIL_OR_PASSWORD")
	wantStatus(test, harness.signIn(email, "new-password"), http.StatusOK)
	if harness.getSession(other) == nil || harness.getSession(cookie) == nil {
		test.Fatal("sessions revoked without revokeOtherSessions")
	}
	if err := harness.auth.ResetPassword(context.Background(), resetToken, "reset-password"); !errors.Is(err, ErrInvalidToken) {
		test.Fatalf("reset link survived the password change: %v", err)
	}

	// revokeOtherSessions keeps only the session that made the change.
	if result := change(cookie, body("new-password", "newer-password", true)); result.code != 200 {
		test.Fatalf("change with revoke: %+v", result)
	}
	if harness.getSession(cookie) == nil {
		test.Fatal("current session revoked")
	}
	if harness.getSession(other) != nil {
		test.Fatal("other session survived revokeOtherSessions")
	}
}

func TestChangePasswordWithoutCredentialAccount(test *testing.T) {
	test.Parallel()
	harness := newHarness(test, nil)
	ctx := context.Background()
	id := generateID(32)
	if _, err := testPool.Exec(ctx, `INSERT INTO "user" ("id","name","email") VALUES ($1,'OAuth',$2)`, id, uniqueEmail(test)); err != nil {
		test.Fatal(err)
	}
	if err := harness.auth.ChangePassword(ctx, id, "anything-at-all", "new-password", false, ""); !errors.Is(err, ErrCredentialAccountNotFound) {
		test.Fatalf("err = %v", err)
	}
	if err := harness.auth.ChangePassword(ctx, id, strings.Repeat("x", 129), "new-password", false, ""); !errors.Is(err, ErrInvalidPassword) {
		test.Fatalf("over-long current password err = %v", err)
	}
}

func TestChangeEmail(test *testing.T) {
	test.Parallel()
	harness := newHarness(test, nil)
	email, taken, next := uniqueEmail(test), uniqueEmail(test), uniqueEmail(test)
	cookie := hdr{"Cookie": cookieHeader(test, harness.signUp(email, "password123")), "Origin": testBase}
	wantStatus(test, harness.signUp(taken, "password123"), http.StatusOK)
	change := func(headers hdr, newEmail, callback string) *httpResult {
		rec := harness.do("POST", BasePath+"/change-email", map[string]any{"newEmail": newEmail, "callbackURL": callback}, headers)
		return &httpResult{rec.Code, rec.Body.String()}
	}

	for name, testCase := range map[string]struct {
		headers  hdr
		newEmail string
		callback string
		code     int
		errCode  string
	}{
		"no session":         {nil, next, "", 401, "UNAUTHORIZED"},
		"email taken":        {cookie, strings.ToUpper(taken), "", 422, "USER_ALREADY_EXISTS_USE_ANOTHER_EMAIL"},
		"same email":         {cookie, email, "", 400, "EMAIL_IS_THE_SAME"},
		"invalid email":      {cookie, "not-an-email", "", 400, "INVALID_EMAIL"},
		"untrusted callback": {cookie, next, "https://evil.test/", 403, "INVALID_CALLBACK_URL"},
	} {
		if result := change(testCase.headers, testCase.newEmail, testCase.callback); result.code != testCase.code || !strings.Contains(result.body, testCase.errCode) {
			test.Errorf("%s: %d %s", name, result.code, result.body)
		}
	}
	if count := len(harness.mails()); count != 0 {
		test.Fatalf("rejected requests sent %d mails", count)
	}
	if err := harness.auth.RequestPasswordReset(context.Background(), email, "/r"); err != nil {
		test.Fatal(err)
	}
	resetLink := harness.mails()[0].url

	if result := change(cookie, strings.ToUpper(next), "/dashboard"); result.code != 200 || !strings.Contains(result.body, `"status":true`) {
		test.Fatalf("change-email: %+v", result)
	}
	mails := harness.mails()[1:]
	link := mails[slices.IndexFunc(mails, func(mail sentMail) bool { return mail.kind == "verification" })]
	notice := mails[slices.IndexFunc(mails, func(mail sentMail) bool { return mail.kind == "change-notice" })]
	if len(mails) != 2 || link.to != next || notice.to != email || notice.url != next {
		test.Fatalf("mails = %+v", mails)
	}
	if !strings.HasPrefix(link.url, testBase+"/api/auth/verify-email?token=") || !strings.Contains(link.url, "callbackURL=%2Fdashboard") {
		test.Fatalf("link = %s", link.url)
	}

	// Nothing changes until the link is opened.
	if got := harness.getSession(cookie)["user"].(map[string]any)["email"]; got != email {
		test.Fatalf("email changed before confirmation: %v", got)
	}
	wantCode(test, harness.signIn(next, "password123"), http.StatusUnauthorized, "INVALID_EMAIL_OR_PASSWORD")

	// Opening the link switches the address, verifies it and signs in.
	rec := harness.do("GET", pathOf(test, link.url), nil, nil)
	wantStatus(test, rec, http.StatusFound)
	if loc := rec.Header().Get("Location"); loc != "/dashboard" {
		test.Fatalf("redirect = %q", loc)
	}
	user := harness.getSession(hdr{"Cookie": cookieHeader(test, rec)})["user"].(map[string]any)
	if user["email"] != next || user["emailVerified"] != true {
		test.Fatalf("after confirmation user = %v", user)
	}
	if got := harness.getSession(cookie)["user"].(map[string]any)["email"]; got != next {
		test.Fatalf("existing session sees %v", got)
	}
	wantStatus(test, harness.signIn(next, "password123"), http.StatusOK)
	wantCode(test, harness.signIn(email, "password123"), http.StatusUnauthorized, "INVALID_EMAIL_OR_PASSWORD")

	// Single use, and reset links sent to the old address are void.
	rec = harness.do("GET", pathOf(test, link.url), nil, nil)
	if loc := rec.Header().Get("Location"); loc != "/dashboard?error=INVALID_TOKEN" || sessionCookie(rec) != nil {
		test.Fatalf("reused link: %d %q", rec.Code, loc)
	}
	if err := harness.auth.ResetPassword(context.Background(), mustQuery(test, resetLink, "token"), "reset-password"); !errors.Is(err, ErrInvalidToken) {
		test.Fatalf("old reset link err = %v", err)
	}
}

func TestChangeEmailLinkExpiresAndRechecksTheAddress(test *testing.T) {
	test.Parallel()
	harness := newHarness(test, nil)
	ctx := context.Background()
	first, second := uniqueEmail(test), uniqueEmail(test)
	wantStatus(test, harness.signUp(first, "password123"), http.StatusOK)
	wantStatus(test, harness.signUp(second, "password123"), http.StatusOK)
	firstID, secondID := harness.userID(hdr{"Cookie": harness.loginCookie(first)}), harness.userID(hdr{"Cookie": harness.loginCookie(second)})

	claimed, expiring := uniqueEmail(test), uniqueEmail(test)
	if err := harness.auth.ChangeEmail(ctx, firstID, claimed, ""); err != nil {
		test.Fatal(err)
	}
	if err := harness.auth.ChangeEmail(ctx, secondID, expiring, ""); err != nil {
		test.Fatal(err)
	}
	links := harness.changeLinks()

	// Someone registers the target address before the link is opened.
	wantStatus(test, harness.signUp(claimed, "password123"), http.StatusOK)
	if _, _, err := harness.auth.VerifyEmail(ctx, links[claimed]); !errors.Is(err, ErrEmailTaken) {
		test.Fatalf("claimed address err = %v", err)
	}

	harness.clock.Advance(VerificationTokenTTL + time.Minute)
	if _, _, err := harness.auth.VerifyEmail(ctx, links[expiring]); !errors.Is(err, ErrTokenExpired) {
		test.Fatalf("expired link err = %v", err)
	}
	if err := harness.auth.ChangeEmail(ctx, generateID(32), uniqueEmail(test), ""); !errors.Is(err, ErrUserNotFound) {
		test.Fatalf("unknown user err = %v", err)
	}
}

// loginCookie signs email in and returns its Cookie header value.
func (harness *harness) loginCookie(email string) string {
	harness.test.Helper()
	return cookieHeader(harness.test, harness.signIn(email, "password123"))
}

// changeLinks maps each new address to the token of its change-email link.
func (harness *harness) changeLinks() map[string]string {
	harness.test.Helper()
	links := map[string]string{}
	for _, mail := range harness.mails() {
		if mail.kind == "verification" && strings.Contains(mail.url, "token=") {
			links[mail.to] = mustQuery(harness.test, mail.url, "token")
		}
	}
	return links
}

func TestChangeEmailLinksAreBoundToTheirUserAndRequest(test *testing.T) {
	test.Parallel()
	harness := newHarness(test, nil)
	ctx := context.Background()
	left, right, code := uniqueEmail(test), uniqueEmail(test), uniqueEmail(test)
	wantStatus(test, harness.signUp(left, "password123"), http.StatusOK)
	id := harness.userID(hdr{"Cookie": harness.loginCookie(left)})

	// Attacker X keeps an A→B link, moves to C, and A is recycled by victim Y.
	if err := harness.auth.ChangeEmail(ctx, id, right, ""); err != nil {
		test.Fatal(err)
	}
	stale := harness.changeLinks()[right]
	if err := harness.auth.ChangeEmail(ctx, id, code, ""); err != nil {
		test.Fatal(err)
	}
	if _, _, err := harness.auth.VerifyEmail(ctx, stale); !errors.Is(err, ErrInvalidToken) {
		test.Fatalf("superseded link err = %v", err)
	}
	if _, _, err := harness.auth.VerifyEmail(ctx, harness.changeLinks()[code]); err != nil {
		test.Fatal(err)
	}
	wantStatus(test, harness.signUp(left, "password123"), http.StatusOK)
	victim := harness.userID(hdr{"Cookie": harness.loginCookie(left)})
	if user, _, err := harness.auth.VerifyEmail(ctx, stale); !errors.Is(err, ErrInvalidToken) {
		test.Fatalf("stale link hit %s (victim %s): %v", user.ID, victim, err)
	}

	// A→B→A: the first link cannot be replayed after moving back.
	if err := harness.auth.ChangeEmail(ctx, victim, right, ""); err != nil {
		test.Fatal(err)
	}
	toB := harness.changeLinks()[right]
	if _, _, err := harness.auth.VerifyEmail(ctx, toB); err != nil {
		test.Fatal(err)
	}
	if err := harness.auth.ChangeEmail(ctx, victim, left, ""); err != nil {
		test.Fatal(err)
	}
	if _, _, err := harness.auth.VerifyEmail(ctx, harness.changeLinks()[left]); err != nil {
		test.Fatal(err)
	}
	if _, _, err := harness.auth.VerifyEmail(ctx, toB); !errors.Is(err, ErrInvalidToken) {
		test.Fatalf("replayed A→B link err = %v", err)
	}

	// A link opened in a browser signed in as someone else does not switch accounts.
	if err := harness.auth.ChangeEmail(ctx, victim, right, "/done"); err != nil {
		test.Fatal(err)
	}
	link := harness.changeLinks()[right]
	stranger := cookieHeader(test, harness.signUp(uniqueEmail(test), "password123"))
	rec := harness.do("GET", BasePath+"/verify-email?callbackURL=/done&token="+link, nil, hdr{"Cookie": stranger})
	if loc := rec.Header().Get("Location"); loc != "/done?error=INVALID_USER" || sessionCookie(rec) != nil {
		test.Fatalf("other user's browser: %d %q", rec.Code, loc)
	}
	// The JSON form returns the updated user, as Better Auth does.
	rec = harness.do("GET", BasePath+"/verify-email?token="+link, nil, nil)
	wantStatus(test, rec, http.StatusOK)
	if lookup := jsonBody(test, rec)["user"].(map[string]any); lookup["email"] != right || lookup["emailVerified"] != true {
		test.Fatalf("user = %v", lookup)
	}
}

func TestPendingEmailChangeIsCancelledByRecovery(test *testing.T) {
	test.Parallel()
	ctx := context.Background()
	// Each recovery fails the subtest that runs it (h.t), not the parent.
	for name, recover := range map[string]func(fixture *harness, userID, email string){
		"change password": func(harness *harness, userID, _ string) {
			if err := harness.auth.ChangePassword(ctx, userID, "password123", "new-password", false, ""); err != nil {
				harness.test.Fatal(err)
			}
		},
		"revoke other sessions": func(harness *harness, userID, _ string) {
			if err := harness.auth.RevokeOtherSessions(ctx, userID, ""); err != nil {
				harness.test.Fatal(err)
			}
		},
		"password reset": func(harness *harness, _, email string) {
			if err := harness.auth.RequestPasswordReset(ctx, email, "/r"); err != nil {
				harness.test.Fatal(err)
			}
			var link string
			for _, mail := range harness.mails() {
				if mail.kind == "reset" {
					link = mail.url
				}
			}
			if err := harness.auth.ResetPassword(ctx, mustQuery(harness.test, link, "token"), "new-password"); err != nil {
				harness.test.Fatal(err)
			}
		},
	} {
		test.Run(name, func(test *testing.T) {
			test.Parallel()
			harness := newHarness(test, nil)
			email, attacker := uniqueEmail(test), uniqueEmail(test)
			wantStatus(test, harness.signUp(email, "password123"), http.StatusOK)
			id := harness.userID(hdr{"Cookie": harness.loginCookie(email)})
			if err := harness.auth.ChangeEmail(ctx, id, attacker, ""); err != nil {
				test.Fatal(err)
			}
			link := harness.changeLinks()[attacker]
			recover(harness, id, email)
			if _, _, err := harness.auth.VerifyEmail(ctx, link); !errors.Is(err, ErrInvalidToken) {
				test.Fatalf("pending change survived: %v", err)
			}
		})
	}
}

func TestChangeEmailNeedsAFreshSession(test *testing.T) {
	test.Parallel()
	harness := newHarness(test, nil)
	email := uniqueEmail(test)
	cookie := hdr{"Cookie": cookieHeader(test, harness.signUp(email, "password123")), "Origin": testBase}
	harness.clock.Advance(DefaultFreshAge + time.Minute)
	rec := harness.do("POST", BasePath+"/change-email", map[string]any{"newEmail": uniqueEmail(test)}, cookie)
	wantCode(test, rec, http.StatusForbidden, "SESSION_NOT_FRESH")
	// Signing in again gives a fresh session.
	fresh := hdr{"Cookie": harness.loginCookie(email), "Origin": testBase}
	wantStatus(test, harness.do("POST", BasePath+"/change-email", map[string]any{"newEmail": uniqueEmail(test)}, fresh), http.StatusOK)
}

func TestChangePasswordConcurrencyAndPerUserLimit(test *testing.T) {
	test.Parallel()
	harness := newHarness(test, nil)
	ctx := context.Background()
	email := uniqueEmail(test)
	wantStatus(test, harness.signUp(email, "password123"), http.StatusOK)
	id := harness.userID(hdr{"Cookie": harness.loginCookie(email)})

	var wg sync.WaitGroup
	results := make([]error, 6)
	for index := range results {
		wg.Go(func() {
			results[index] = harness.auth.ChangePassword(ctx, id, "password123", "new-password-"+string(rune('a'+index)), true, "")
		})
	}
	wg.Wait()
	won := 0
	for _, err := range results {
		if err == nil {
			won++
		} else if !errors.Is(err, ErrInvalidPassword) {
			test.Fatalf("unexpected error: %v", err)
		}
	}
	if won != 1 {
		test.Fatalf("%d concurrent password changes succeeded, want 1", won)
	}

	// The per-user budget holds even when every attempt comes from a new IP.
	limited := newHarness(test, func(config *Config) { config.RateLimiter = NewMemoryRateLimiter(); config.ClientIPHeader = "X-Real-IP" })
	victim := uniqueEmail(test)
	cookie := cookieHeader(test, limited.signUp(victim, "password123"))
	codes := []int{}
	for index := range 4 {
		rec := limited.do("POST", BasePath+"/change-password", map[string]any{"currentPassword": "guess-guess", "newPassword": "new-password"},
			hdr{"Cookie": cookie, "Origin": testBase, "X-Real-IP": "203.0.113." + string(rune('1'+index))})
		codes = append(codes, rec.Code)
	}
	if !slices.Equal(codes, []int{400, 400, 400, 429}) {
		test.Fatalf("codes = %v, want three guesses then 429", codes)
	}
}

func TestChangeEmailHidesTakenAddressWhenVerificationRequired(test *testing.T) {
	test.Parallel()
	harness := newHarness(test, func(config *Config) { config.RequireEmailVerification = true })
	ctx := context.Background()
	owner, taken := uniqueEmail(test), uniqueEmail(test)
	for _, email := range []string{owner, taken} {
		wantStatus(test, harness.signUp(email, "password123"), http.StatusOK)
		rec := harness.do("GET", pathOf(test, harness.mails()[len(harness.mails())-1].url), nil, nil)
		wantStatus(test, rec, http.StatusFound)
	}
	sent := len(harness.mails())
	user, _, err := harness.auth.SignInEmail(ctx, SignInInput{Email: owner, Password: "password123"})
	if err != nil {
		test.Fatal(err)
	}
	if err := harness.auth.ChangeEmail(ctx, user.ID, taken, ""); err != nil {
		test.Fatalf("taken address revealed: %v", err)
	}
	if count := len(harness.mails()); count != sent {
		test.Fatalf("taken address got mail (%d new)", count-sent)
	}
}

func TestSessionsListAndRevoke(test *testing.T) {
	test.Parallel()
	harness := newHarness(test, nil)
	ctx := context.Background()
	email, otherEmail := uniqueEmail(test), uniqueEmail(test)
	rec := harness.signUp(email, "password123")
	cookie := hdr{"Cookie": cookieHeader(test, rec), "Origin": testBase}
	cookieID := harness.getSession(cookie)["session"].(map[string]any)["id"].(string)
	sessionB, bID := harness.session(email, "password123")
	sessionC, cID := harness.session(email, "password123")
	wantStatus(test, harness.signUp(otherEmail, "password123"), http.StatusOK)
	stranger, strangerID := harness.session(otherEmail, "password123")
	userID := harness.userID(cookie)

	// list-sessions: own sessions only, newest first, never a token.
	wantCode(test, harness.do("GET", BasePath+"/list-sessions", nil, nil), http.StatusUnauthorized, "UNAUTHORIZED")
	rec = harness.do("GET", BasePath+"/list-sessions", nil, sessionC)
	wantStatus(test, rec, http.StatusOK)
	if strings.Contains(rec.Body.String(), `"token"`) {
		test.Fatalf("list-sessions exposes tokens: %s", rec.Body)
	}
	sessions, err := harness.auth.ListSessions(ctx, userID)
	if err != nil {
		test.Fatal(err)
	}
	var ids []string
	for _, session := range sessions {
		if session.Token != "" || session.UserID != userID {
			test.Fatalf("session %+v", session)
		}
		ids = append(ids, session.ID)
	}
	if len(ids) != 3 || !slices.Contains(ids, cookieID) || !slices.Contains(ids, bID) || !slices.Contains(ids, cID) || slices.Contains(ids, strangerID) {
		test.Fatalf("ids = %v", ids)
	}
	if !sessions[0].CreatedAt.After(sessions[2].CreatedAt) && !sessions[0].CreatedAt.Equal(sessions[2].CreatedAt) {
		test.Fatalf("not newest first: %v", sessions)
	}

	revoke := func(headers hdr, body map[string]any) *httpResult {
		rec := harness.do("POST", BasePath+"/revoke-session", body, headers)
		return &httpResult{rec.Code, rec.Body.String()}
	}
	// Someone else's session cannot be revoked, by id or by token.
	if result := revoke(sessionC, map[string]any{"id": strangerID}); result.code != 404 || !strings.Contains(result.body, "SESSION_NOT_FOUND") {
		test.Fatalf("revoke stranger: %+v", result)
	}
	if result := revoke(sessionC, map[string]any{"token": strings.TrimPrefix(stranger["Authorization"], "Bearer ")}); result.code != 404 {
		test.Fatalf("revoke stranger by token: %+v", result)
	}
	if err := harness.auth.RevokeSession(ctx, userID, strangerID); !errors.Is(err, ErrSessionNotFound) {
		test.Fatalf("service revoke stranger err = %v", err)
	}
	if result := revoke(sessionC, map[string]any{}); result.code != 404 {
		test.Fatalf("revoke nothing: %+v", result)
	}
	if harness.getSession(stranger) == nil {
		test.Fatal("stranger's session was revoked")
	}
	if result := revoke(hdr{"Cookie": cookie["Cookie"], "Origin": "https://evil.test"}, map[string]any{"id": bID}); result.code != 403 {
		test.Fatalf("cross-origin revoke: %+v", result)
	}

	// Revoking one of my sessions signs only that one out.
	if result := revoke(sessionC, map[string]any{"id": bID}); result.code != 200 {
		test.Fatalf("revoke b: %+v", result)
	}
	if harness.getSession(sessionB) != nil || harness.getSession(sessionC) == nil || harness.getSession(cookie) == nil {
		test.Fatal("wrong sessions after revoking b")
	}
	// Revoking the current cookie session also clears the cookie.
	rec = harness.do("POST", BasePath+"/revoke-session", map[string]any{"id": cookieID}, cookie)
	wantStatus(test, rec, http.StatusOK)
	if ck := sessionCookie(rec); ck == nil || ck.MaxAge >= 0 {
		test.Fatal("cookie not cleared when revoking the current session")
	}
	if harness.getSession(cookie) != nil {
		test.Fatal("current session survived")
	}

	// revoke-other-sessions keeps the caller's session only.
	sessionD, _ := harness.session(email, "password123")
	sessionE, _ := harness.session(email, "password123")
	wantStatus(test, harness.do("POST", BasePath+"/revoke-other-sessions", nil, sessionC), http.StatusOK)
	if harness.getSession(sessionC) == nil || harness.getSession(sessionD) != nil || harness.getSession(sessionE) != nil || harness.getSession(stranger) == nil {
		test.Fatal("revoke-other-sessions revoked the wrong sessions")
	}
	if sessions, _ := harness.auth.ListSessions(ctx, userID); len(sessions) != 1 || sessions[0].ID != cID {
		test.Fatalf("remaining = %+v", sessions)
	}
	wantCode(test, harness.do("POST", BasePath+"/revoke-other-sessions", nil, nil), http.StatusUnauthorized, "UNAUTHORIZED")

	// Expired sessions are not listed.
	harness.clock.Advance(DefaultSessionTTL + time.Minute)
	if sessions, _ := harness.auth.ListSessions(ctx, userID); len(sessions) != 0 {
		test.Fatalf("expired sessions listed: %+v", sessions)
	}
}
