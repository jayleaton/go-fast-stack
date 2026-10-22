package auth

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

// eventLog collects delivered events.
type eventLog struct {
	mu     sync.Mutex
	events []Event
}

func (l *eventLog) handle(_ context.Context, e Event) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.events = append(l.events, e)
}

// take waits for queued events (Shutdown drains them) and returns and clears them.
func (l *eventLog) take(t *testing.T, a *Auth) []Event {
	t.Helper()
	if err := a.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	out := l.events
	l.events = nil
	return out
}

func kinds(events []Event) string {
	var k []string
	for _, e := range events {
		k = append(k, string(e.Kind))
	}
	return strings.Join(k, ",")
}

func withEvents(l *eventLog, more ...func(*Config)) func(*Config) {
	return func(c *Config) {
		c.Events = l.handle
		for _, m := range more {
			m(c)
		}
	}
}

func TestEventsForAccountFlows(t *testing.T) {
	log := &eventLog{}
	h := newHarness(t, withEvents(log))
	ctx := context.Background()
	email := uniqueEmail(t)
	ua := hdr{"User-Agent": "events-test"}

	rec := h.do("POST", BasePath+"/sign-up/email", map[string]any{"name": "E", "email": email, "password": "password123"}, ua)
	wantStatus(t, rec, http.StatusOK)
	cookie := hdr{"Cookie": cookieHeader(t, rec), "Origin": testBase}
	id := h.userID(cookie)
	ev := log.take(t, h.a)
	if kinds(ev) != "sign_in" || ev[0].Method != SignInMethodSignUp || ev[0].UserID != id || ev[0].Email != email ||
		ev[0].SessionID == "" || ev[0].IP != "192.0.2.1" || ev[0].UserAgent != "events-test" || ev[0].At.IsZero() {
		t.Fatalf("sign-up events = %+v", ev)
	}

	// Failures: a known account is named (with its email); an unknown
	// address is not echoed back, since it may be a mistyped password.
	wantStatus(t, h.signIn(email, "wrong-password"), http.StatusUnauthorized)
	wantStatus(t, h.signIn("password123@not-an-account.example", "password123"), http.StatusUnauthorized)
	ev = log.take(t, h.a)
	if kinds(ev) != "sign_in_failed,sign_in_failed" ||
		ev[0].UserID != id || ev[0].Email != email || ev[0].Reason != "INVALID_EMAIL_OR_PASSWORD" ||
		ev[1].UserID != "" || ev[1].Email != "" || ev[1].Reason != "INVALID_EMAIL_OR_PASSWORD" {
		t.Fatalf("failure events = %+v", ev)
	}

	rec = h.signIn(email, "password123")
	token := jsonBody(t, rec)["token"].(string)
	bearer := hdr{"Authorization": "Bearer " + token}
	ev = log.take(t, h.a)
	if kinds(ev) != "sign_in" || ev[0].Method != SignInMethodPassword || ev[0].SessionID == "" {
		t.Fatalf("sign-in events = %+v", ev)
	}
	bearerSession := ev[0].SessionID

	wantStatus(t, h.do("POST", BasePath+"/sign-out", nil, bearer), http.StatusOK)
	ev = log.take(t, h.a)
	if kinds(ev) != "sign_out" || ev[0].SessionID != bearerSession || ev[0].UserID != id {
		t.Fatalf("sign-out events = %+v", ev)
	}

	// Revocations by the user.
	_, other := h.session(email, "password123")
	log.take(t, h.a)
	wantStatus(t, h.do("POST", BasePath+"/revoke-session", map[string]any{"id": other}, cookie), http.StatusOK)
	ev = log.take(t, h.a)
	if kinds(ev) != "session_revoked" || ev[0].SessionID != other || ev[0].Details["count"] != "1" || ev[0].Details["by"] != "user" {
		t.Fatalf("revoke events = %+v", ev)
	}
	h.session(email, "password123")
	h.session(email, "password123")
	log.take(t, h.a)
	wantStatus(t, h.do("POST", BasePath+"/revoke-other-sessions", nil, cookie), http.StatusOK)
	if ev = log.take(t, h.a); kinds(ev) != "session_revoked" || ev[0].Details["count"] != "2" {
		t.Fatalf("revoke-others events = %+v", ev)
	}

	// Password change revoking the others.
	h.session(email, "password123")
	log.take(t, h.a)
	wantStatus(t, h.do("POST", BasePath+"/change-password", map[string]any{"currentPassword": "password123", "newPassword": "password456", "revokeOtherSessions": true}, cookie), http.StatusOK)
	ev = log.take(t, h.a)
	if kinds(ev) != "password_changed,session_revoked" || ev[1].Details["by"] != "password_change" || ev[1].Details["count"] != "1" {
		t.Fatalf("password change events = %+v", ev)
	}

	// Email change: requested, then confirmed.
	newEmail := uniqueEmail(t)
	wantStatus(t, h.do("POST", BasePath+"/change-email", map[string]any{"newEmail": newEmail}, cookie), http.StatusOK)
	link := h.changeLinks()[newEmail]
	if _, _, err := h.a.VerifyEmail(ctx, link); err != nil {
		t.Fatal(err)
	}
	ev = log.take(t, h.a)
	if kinds(ev) != "email_change_requested,email_changed,sign_in" || ev[0].Details["newEmail"] != newEmail ||
		ev[1].Details["oldEmail"] != email || ev[1].Details["newEmail"] != newEmail || ev[2].Method != SignInMethodEmailChanged {
		t.Fatalf("email change events = %+v", ev)
	}

	// Reset by link, then admin actions.
	if err := h.a.RequestPasswordReset(ctx, newEmail, "/r"); err != nil {
		t.Fatal(err)
	}
	var resetLink string
	for _, m := range h.mails() {
		if m.kind == "reset" {
			resetLink = m.url
		}
	}
	resetToken := mustQuery(t, h.do("GET", pathOf(t, resetLink), nil, nil).Header().Get("Location"), "token")
	if err := h.a.ResetPassword(ctx, resetToken, "password789"); err != nil {
		t.Fatal(err)
	}
	h.session(newEmail, "password789")
	log.take(t, h.a)
	admin := h.a.Users()
	if err := admin.BanUser(ctx, id, "spam", time.Hour); err != nil {
		t.Fatal(err)
	}
	if err := admin.UnbanUser(ctx, id); err != nil {
		t.Fatal(err)
	}
	ev = log.take(t, h.a)
	if kinds(ev) != "user_banned,session_revoked,user_unbanned" || ev[0].Details["reason"] != "spam" || ev[0].Details["expiresAt"] == "" ||
		ev[1].Details["by"] != "ban" || ev[1].Details["count"] != "1" {
		t.Fatalf("ban events = %+v", ev)
	}
}

func TestTwoFactorEventsNameTheChallengedUser(t *testing.T) {
	log := &eventLog{}
	h := newHarness(t, withEvents(log, with2FA(false)))
	email := uniqueEmail(t)
	setup, signedIn := h.enrol(email)
	id := h.userID(signedIn)
	ev := log.take(t, h.a)
	if kinds(ev) != "sign_in,two_factor_succeeded,two_factor_enabled" || ev[2].UserID != id {
		t.Fatalf("enrol events = %+v", ev)
	}

	pending := hdr{"Cookie": h.challenge(email, "password123"), "Origin": testBase}
	wantStatus(t, h.do("POST", BasePath+"/two-factor/verify-totp", map[string]any{"code": h.wrongCode(setup.Secret)}, pending), http.StatusUnauthorized)
	wantStatus(t, h.do("POST", BasePath+"/two-factor/verify-backup-code", map[string]any{"code": setup.BackupCodes[0]}, pending), http.StatusOK)
	wantStatus(t, h.do("POST", BasePath+"/two-factor/verify-totp", map[string]any{"code": "123456"}, nil), http.StatusUnauthorized)
	ev = log.take(t, h.a)
	want := "two_factor_failed,two_factor_succeeded,sign_in,two_factor_failed"
	if kinds(ev) != want ||
		ev[0].UserID != id || ev[0].Email != email || ev[0].Reason != "INVALID_CODE" || ev[0].Method != TwoFactorMethodTOTP ||
		ev[1].UserID != id || ev[1].Method != TwoFactorMethodBackup ||
		ev[2].Method != SignInMethodTwoFactor || ev[2].Details["factor"] != TwoFactorMethodBackup || ev[2].SessionID == "" ||
		ev[3].UserID != "" || ev[3].Reason != "INVALID_TWO_FACTOR_COOKIE" {
		t.Fatalf("2FA events = %+v", ev)
	}
	// The sign-in that stopped at the challenge is not itself an event.
	wantStatus(t, h.do("POST", BasePath+"/two-factor/disable", map[string]any{"password": "password123"}, signedIn), http.StatusOK)
	if ev = log.take(t, h.a); kinds(ev) != "two_factor_disabled" || ev[0].UserID != id {
		t.Fatalf("disable events = %+v", ev)
	}
}

// TestEventsNeverCarrySecrets serialises every event of a busy session and
// looks for the tokens, passwords and codes involved.
func TestEventsNeverCarrySecrets(t *testing.T) {
	log := &eventLog{}
	h := newHarness(t, withEvents(log, with2FA(false)))
	email := uniqueEmail(t)
	setup, _ := h.enrol(email)
	pending := hdr{"Cookie": h.challenge(email, "password123"), "Origin": testBase}
	rec := h.do("POST", BasePath+"/two-factor/verify-totp", map[string]any{"code": h.code(setup.Secret), "trustDevice": true}, pending)
	token := jsonBody(t, rec)["token"].(string)
	raw, err := json.Marshal(log.take(t, h.a))
	if err != nil {
		t.Fatal(err)
	}
	secrets := append([]string{token, "password123", setup.Secret, h.code(setup.Secret), pending["Cookie"]}, setup.BackupCodes...)
	for _, s := range secrets {
		for _, part := range strings.Split(s, "; ") {
			if v := part[strings.IndexByte(part, '=')+1:]; len(v) >= 6 && bytes.Contains(raw, []byte(v)) {
				t.Fatalf("event JSON contains a secret (%q): %s", v, raw)
			}
		}
	}
}

func TestEventHandlerCannotBreakAuth(t *testing.T) {
	// A panicking handler: auth keeps working and Shutdown still returns.
	panicky := newHarness(t, func(c *Config) { c.Events = func(context.Context, Event) { panic("boom") } })
	email := uniqueEmail(t)
	wantStatus(t, panicky.signUp(email, "password123"), http.StatusOK)
	wantStatus(t, panicky.signIn(email, "password123"), http.StatusOK)
	if err := panicky.a.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(panicky.logBuf.String(), "event handler panicked") {
		t.Fatalf("panic not logged: %s", panicky.logBuf)
	}

	// A handler that never returns: sign-ins go on, the bounded queue fills
	// and further events are dropped with a warning instead of waiting.
	gate := make(chan struct{})
	var delivered int
	var mu sync.Mutex
	blocked := newHarness(t, func(c *Config) {
		c.Events = func(context.Context, Event) {
			<-gate
			mu.Lock()
			delivered++
			mu.Unlock()
		}
	})
	user, _, err := blocked.a.SignUpEmail(context.Background(), SignUpInput{Email: uniqueEmail(t), Password: "password123"})
	if err != nil {
		t.Fatal(err)
	}
	for range eventQueueSize + 50 {
		blocked.a.emit(context.Background(), Event{Kind: EventSignIn, UserID: user.ID})
	}
	wantStatus(t, blocked.signIn(user.Email, "password123"), http.StatusOK) // still instant
	if !strings.Contains(blocked.logBuf.String(), "event queue full") {
		t.Fatal("dropped events not logged")
	}
	short, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if err := blocked.a.Shutdown(short); err == nil {
		t.Fatal("Shutdown returned while events were still queued")
	}
	close(gate)
	if err := blocked.a.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if delivered < eventQueueSize || delivered > eventQueueSize+52 {
		t.Fatalf("delivered %d events", delivered)
	}
}

func TestStandaloneUserAdminEvents(t *testing.T) {
	h := newHarness(t, nil)
	ctx := context.Background()
	email := uniqueEmail(t)
	cookie := hdr{"Cookie": cookieHeader(t, h.signUp(email, "password123"))}
	id := h.userID(cookie)

	log := &eventLog{}
	admin := NewUserAdmin(testPool).WithEvents(log.handle)
	if err := admin.RevokeUserSessions(ctx, id); err != nil {
		t.Fatal(err)
	}
	if err := admin.ResetTwoFactor(ctx, id); err != nil {
		t.Fatal(err)
	}
	// Without events nothing is reported and nothing breaks.
	if err := NewUserAdmin(testPool).BanUser(ctx, id, "", 0); err != nil {
		t.Fatal(err)
	}
	if err := admin.sink.drain(ctx); err != nil {
		t.Fatal(err)
	}
	log.mu.Lock()
	defer log.mu.Unlock()
	if kinds(log.events) != "session_revoked,two_factor_disabled" || log.events[0].Details["by"] != "admin" ||
		log.events[0].Email != email || log.events[1].Details["by"] != "admin" {
		t.Fatalf("standalone events = %+v", log.events)
	}
}
