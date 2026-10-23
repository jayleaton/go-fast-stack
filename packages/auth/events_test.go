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

func (log *eventLog) handle(_ context.Context, event Event) {
	log.mu.Lock()
	defer log.mu.Unlock()
	log.events = append(log.events, event)
}

// take waits for queued events (Shutdown drains them) and returns and clears them.
func (log *eventLog) take(test *testing.T, auth *Auth) []Event {
	test.Helper()
	if err := auth.Shutdown(context.Background()); err != nil {
		test.Fatal(err)
	}
	log.mu.Lock()
	defer log.mu.Unlock()
	out := log.events
	log.events = nil
	return out
}

func kinds(events []Event) string {
	var keys []string
	for _, event := range events {
		keys = append(keys, string(event.Kind))
	}
	return strings.Join(keys, ",")
}

func withEvents(log *eventLog, more ...func(*Config)) func(*Config) {
	return func(config *Config) {
		config.Events = log.handle
		for _, fn := range more {
			fn(config)
		}
	}
}

func TestEventsForAccountFlows(test *testing.T) {
	test.Parallel()
	log := &eventLog{}
	harness := newHarness(test, withEvents(log))
	ctx := context.Background()
	email := uniqueEmail(test)
	ua := hdr{"User-Agent": "events-test"}

	rec := harness.do("POST", BasePath+"/sign-up/email", map[string]any{"name": "E", "email": email, "password": "password123"}, ua)
	wantStatus(test, rec, http.StatusOK)
	cookie := hdr{"Cookie": cookieHeader(test, rec), "Origin": testBase}
	id := harness.userID(cookie)
	ev := log.take(test, harness.auth)
	if kinds(ev) != "sign_in" || ev[0].Method != SignInMethodSignUp || ev[0].UserID != id || ev[0].Email != email ||
		ev[0].SessionID == "" || ev[0].IP != "192.0.2.1" || ev[0].UserAgent != "events-test" || ev[0].At.IsZero() {
		test.Fatalf("sign-up events = %+v", ev)
	}

	// Failures: a known account is named (with its email); an unknown
	// address is not echoed back, since it may be a mistyped password.
	wantStatus(test, harness.signIn(email, "wrong-password"), http.StatusUnauthorized)
	wantStatus(test, harness.signIn("password123@not-an-account.example", "password123"), http.StatusUnauthorized)
	ev = log.take(test, harness.auth)
	if kinds(ev) != "sign_in_failed,sign_in_failed" ||
		ev[0].UserID != id || ev[0].Email != email || ev[0].Reason != "INVALID_EMAIL_OR_PASSWORD" ||
		ev[1].UserID != "" || ev[1].Email != "" || ev[1].Reason != "INVALID_EMAIL_OR_PASSWORD" {
		test.Fatalf("failure events = %+v", ev)
	}

	rec = harness.signIn(email, "password123")
	token := jsonBody(test, rec)["token"].(string)
	bearer := hdr{"Authorization": "Bearer " + token}
	ev = log.take(test, harness.auth)
	if kinds(ev) != "sign_in" || ev[0].Method != SignInMethodPassword || ev[0].SessionID == "" {
		test.Fatalf("sign-in events = %+v", ev)
	}
	bearerSession := ev[0].SessionID

	wantStatus(test, harness.do("POST", BasePath+"/sign-out", nil, bearer), http.StatusOK)
	ev = log.take(test, harness.auth)
	if kinds(ev) != "sign_out" || ev[0].SessionID != bearerSession || ev[0].UserID != id {
		test.Fatalf("sign-out events = %+v", ev)
	}

	// Revocations by the user.
	_, other := harness.session(email, "password123")
	log.take(test, harness.auth)
	wantStatus(test, harness.do("POST", BasePath+"/revoke-session", map[string]any{"id": other}, cookie), http.StatusOK)
	ev = log.take(test, harness.auth)
	if kinds(ev) != "session_revoked" || ev[0].SessionID != other || ev[0].Details["count"] != "1" || ev[0].Details["by"] != "user" {
		test.Fatalf("revoke events = %+v", ev)
	}
	harness.session(email, "password123")
	harness.session(email, "password123")
	log.take(test, harness.auth)
	wantStatus(test, harness.do("POST", BasePath+"/revoke-other-sessions", nil, cookie), http.StatusOK)
	if ev = log.take(test, harness.auth); kinds(ev) != "session_revoked" || ev[0].Details["count"] != "2" {
		test.Fatalf("revoke-others events = %+v", ev)
	}

	// Password change revoking the others.
	harness.session(email, "password123")
	log.take(test, harness.auth)
	wantStatus(test, harness.do("POST", BasePath+"/change-password", map[string]any{"currentPassword": "password123", "newPassword": "password456", "revokeOtherSessions": true}, cookie), http.StatusOK)
	ev = log.take(test, harness.auth)
	if kinds(ev) != "password_changed,session_revoked" || ev[1].Details["by"] != "password_change" || ev[1].Details["count"] != "1" {
		test.Fatalf("password change events = %+v", ev)
	}

	// Email change: requested, then confirmed.
	newEmail := uniqueEmail(test)
	wantStatus(test, harness.do("POST", BasePath+"/change-email", map[string]any{"newEmail": newEmail}, cookie), http.StatusOK)
	link := harness.changeLinks()[newEmail]
	if _, _, err := harness.auth.VerifyEmail(ctx, link); err != nil {
		test.Fatal(err)
	}
	ev = log.take(test, harness.auth)
	if kinds(ev) != "email_change_requested,email_changed,sign_in" || ev[0].Details["newEmail"] != newEmail ||
		ev[1].Details["oldEmail"] != email || ev[1].Details["newEmail"] != newEmail || ev[2].Method != SignInMethodEmailChanged {
		test.Fatalf("email change events = %+v", ev)
	}

	// Reset by link, then admin actions.
	if err := harness.auth.RequestPasswordReset(ctx, newEmail, "/r"); err != nil {
		test.Fatal(err)
	}
	var resetLink string
	for _, mail := range harness.mails() {
		if mail.kind == "reset" {
			resetLink = mail.url
		}
	}
	resetToken := mustQuery(test, resetLink, "token")
	log.take(test, harness.auth)
	if err := harness.auth.ResetPassword(ctx, resetToken, "password789"); err != nil {
		test.Fatal(err)
	}
	if ev = log.take(test, harness.auth); kinds(ev) != "password_reset,session_revoked" || ev[1].Details["by"] != "password_reset" {
		test.Fatalf("reset events = %+v", ev)
	}
	harness.session(newEmail, "password789")
	log.take(test, harness.auth)
	admin := harness.auth.Users()
	if err := admin.BanUser(ctx, id, "spam", time.Hour); err != nil {
		test.Fatal(err)
	}
	if err := admin.UnbanUser(ctx, id); err != nil {
		test.Fatal(err)
	}
	ev = log.take(test, harness.auth)
	if kinds(ev) != "user_banned,session_revoked,user_unbanned" || ev[0].Details["reason"] != "spam" || ev[0].Details["expiresAt"] == "" ||
		ev[1].Details["by"] != "ban" || ev[1].Details["count"] != "1" {
		test.Fatalf("ban events = %+v", ev)
	}
}

func TestTwoFactorEventsNameTheChallengedUser(test *testing.T) {
	test.Parallel()
	log := &eventLog{}
	harness := newHarness(test, withEvents(log, with2FA(false)))
	email := uniqueEmail(test)
	setup, signedIn := harness.enrol(email)
	id := harness.userID(signedIn)
	ev := log.take(test, harness.auth)
	if kinds(ev) != "sign_in,two_factor_succeeded,two_factor_enabled" || ev[2].UserID != id {
		test.Fatalf("enrol events = %+v", ev)
	}

	pending := hdr{"Cookie": harness.challenge(email, "password123"), "Origin": testBase}
	wantStatus(test, harness.do("POST", BasePath+"/two-factor/verify-totp", map[string]any{"code": harness.wrongCode(setup.Secret)}, pending), http.StatusUnauthorized)
	wantStatus(test, harness.do("POST", BasePath+"/two-factor/verify-backup-code", map[string]any{"code": setup.BackupCodes[0]}, pending), http.StatusOK)
	// No valid pending sign-in: nobody to attribute it to, so no event.
	wantStatus(test, harness.do("POST", BasePath+"/two-factor/verify-totp", map[string]any{"code": "123456"}, nil), http.StatusUnauthorized)
	ev = log.take(test, harness.auth)
	want := "two_factor_failed,two_factor_succeeded,sign_in"
	if kinds(ev) != want ||
		ev[0].UserID != id || ev[0].Email != email || ev[0].Reason != "INVALID_CODE" || ev[0].Method != TwoFactorMethodTOTP ||
		ev[1].UserID != id || ev[1].Method != TwoFactorMethodBackup ||
		ev[2].Method != SignInMethodTwoFactor || ev[2].Details["factor"] != TwoFactorMethodBackup || ev[2].SessionID == "" {
		test.Fatalf("2FA events = %+v", ev)
	}
	// The sign-in that stopped at the challenge is not itself an event.
	wantStatus(test, harness.do("POST", BasePath+"/two-factor/disable", map[string]any{"password": "password123"}, signedIn), http.StatusOK)
	if ev = log.take(test, harness.auth); kinds(ev) != "two_factor_disabled" || ev[0].UserID != id {
		test.Fatalf("disable events = %+v", ev)
	}
}

// TestEventsNeverCarrySecrets serialises every event of a busy session and
// looks for the tokens, passwords and codes involved.
func TestEventsNeverCarrySecrets(test *testing.T) {
	test.Parallel()
	log := &eventLog{}
	harness := newHarness(test, withEvents(log, with2FA(false)))
	email := uniqueEmail(test)
	setup, _ := harness.enrol(email)
	pending := hdr{"Cookie": harness.challenge(email, "password123"), "Origin": testBase}
	rec := harness.do("POST", BasePath+"/two-factor/verify-totp", map[string]any{"code": harness.code(setup.Secret), "trustDevice": true}, pending)
	token := jsonBody(test, rec)["token"].(string)
	raw, err := json.Marshal(log.take(test, harness.auth))
	if err != nil {
		test.Fatal(err)
	}
	secrets := append([]string{token, "password123", setup.Secret, harness.code(setup.Secret), pending["Cookie"]}, setup.BackupCodes...)
	for _, secret := range secrets {
		for _, part := range strings.Split(secret, "; ") {
			if value := part[strings.IndexByte(part, '=')+1:]; len(value) >= 6 && bytes.Contains(raw, []byte(value)) {
				test.Fatalf("event JSON contains a secret (%q): %s", value, raw)
			}
		}
	}
}

func TestEventHandlerCannotBreakAuth(test *testing.T) {
	test.Parallel()
	// A panicking handler: auth keeps working and Shutdown still returns.
	panicky := newHarness(test, func(config *Config) { config.Events = func(context.Context, Event) { panic("boom") } })
	email := uniqueEmail(test)
	wantStatus(test, panicky.signUp(email, "password123"), http.StatusOK)
	wantStatus(test, panicky.signIn(email, "password123"), http.StatusOK)
	if err := panicky.auth.Shutdown(context.Background()); err != nil {
		test.Fatal(err)
	}
	if !strings.Contains(panicky.logBuf.String(), "event handler panicked") {
		test.Fatalf("panic not logged: %s", panicky.logBuf)
	}

	// The worker survives a panic: events after it are still delivered.
	var calls int
	var callsMu sync.Mutex
	survivor := newHarness(test, func(config *Config) {
		config.Events = func(context.Context, Event) {
			callsMu.Lock()
			calls++
			count := calls
			callsMu.Unlock()
			if count == 1 {
				panic("first event")
			}
		}
	})
	wantStatus(test, survivor.signUp(uniqueEmail(test), "password123"), http.StatusOK)
	wantStatus(test, survivor.signUp(uniqueEmail(test), "password123"), http.StatusOK)
	if err := survivor.auth.Shutdown(context.Background()); err != nil {
		test.Fatal(err)
	}
	callsMu.Lock()
	if calls != 2 {
		test.Fatalf("handler called %d times after a panic, want 2", calls)
	}
	callsMu.Unlock()

	// A handler that calls back into Auth while Shutdown is draining with a
	// short deadline: Shutdown gives up on time and auth keeps working.
	release := make(chan struct{})
	var reentrant *harness
	reentrant = newHarness(test, func(config *Config) {
		config.Events = func(ctx context.Context, event Event) {
			if event.Kind == EventSignIn {
				<-release
				_ = reentrant.auth.Users().UnbanUser(ctx, event.UserID) // emits from inside the handler
			}
		}
	})
	reEmail := uniqueEmail(test)
	wantStatus(test, reentrant.signUp(reEmail, "password123"), http.StatusOK)
	short, cancelShort := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancelShort()
	if err := reentrant.auth.Shutdown(short); err == nil {
		test.Fatal("Shutdown returned while the handler was still blocked")
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		reentrant.signIn(reEmail, "password123") // must not wait for the drain or the handler
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		test.Fatal("sign-in blocked behind a draining Shutdown")
	}
	close(release)
	if err := reentrant.auth.Shutdown(context.Background()); err != nil {
		test.Fatal(err)
	}

	// A handler that never returns: sign-ins go on, the bounded queue fills
	// and further events are dropped with a warning instead of waiting.
	gate := make(chan struct{})
	var delivered int
	var mu sync.Mutex
	blocked := newHarness(test, func(config *Config) {
		config.Events = func(context.Context, Event) {
			<-gate
			mu.Lock()
			delivered++
			mu.Unlock()
		}
	})
	user, _, err := blocked.auth.SignUpEmail(context.Background(), SignUpInput{Email: uniqueEmail(test), Password: "password123"})
	if err != nil {
		test.Fatal(err)
	}
	for range eventQueueSize + 50 {
		blocked.auth.emit(context.Background(), Event{Kind: EventSignIn, UserID: user.ID})
	}
	wantStatus(test, blocked.signIn(user.Email, "password123"), http.StatusOK) // still instant
	if !strings.Contains(blocked.logBuf.String(), "event queue full") || !strings.Contains(blocked.logBuf.String(), "dropped_total=1\n") {
		test.Fatalf("dropped events not logged with a total: %s", blocked.logBuf)
	}
	// The queue holds 1024 (plus one with the handler): the 50 extra emits and
	// the sign-in are dropped and counted, give or take the sign-up's events.
	if events := blocked.auth.DroppedEvents(); events < 50 || events > 54 {
		test.Fatalf("DroppedEvents = %d, want about 51", events)
	}
	if strings.Count(blocked.logBuf.String(), "event queue full") > 2 {
		test.Fatal("every drop was logged; drops should be sampled")
	}
	short, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if err := blocked.auth.Shutdown(short); err == nil {
		test.Fatal("Shutdown returned while events were still queued")
	}
	close(gate)
	if err := blocked.auth.Shutdown(context.Background()); err != nil {
		test.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if delivered < eventQueueSize || delivered > eventQueueSize+52 {
		test.Fatalf("delivered %d events", delivered)
	}
}

func TestStandaloneUserAdminEvents(test *testing.T) {
	test.Parallel()
	harness := newHarness(test, nil)
	ctx := context.Background()
	email := uniqueEmail(test)
	cookie := hdr{"Cookie": cookieHeader(test, harness.signUp(email, "password123"))}
	id := harness.userID(cookie)

	log := &eventLog{}
	admin := NewUserAdmin(testPool).WithEvents(log.handle)
	if err := admin.RevokeUserSessions(ctx, id); err != nil {
		test.Fatal(err)
	}
	if err := admin.ResetTwoFactor(ctx, id); err != nil {
		test.Fatal(err)
	}
	// Without events nothing is reported and nothing breaks.
	if err := NewUserAdmin(testPool).BanUser(ctx, id, "", 0); err != nil {
		test.Fatal(err)
	}
	if err := admin.Shutdown(ctx); err != nil {
		test.Fatal(err)
	}
	log.mu.Lock()
	defer log.mu.Unlock()
	if kinds(log.events) != "session_revoked,two_factor_disabled" || log.events[0].Details["by"] != "admin" ||
		log.events[0].Email != email || log.events[1].Details["by"] != "admin" {
		test.Fatalf("standalone events = %+v", log.events)
	}
}
