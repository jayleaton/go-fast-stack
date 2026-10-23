package auth

import (
	"context"
	"errors"
	"log/slog"
	"runtime/debug"
	"sync"
	"sync/atomic"
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
	EventSessionRevoked       EventKind = "session_revoked"        // sessions ended other than by signing out; Details["count"], Details["by"]
	EventPasswordChanged      EventKind = "password_changed"       // ChangePassword
	EventPasswordReset        EventKind = "password_reset"         // a reset or invite (set-password) link was used; Method "reset" or "set_password"
	EventEmailChangeRequested EventKind = "email_change_requested" // Details["newEmail"]
	EventEmailChanged         EventKind = "email_changed"          // the link was opened; Details["oldEmail"], Details["newEmail"]
	EventUserBanned           EventKind = "user_banned"            // Details["reason"], Details["expiresAt"] (RFC 3339; absent: permanent)
	EventUserUnbanned         EventKind = "user_unbanned"
	EventAccountLinked        EventKind = "account_linked" // a social account was linked to an existing user by verified email; Details["provider"]
)

// How a session was created (Event.Method of EventSignIn).
const (
	SignInMethodPassword      = "password"           // email + password, no second factor needed
	SignInMethodTwoFactor     = "two_factor"         // password, then a TOTP or backup code
	SignInMethodSetPassword   = "set_password"       // the invite / set-password link
	SignInMethodSignUp        = "sign_up"            // sign-up signed the user straight in
	SignInMethodEmailVerified = "email_verification" // the verification link signed the user in
	SignInMethodEmailChanged  = "email_change"       // the change-email link signed the user in
	SignInMethodSocial        = "social"             // Google or Apple (Details["provider"])
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
// worker, one event at a time in the order they happened, with a fresh
// context (no request values, so nothing like a session token reaches it)
// that expires after 5s. It cannot block or fail auth: events wait in a
// bounded queue (1024) and are dropped with a warning when it is full, a
// panic is recovered and logged, and emitting never waits, even while
// Shutdown is draining or when the handler itself calls back into Auth.
// Delivery is best-effort: events still queued when the process exits are
// lost, so call Auth.Shutdown on graceful shutdown.
type EventHandler func(ctx context.Context, event Event)

// eventSink delivers events to a handler in order on one worker goroutine.
type eventSink struct {
	handler EventHandler
	log     *slog.Logger
	queue   chan Event
	email   func(ctx context.Context, userID string) string // fills Event.Email when the emitter could not

	mu      sync.Mutex
	pending int           // queued or being delivered
	idle    chan struct{} // closed while pending == 0

	dropped atomic.Int64 // events refused because the queue was full (DroppedEvents)
}

// dropLogEvery bounds the drop warnings: the first drop and every 100th after
// it are logged with the running total, so a burst stays visible without
// flooding the log.
const dropLogEvery = 100

func newEventSink(handler EventHandler, log *slog.Logger, email func(context.Context, string) string) *eventSink {
	if handler == nil {
		return nil
	}
	idle := make(chan struct{})
	close(idle)
	sink := &eventSink{handler: handler, log: log, queue: make(chan Event, eventQueueSize), email: email, idle: idle}
	go sink.run()
	return sink
}

// emit queues ev and never waits: a full queue drops it.
func (sink *eventSink) emit(ctx context.Context, ev Event) {
	if sink == nil {
		return
	}
	sink.mu.Lock()
	select {
	case sink.queue <- ev:
		if sink.pending == 0 {
			sink.idle = make(chan struct{})
		}
		sink.pending++
		sink.mu.Unlock()
	default:
		sink.mu.Unlock()
		if count := sink.dropped.Add(1); count == 1 || count%dropLogEvery == 0 {
			sink.log.WarnContext(ctx, "auth: event queue full, event dropped", "kind", ev.Kind, "userId", ev.UserID, "dropped_total", count)
		}
	}
}

func (sink *eventSink) run() {
	for ev := range sink.queue {
		sink.deliver(ev)
		sink.mu.Lock()
		if sink.pending--; sink.pending == 0 {
			close(sink.idle)
		}
		sink.mu.Unlock()
	}
}

func (sink *eventSink) deliver(ev Event) {
	defer func() {
		if value := recover(); value != nil {
			sink.log.Error("auth: event handler panicked", "kind", ev.Kind, "panic", value, "stack", string(debug.Stack()))
		}
	}()
	if ev.Email == "" && ev.UserID != "" && sink.email != nil {
		lookup, cancel := context.WithTimeout(context.Background(), eventDeliveryTimeout)
		ev.Email = sink.email(lookup, ev.UserID)
		cancel()
	}
	ctx, cancel := context.WithTimeout(context.Background(), eventDeliveryTimeout)
	defer cancel()
	sink.handler(ctx, ev)
}

// droppedCount is the number of events dropped so far (0 without a sink).
func (sink *eventSink) droppedCount() int64 {
	if sink == nil {
		return 0
	}
	return sink.dropped.Load()
}

// drain waits until the queue is empty and the worker idle (or ctx ends). It
// never stops new events from being emitted meanwhile.
func (sink *eventSink) drain(ctx context.Context) error {
	if sink == nil {
		return nil
	}
	sink.mu.Lock()
	idle := sink.idle
	sink.mu.Unlock()
	select {
	case <-idle:
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
func (auth *Auth) emit(ctx context.Context, ev Event) {
	if auth.events == nil {
		return
	}
	ev.At = auth.now()
	if meta := auth.meta(ctx); meta != nil {
		ev.IP, ev.UserAgent = meta.ip, meta.userAgent
	}
	auth.events.emit(ctx, ev)
}

// DroppedEvents is how many events this instance has dropped because its queue
// (1024) was full: a slow or stuck Config.Events handler. It only grows; export
// it as a counter metric, or alert when it moves. Each drop is also logged
// (the first and every 100th, with the running total).
func (auth *Auth) DroppedEvents() int64 { return auth.events.droppedCount() }

// emitFailure reports a refused sign-in or second factor with the error code.
// Rate-limited attempts are not reported: they never reached a credential
// check, cost an attacker nothing, and would crowd real events out of the
// queue.
func (auth *Auth) emitFailure(ctx context.Context, kind EventKind, userID, method string, err error) {
	if errors.Is(err, ErrTooManyRequests) {
		return
	}
	code := "INTERNAL_ERROR"
	if authErr := asError(err); authErr != nil {
		code = authErr.Code
	}
	auth.emit(ctx, Event{Kind: kind, UserID: userID, Method: method, Reason: code})
}
