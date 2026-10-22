package auth

import (
	"context"
	"log/slog"
	"runtime/debug"
	"sync"
	"time"
)

// EventKind names an auth event. Values are stable strings, suitable for
// audit logs.
type EventKind string

const (
	EventSignIn               EventKind = "sign_in"                // a session was created; Method says how
	EventSignInFailed         EventKind = "sign_in_failed"         // sign-in refused; Reason is the error code
	EventTwoFactorSucceeded   EventKind = "two_factor_succeeded"   // a second factor (or enrolment code) was accepted
	EventTwoFactorFailed      EventKind = "two_factor_failed"      // a second factor was refused; Reason is the error code
	EventTwoFactorEnabled     EventKind = "two_factor_enabled"     // enrolment confirmed
	EventTwoFactorDisabled    EventKind = "two_factor_disabled"    // by the user (DisableTwoFactor) or an admin (ResetTwoFactor)
	EventSignOut              EventKind = "sign_out"               // the user ended their own session
	EventSessionRevoked       EventKind = "session_revoked"        // sessions ended by the user elsewhere, an admin or a ban; Details["count"]
	EventPasswordChanged      EventKind = "password_changed"       // ChangePassword
	EventPasswordReset        EventKind = "password_reset"         // a reset or invite (set-password) link was used; Method "reset" or "set_password"
	EventEmailChangeRequested EventKind = "email_change_requested" // Details["newEmail"]
	EventEmailChanged         EventKind = "email_changed"          // the link was opened; Details["oldEmail"], Details["newEmail"]
	EventUserBanned           EventKind = "user_banned"            // Details["reason"], Details["expiresAt"] (RFC 3339; absent: permanent)
	EventUserUnbanned         EventKind = "user_unbanned"
)

// How a session was created (Event.Method of EventSignIn).
const (
	SignInMethodPassword      = "password"           // email + password, no second factor needed
	SignInMethodTwoFactor     = "two_factor"         // password, then a TOTP or backup code
	SignInMethodSetPassword   = "set_password"       // the invite / set-password link
	SignInMethodSignUp        = "sign_up"            // sign-up signed the user straight in
	SignInMethodEmailVerified = "email_verification" // the verification link signed the user in
	SignInMethodEmailChanged  = "email_change"       // the change-email link signed the user in
)

// Event delivery limits (see EventHandler).
const (
	eventQueueSize       = 1024
	eventDeliveryTimeout = 5 * time.Second
)

// Event is something that happened in auth, delivered after the fact. It
// never contains a password, token, secret or code. Email is the account's
// address and is only set for a known user: a failed sign-in for an unknown
// address carries no email, since people type passwords into email fields.
type Event struct {
	Kind      EventKind         `json:"kind"`
	At        time.Time         `json:"at"`
	UserID    string            `json:"userId,omitempty"`    // empty when unknown (e.g. sign-in for an unknown email)
	Email     string            `json:"email,omitempty"`     // the account's address (filled in before delivery)
	SessionID string            `json:"sessionId,omitempty"` // the session created or ended (its id, never its token)
	Method    string            `json:"method,omitempty"`    // SignInMethod* for EventSignIn; "totp"/"backup_code" for 2FA events
	Reason    string            `json:"reason,omitempty"`    // the Error code of a failure (e.g. INVALID_EMAIL_OR_PASSWORD)
	IP        string            `json:"ip,omitempty"`        // client IP under the Config.ClientIPHeader rules
	UserAgent string            `json:"userAgent,omitempty"` //
	Details   map[string]string `json:"details,omitempty"`   // kind-specific, see the EventKind docs
}

// EventHandler receives events (Config.Events). It runs on a background
// worker, one event at a time in the order they happened, with a context that
// keeps the request's values but not its cancellation and expires after 5s.
// It cannot block or fail auth: events wait in a bounded queue (1024) and are
// dropped with a warning when it is full, and a panic is recovered and logged.
// Auth.Shutdown waits for queued events.
type EventHandler func(ctx context.Context, e Event)

// eventSink delivers events to a handler in order on one worker goroutine.
type eventSink struct {
	handler EventHandler
	log     *slog.Logger
	queue   chan queuedEvent
	pending sync.WaitGroup
	mu      sync.RWMutex                                    // emit holds it shared so drain's Wait never races an Add
	email   func(ctx context.Context, userID string) string // fills Event.Email
}

type queuedEvent struct {
	ctx context.Context
	ev  Event
}

func newEventSink(h EventHandler, log *slog.Logger, email func(context.Context, string) string) *eventSink {
	if h == nil {
		return nil
	}
	s := &eventSink{handler: h, log: log, queue: make(chan queuedEvent, eventQueueSize), email: email}
	go s.run()
	return s
}

// emit queues ev without ever waiting: a full queue drops it.
func (s *eventSink) emit(ctx context.Context, ev Event) {
	if s == nil {
		return
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	s.pending.Add(1)
	select {
	case s.queue <- queuedEvent{ctx: context.WithoutCancel(ctx), ev: ev}:
	default:
		s.pending.Done()
		s.log.WarnContext(ctx, "auth: event queue full, event dropped", "kind", ev.Kind, "userId", ev.UserID)
	}
}

func (s *eventSink) run() {
	for q := range s.queue {
		s.deliver(q)
	}
}

func (s *eventSink) deliver(q queuedEvent) {
	defer s.pending.Done()
	ctx, cancel := context.WithTimeout(q.ctx, eventDeliveryTimeout)
	defer cancel()
	defer func() {
		if r := recover(); r != nil {
			s.log.ErrorContext(ctx, "auth: event handler panicked", "kind", q.ev.Kind, "panic", r, "stack", string(debug.Stack()))
		}
	}()
	if q.ev.Email == "" && q.ev.UserID != "" && s.email != nil {
		q.ev.Email = s.email(ctx, q.ev.UserID)
	}
	s.handler(ctx, q.ev)
}

// drain waits until every queued event has been delivered (or ctx ends).
func (s *eventSink) drain(ctx context.Context) error {
	if s == nil {
		return nil
	}
	done := make(chan struct{})
	go func() {
		s.mu.Lock()
		s.pending.Wait()
		s.mu.Unlock()
		close(done)
	}()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// userEmail looks up a user's address for an event, or "".
func (st *store) userEmail(ctx context.Context, userID string) string {
	var email string
	_ = st.pool.QueryRow(ctx, `SELECT "email" FROM "user" WHERE "id" = $1`, userID).Scan(&email)
	return email
}

// emit stamps an event with the time and this instance's request metadata
// and queues it for Config.Events.
func (a *Auth) emit(ctx context.Context, ev Event) {
	if a.events == nil {
		return
	}
	ev.At = a.now()
	if m := a.meta(ctx); m != nil {
		ev.IP, ev.UserAgent = m.ip, m.userAgent
	}
	a.events.emit(ctx, ev)
}

// emitFailure reports a refused sign-in or second factor with the error code.
func (a *Auth) emitFailure(ctx context.Context, kind EventKind, userID, method string, err error) {
	code := "INTERNAL_ERROR"
	if e := asError(err); e != nil {
		code = e.Code
	}
	a.emit(ctx, Event{Kind: kind, UserID: userID, Method: method, Reason: code})
}
