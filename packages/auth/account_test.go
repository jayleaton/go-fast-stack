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
func (h *harness) session(email, password string) (hdr, string) {
	h.t.Helper()
	rec := h.signIn(email, password)
	wantStatus(h.t, rec, http.StatusOK)
	bearer := hdr{"Authorization": "Bearer " + jsonBody(h.t, rec)["token"].(string)}
	got := h.getSession(bearer)
	return bearer, got["session"].(map[string]any)["id"].(string)
}

func (h *harness) userID(headers hdr) string {
	h.t.Helper()
	return h.getSession(headers)["user"].(map[string]any)["id"].(string)
}

func TestChangePassword(t *testing.T) {
	h := newHarness(t, nil)
	email := uniqueEmail(t)
	cookie := hdr{"Cookie": cookieHeader(t, h.signUp(email, "old-password")), "Origin": testBase}
	other, _ := h.session(email, "old-password")
	change := func(headers hdr, body map[string]any) *httpResult {
		rec := h.do("POST", BasePath+"/change-password", body, headers)
		return &httpResult{rec.Code, rec.Body.String()}
	}
	body := func(current, next string, revoke bool) map[string]any {
		return map[string]any{"currentPassword": current, "newPassword": next, "revokeOtherSessions": revoke}
	}

	for name, c := range map[string]struct {
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
		if r := change(c.headers, c.body); r.code != c.code || !strings.Contains(r.body, c.errCode) {
			t.Errorf("%s: %d %s", name, r.code, r.body)
		}
	}
	wantStatus(t, h.signIn(email, "old-password"), http.StatusOK) // nothing changed yet

	// An outstanding reset link dies with the password change.
	if err := h.a.RequestPasswordReset(context.Background(), email, "/r"); err != nil {
		t.Fatal(err)
	}
	resetToken := mustQuery(t, h.do("GET", pathOf(t, h.mails()[0].url), nil, nil).Header().Get("Location"), "token")

	if r := change(cookie, body("old-password", "new-password", false)); r.code != 200 || !strings.Contains(r.body, `"token":null`) {
		t.Fatalf("change: %+v", r)
	}
	wantCode(t, h.signIn(email, "old-password"), http.StatusUnauthorized, "INVALID_EMAIL_OR_PASSWORD")
	wantStatus(t, h.signIn(email, "new-password"), http.StatusOK)
	if h.getSession(other) == nil || h.getSession(cookie) == nil {
		t.Fatal("sessions revoked without revokeOtherSessions")
	}
	if err := h.a.ResetPassword(context.Background(), resetToken, "reset-password"); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("reset link survived the password change: %v", err)
	}

	// revokeOtherSessions keeps only the session that made the change.
	if r := change(cookie, body("new-password", "newer-password", true)); r.code != 200 {
		t.Fatalf("change with revoke: %+v", r)
	}
	if h.getSession(cookie) == nil {
		t.Fatal("current session revoked")
	}
	if h.getSession(other) != nil {
		t.Fatal("other session survived revokeOtherSessions")
	}
}

func TestChangePasswordWithoutCredentialAccount(t *testing.T) {
	h := newHarness(t, nil)
	ctx := context.Background()
	id := generateID(32)
	if _, err := testPool.Exec(ctx, `INSERT INTO "user" ("id","name","email") VALUES ($1,'OAuth',$2)`, id, uniqueEmail(t)); err != nil {
		t.Fatal(err)
	}
	if err := h.a.ChangePassword(ctx, id, "anything-at-all", "new-password", false, ""); !errors.Is(err, ErrCredentialAccountNotFound) {
		t.Fatalf("err = %v", err)
	}
	if err := h.a.ChangePassword(ctx, id, strings.Repeat("x", 129), "new-password", false, ""); !errors.Is(err, ErrInvalidPassword) {
		t.Fatalf("over-long current password err = %v", err)
	}
}

func TestChangeEmail(t *testing.T) {
	h := newHarness(t, nil)
	email, taken, next := uniqueEmail(t), uniqueEmail(t), uniqueEmail(t)
	cookie := hdr{"Cookie": cookieHeader(t, h.signUp(email, "password123")), "Origin": testBase}
	wantStatus(t, h.signUp(taken, "password123"), http.StatusOK)
	change := func(headers hdr, newEmail, callback string) *httpResult {
		rec := h.do("POST", BasePath+"/change-email", map[string]any{"newEmail": newEmail, "callbackURL": callback}, headers)
		return &httpResult{rec.Code, rec.Body.String()}
	}

	for name, c := range map[string]struct {
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
		if r := change(c.headers, c.newEmail, c.callback); r.code != c.code || !strings.Contains(r.body, c.errCode) {
			t.Errorf("%s: %d %s", name, r.code, r.body)
		}
	}
	if n := len(h.mails()); n != 0 {
		t.Fatalf("rejected requests sent %d mails", n)
	}
	if err := h.a.RequestPasswordReset(context.Background(), email, "/r"); err != nil {
		t.Fatal(err)
	}
	resetLink := h.mails()[0].url

	if r := change(cookie, strings.ToUpper(next), "/dashboard"); r.code != 200 || !strings.Contains(r.body, `"status":true`) {
		t.Fatalf("change-email: %+v", r)
	}
	mails := h.mails()[1:]
	link := mails[slices.IndexFunc(mails, func(m sentMail) bool { return m.kind == "verification" })]
	notice := mails[slices.IndexFunc(mails, func(m sentMail) bool { return m.kind == "change-notice" })]
	if len(mails) != 2 || link.to != next || notice.to != email || notice.url != next {
		t.Fatalf("mails = %+v", mails)
	}
	if !strings.HasPrefix(link.url, testBase+"/api/auth/verify-email?token=") || !strings.Contains(link.url, "callbackURL=%2Fdashboard") {
		t.Fatalf("link = %s", link.url)
	}

	// Nothing changes until the link is opened.
	if got := h.getSession(cookie)["user"].(map[string]any)["email"]; got != email {
		t.Fatalf("email changed before confirmation: %v", got)
	}
	wantCode(t, h.signIn(next, "password123"), http.StatusUnauthorized, "INVALID_EMAIL_OR_PASSWORD")

	// Opening the link switches the address, verifies it and signs in.
	rec := h.do("GET", pathOf(t, link.url), nil, nil)
	wantStatus(t, rec, http.StatusFound)
	if loc := rec.Header().Get("Location"); loc != "/dashboard" {
		t.Fatalf("redirect = %q", loc)
	}
	user := h.getSession(hdr{"Cookie": cookieHeader(t, rec)})["user"].(map[string]any)
	if user["email"] != next || user["emailVerified"] != true {
		t.Fatalf("after confirmation user = %v", user)
	}
	if got := h.getSession(cookie)["user"].(map[string]any)["email"]; got != next {
		t.Fatalf("existing session sees %v", got)
	}
	wantStatus(t, h.signIn(next, "password123"), http.StatusOK)
	wantCode(t, h.signIn(email, "password123"), http.StatusUnauthorized, "INVALID_EMAIL_OR_PASSWORD")

	// Single use, and reset links sent to the old address are void.
	rec = h.do("GET", pathOf(t, link.url), nil, nil)
	if loc := rec.Header().Get("Location"); loc != "/dashboard?error=INVALID_TOKEN" || sessionCookie(rec) != nil {
		t.Fatalf("reused link: %d %q", rec.Code, loc)
	}
	if loc := h.do("GET", pathOf(t, resetLink), nil, nil).Header().Get("Location"); loc != "/r?error=INVALID_TOKEN" {
		t.Fatalf("old reset link redirect = %q", loc)
	}
}

func TestChangeEmailLinkExpiresAndRechecksTheAddress(t *testing.T) {
	h := newHarness(t, nil)
	ctx := context.Background()
	first, second := uniqueEmail(t), uniqueEmail(t)
	wantStatus(t, h.signUp(first, "password123"), http.StatusOK)
	wantStatus(t, h.signUp(second, "password123"), http.StatusOK)
	firstID, secondID := h.userID(hdr{"Cookie": h.loginCookie(first)}), h.userID(hdr{"Cookie": h.loginCookie(second)})

	claimed, expiring := uniqueEmail(t), uniqueEmail(t)
	if err := h.a.ChangeEmail(ctx, firstID, claimed, ""); err != nil {
		t.Fatal(err)
	}
	if err := h.a.ChangeEmail(ctx, secondID, expiring, ""); err != nil {
		t.Fatal(err)
	}
	links := h.changeLinks()

	// Someone registers the target address before the link is opened.
	wantStatus(t, h.signUp(claimed, "password123"), http.StatusOK)
	if _, _, err := h.a.VerifyEmail(ctx, links[claimed]); !errors.Is(err, ErrEmailTaken) {
		t.Fatalf("claimed address err = %v", err)
	}

	h.clock.Advance(VerificationTokenTTL + time.Minute)
	if _, _, err := h.a.VerifyEmail(ctx, links[expiring]); !errors.Is(err, ErrTokenExpired) {
		t.Fatalf("expired link err = %v", err)
	}
	if err := h.a.ChangeEmail(ctx, generateID(32), uniqueEmail(t), ""); !errors.Is(err, ErrUserNotFound) {
		t.Fatalf("unknown user err = %v", err)
	}
}

// loginCookie signs email in and returns its Cookie header value.
func (h *harness) loginCookie(email string) string {
	h.t.Helper()
	return cookieHeader(h.t, h.signIn(email, "password123"))
}

// changeLinks maps each new address to the token of its change-email link.
func (h *harness) changeLinks() map[string]string {
	h.t.Helper()
	links := map[string]string{}
	for _, m := range h.mails() {
		if m.kind == "verification" && strings.Contains(m.url, "token=") {
			links[m.to] = mustQuery(h.t, m.url, "token")
		}
	}
	return links
}

func TestChangeEmailLinksAreBoundToTheirUserAndRequest(t *testing.T) {
	h := newHarness(t, nil)
	ctx := context.Background()
	a, b, c := uniqueEmail(t), uniqueEmail(t), uniqueEmail(t)
	wantStatus(t, h.signUp(a, "password123"), http.StatusOK)
	x := h.userID(hdr{"Cookie": h.loginCookie(a)})

	// Attacker X keeps an A→B link, moves to C, and A is recycled by victim Y.
	if err := h.a.ChangeEmail(ctx, x, b, ""); err != nil {
		t.Fatal(err)
	}
	stale := h.changeLinks()[b]
	if err := h.a.ChangeEmail(ctx, x, c, ""); err != nil {
		t.Fatal(err)
	}
	if _, _, err := h.a.VerifyEmail(ctx, stale); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("superseded link err = %v", err)
	}
	if _, _, err := h.a.VerifyEmail(ctx, h.changeLinks()[c]); err != nil {
		t.Fatal(err)
	}
	wantStatus(t, h.signUp(a, "password123"), http.StatusOK)
	victim := h.userID(hdr{"Cookie": h.loginCookie(a)})
	if user, _, err := h.a.VerifyEmail(ctx, stale); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("stale link hit %s (victim %s): %v", user.ID, victim, err)
	}

	// A→B→A: the first link cannot be replayed after moving back.
	if err := h.a.ChangeEmail(ctx, victim, b, ""); err != nil {
		t.Fatal(err)
	}
	toB := h.changeLinks()[b]
	if _, _, err := h.a.VerifyEmail(ctx, toB); err != nil {
		t.Fatal(err)
	}
	if err := h.a.ChangeEmail(ctx, victim, a, ""); err != nil {
		t.Fatal(err)
	}
	if _, _, err := h.a.VerifyEmail(ctx, h.changeLinks()[a]); err != nil {
		t.Fatal(err)
	}
	if _, _, err := h.a.VerifyEmail(ctx, toB); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("replayed A→B link err = %v", err)
	}

	// A link opened in a browser signed in as someone else does not switch accounts.
	if err := h.a.ChangeEmail(ctx, victim, b, "/done"); err != nil {
		t.Fatal(err)
	}
	link := h.changeLinks()[b]
	stranger := cookieHeader(t, h.signUp(uniqueEmail(t), "password123"))
	rec := h.do("GET", BasePath+"/verify-email?callbackURL=/done&token="+link, nil, hdr{"Cookie": stranger})
	if loc := rec.Header().Get("Location"); loc != "/done?error=INVALID_USER" || sessionCookie(rec) != nil {
		t.Fatalf("other user's browser: %d %q", rec.Code, loc)
	}
	// The JSON form returns the updated user, as Better Auth does.
	rec = h.do("GET", BasePath+"/verify-email?token="+link, nil, nil)
	wantStatus(t, rec, http.StatusOK)
	if u := jsonBody(t, rec)["user"].(map[string]any); u["email"] != b || u["emailVerified"] != true {
		t.Fatalf("user = %v", u)
	}
}

func TestPendingEmailChangeIsCancelledByRecovery(t *testing.T) {
	ctx := context.Background()
	for name, recover := range map[string]func(h *harness, userID, email string){
		"change password": func(h *harness, userID, _ string) {
			if err := h.a.ChangePassword(ctx, userID, "password123", "new-password", false, ""); err != nil {
				t.Fatal(err)
			}
		},
		"revoke other sessions": func(h *harness, userID, _ string) {
			if err := h.a.RevokeOtherSessions(ctx, userID, ""); err != nil {
				t.Fatal(err)
			}
		},
		"password reset": func(h *harness, _, email string) {
			if err := h.a.RequestPasswordReset(ctx, email, "/r"); err != nil {
				t.Fatal(err)
			}
			var link string
			for _, m := range h.mails() {
				if m.kind == "reset" {
					link = m.url
				}
			}
			token := mustQuery(t, h.do("GET", pathOf(t, link), nil, nil).Header().Get("Location"), "token")
			if err := h.a.ResetPassword(ctx, token, "new-password"); err != nil {
				t.Fatal(err)
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t, nil)
			email, attacker := uniqueEmail(t), uniqueEmail(t)
			wantStatus(t, h.signUp(email, "password123"), http.StatusOK)
			id := h.userID(hdr{"Cookie": h.loginCookie(email)})
			if err := h.a.ChangeEmail(ctx, id, attacker, ""); err != nil {
				t.Fatal(err)
			}
			link := h.changeLinks()[attacker]
			recover(h, id, email)
			if _, _, err := h.a.VerifyEmail(ctx, link); !errors.Is(err, ErrInvalidToken) {
				t.Fatalf("pending change survived: %v", err)
			}
		})
	}
}

func TestChangeEmailNeedsAFreshSession(t *testing.T) {
	h := newHarness(t, nil)
	email := uniqueEmail(t)
	cookie := hdr{"Cookie": cookieHeader(t, h.signUp(email, "password123")), "Origin": testBase}
	h.clock.Advance(DefaultFreshAge + time.Minute)
	rec := h.do("POST", BasePath+"/change-email", map[string]any{"newEmail": uniqueEmail(t)}, cookie)
	wantCode(t, rec, http.StatusForbidden, "SESSION_NOT_FRESH")
	// Signing in again gives a fresh session.
	fresh := hdr{"Cookie": h.loginCookie(email), "Origin": testBase}
	wantStatus(t, h.do("POST", BasePath+"/change-email", map[string]any{"newEmail": uniqueEmail(t)}, fresh), http.StatusOK)
}

func TestChangePasswordConcurrencyAndPerUserLimit(t *testing.T) {
	h := newHarness(t, nil)
	ctx := context.Background()
	email := uniqueEmail(t)
	wantStatus(t, h.signUp(email, "password123"), http.StatusOK)
	id := h.userID(hdr{"Cookie": h.loginCookie(email)})

	var wg sync.WaitGroup
	results := make([]error, 6)
	for i := range results {
		wg.Go(func() {
			results[i] = h.a.ChangePassword(ctx, id, "password123", "new-password-"+string(rune('a'+i)), true, "")
		})
	}
	wg.Wait()
	won := 0
	for _, err := range results {
		if err == nil {
			won++
		} else if !errors.Is(err, ErrInvalidPassword) {
			t.Fatalf("unexpected error: %v", err)
		}
	}
	if won != 1 {
		t.Fatalf("%d concurrent password changes succeeded, want 1", won)
	}

	// The per-user budget holds even when every attempt comes from a new IP.
	limited := newHarness(t, func(c *Config) { c.RateLimiter = NewMemoryRateLimiter(); c.ClientIPHeader = "X-Real-IP" })
	victim := uniqueEmail(t)
	cookie := cookieHeader(t, limited.signUp(victim, "password123"))
	codes := []int{}
	for i := range 4 {
		rec := limited.do("POST", BasePath+"/change-password", map[string]any{"currentPassword": "guess-guess", "newPassword": "new-password"},
			hdr{"Cookie": cookie, "Origin": testBase, "X-Real-IP": "203.0.113." + string(rune('1'+i))})
		codes = append(codes, rec.Code)
	}
	if !slices.Equal(codes, []int{400, 400, 400, 429}) {
		t.Fatalf("codes = %v, want three guesses then 429", codes)
	}
}

func TestChangeEmailHidesTakenAddressWhenVerificationRequired(t *testing.T) {
	h := newHarness(t, func(c *Config) { c.RequireEmailVerification = true })
	ctx := context.Background()
	owner, taken := uniqueEmail(t), uniqueEmail(t)
	for _, e := range []string{owner, taken} {
		wantStatus(t, h.signUp(e, "password123"), http.StatusOK)
		rec := h.do("GET", pathOf(t, h.mails()[len(h.mails())-1].url), nil, nil)
		wantStatus(t, rec, http.StatusFound)
	}
	sent := len(h.mails())
	user, _, err := h.a.SignInEmail(ctx, SignInInput{Email: owner, Password: "password123"})
	if err != nil {
		t.Fatal(err)
	}
	if err := h.a.ChangeEmail(ctx, user.ID, taken, ""); err != nil {
		t.Fatalf("taken address revealed: %v", err)
	}
	if n := len(h.mails()); n != sent {
		t.Fatalf("taken address got mail (%d new)", n-sent)
	}
}

func TestSessionsListAndRevoke(t *testing.T) {
	h := newHarness(t, nil)
	ctx := context.Background()
	email, otherEmail := uniqueEmail(t), uniqueEmail(t)
	rec := h.signUp(email, "password123")
	cookie := hdr{"Cookie": cookieHeader(t, rec), "Origin": testBase}
	cookieID := h.getSession(cookie)["session"].(map[string]any)["id"].(string)
	b, bID := h.session(email, "password123")
	c, cID := h.session(email, "password123")
	wantStatus(t, h.signUp(otherEmail, "password123"), http.StatusOK)
	stranger, strangerID := h.session(otherEmail, "password123")
	userID := h.userID(cookie)

	// list-sessions: own sessions only, newest first, never a token.
	wantCode(t, h.do("GET", BasePath+"/list-sessions", nil, nil), http.StatusUnauthorized, "UNAUTHORIZED")
	rec = h.do("GET", BasePath+"/list-sessions", nil, c)
	wantStatus(t, rec, http.StatusOK)
	if strings.Contains(rec.Body.String(), `"token"`) {
		t.Fatalf("list-sessions exposes tokens: %s", rec.Body)
	}
	sessions, err := h.a.ListSessions(ctx, userID)
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, s := range sessions {
		if s.Token != "" || s.UserID != userID {
			t.Fatalf("session %+v", s)
		}
		ids = append(ids, s.ID)
	}
	if len(ids) != 3 || !slices.Contains(ids, cookieID) || !slices.Contains(ids, bID) || !slices.Contains(ids, cID) || slices.Contains(ids, strangerID) {
		t.Fatalf("ids = %v", ids)
	}
	if !sessions[0].CreatedAt.After(sessions[2].CreatedAt) && !sessions[0].CreatedAt.Equal(sessions[2].CreatedAt) {
		t.Fatalf("not newest first: %v", sessions)
	}

	revoke := func(headers hdr, body map[string]any) *httpResult {
		rec := h.do("POST", BasePath+"/revoke-session", body, headers)
		return &httpResult{rec.Code, rec.Body.String()}
	}
	// Someone else's session cannot be revoked, by id or by token.
	if r := revoke(c, map[string]any{"id": strangerID}); r.code != 404 || !strings.Contains(r.body, "SESSION_NOT_FOUND") {
		t.Fatalf("revoke stranger: %+v", r)
	}
	if r := revoke(c, map[string]any{"token": strings.TrimPrefix(stranger["Authorization"], "Bearer ")}); r.code != 404 {
		t.Fatalf("revoke stranger by token: %+v", r)
	}
	if err := h.a.RevokeSession(ctx, userID, strangerID); !errors.Is(err, ErrSessionNotFound) {
		t.Fatalf("service revoke stranger err = %v", err)
	}
	if r := revoke(c, map[string]any{}); r.code != 404 {
		t.Fatalf("revoke nothing: %+v", r)
	}
	if h.getSession(stranger) == nil {
		t.Fatal("stranger's session was revoked")
	}
	if r := revoke(hdr{"Cookie": cookie["Cookie"], "Origin": "https://evil.test"}, map[string]any{"id": bID}); r.code != 403 {
		t.Fatalf("cross-origin revoke: %+v", r)
	}

	// Revoking one of my sessions signs only that one out.
	if r := revoke(c, map[string]any{"id": bID}); r.code != 200 {
		t.Fatalf("revoke b: %+v", r)
	}
	if h.getSession(b) != nil || h.getSession(c) == nil || h.getSession(cookie) == nil {
		t.Fatal("wrong sessions after revoking b")
	}
	// Revoking the current cookie session also clears the cookie.
	rec = h.do("POST", BasePath+"/revoke-session", map[string]any{"id": cookieID}, cookie)
	wantStatus(t, rec, http.StatusOK)
	if ck := sessionCookie(rec); ck == nil || ck.MaxAge >= 0 {
		t.Fatal("cookie not cleared when revoking the current session")
	}
	if h.getSession(cookie) != nil {
		t.Fatal("current session survived")
	}

	// revoke-other-sessions keeps the caller's session only.
	d, _ := h.session(email, "password123")
	e, _ := h.session(email, "password123")
	wantStatus(t, h.do("POST", BasePath+"/revoke-other-sessions", nil, c), http.StatusOK)
	if h.getSession(c) == nil || h.getSession(d) != nil || h.getSession(e) != nil || h.getSession(stranger) == nil {
		t.Fatal("revoke-other-sessions revoked the wrong sessions")
	}
	if sessions, _ := h.a.ListSessions(ctx, userID); len(sessions) != 1 || sessions[0].ID != cID {
		t.Fatalf("remaining = %+v", sessions)
	}
	wantCode(t, h.do("POST", BasePath+"/revoke-other-sessions", nil, nil), http.StatusUnauthorized, "UNAUTHORIZED")

	// Expired sessions are not listed.
	h.clock.Advance(DefaultSessionTTL + time.Minute)
	if sessions, _ := h.a.ListSessions(ctx, userID); len(sessions) != 0 {
		t.Fatalf("expired sessions listed: %+v", sessions)
	}
}
