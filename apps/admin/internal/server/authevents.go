package server

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/jayleaton/ai-patterns-go/apps/admin/internal/services"
	"github.com/jayleaton/ai-patterns-go/packages/auth"
)

// AuthEvents audits admin authentication around the admin auth instance,
// whichever way it is used: the UI (through UIAuth) and the JSON auth
// endpoints (Handler). packages/auth has no event hooks, so it watches the
// outcomes:
//
//   - sign-in: a call or response that produced a session names its admin;
//   - sign-out: the caller is read before the session ends;
//   - failed 2FA: a wrong code does not name its admin (the challenge is
//     opaque), but auth bumps that admin's failure counter. Verifications are
//     serialized here, so after a failure the counter that moved since it
//     started is its own (services.AuthEventService.TwoFactorFailed). With
//     several admin instances two simultaneous failures could be attributed
//     to both; one instance (the template's Railway setup) is exact.
//
// An audit write that fails is logged at error level and does not undo the
// sign-in that already happened.
type AuthEvents struct {
	svc *services.AuthEventService
	log *slog.Logger
	now func() time.Time
	mu  sync.Mutex // serializes second-factor verifications
}

// NewAuthEvents returns the auditor over svc.
func NewAuthEvents(svc *services.AuthEventService, log *slog.Logger) *AuthEvents {
	return &AuthEvents{svc: svc, log: log, now: time.Now}
}

func (e *AuthEvents) signedIn(ctx context.Context, u auth.User, method string) {
	if e == nil || u.ID == "" {
		return
	}
	e.report(ctx, "sign-in", e.svc.SignedIn(ctx, u.ID, u.Email, method))
}

func (e *AuthEvents) signedOut(ctx context.Context, u auth.User) {
	if e == nil || u.ID == "" {
		return
	}
	e.report(ctx, "sign-out", e.svc.SignedOut(ctx, u.ID, u.Email))
}

// verify runs one second-factor check; failed reports whether it failed.
func (e *AuthEvents) verify(ctx context.Context, check func() (failed bool)) {
	if e == nil {
		check()
		return
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	// Postgres keeps microseconds; auth stamps the counter after this.
	since := e.now().Truncate(time.Microsecond)
	if check() {
		e.report(ctx, "failed two-factor", e.svc.TwoFactorFailed(ctx, since))
	}
}

func (e *AuthEvents) report(ctx context.Context, what string, err error) {
	if err != nil {
		e.log.ErrorContext(ctx, "admin: audit "+what+" not recorded", "err", err)
	}
}

// Handler audits the JSON auth endpoints the server mounts (sign-in, the
// 2FA steps, set-password, sign-out); other paths pass through untouched.
func (e *AuthEvents) Handler(next http.Handler) http.Handler {
	if e == nil {
		return next
	}
	signIn := map[string]string{
		auth.BasePath + "/sign-in/email":                 services.SignInPassword,
		auth.BasePath + "/set-password":                  services.SignInSetPassword,
		auth.BasePath + "/two-factor/verify-totp":        services.SignInTwoFactor,
		auth.BasePath + "/two-factor/verify-backup-code": services.SignInTwoFactor,
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			next.ServeHTTP(w, r)
			return
		}
		ctx := r.Context()
		if r.URL.Path == auth.BasePath+"/sign-out" {
			u, _, ok := auth.UserFrom(ctx)
			rec := &teeRecorder{ResponseWriter: w, status: http.StatusOK}
			next.ServeHTTP(rec, r)
			if ok && rec.status == http.StatusOK {
				e.signedOut(ctx, u)
			}
			return
		}
		method, ok := signIn[r.URL.Path]
		if !ok {
			next.ServeHTTP(w, r)
			return
		}
		rec := &teeRecorder{ResponseWriter: w, status: http.StatusOK}
		if method == services.SignInTwoFactor {
			e.verify(ctx, func() bool {
				next.ServeHTTP(rec, r)
				return rec.status != http.StatusOK
			})
		} else {
			next.ServeHTTP(rec, r)
		}
		if u, ok := rec.session(); ok {
			e.signedIn(ctx, u, method)
		}
	})
}

// teeRecorder passes the response through and keeps a copy of a small JSON
// body, to read who a new session belongs to.
type teeRecorder struct {
	http.ResponseWriter
	status int
	body   bytes.Buffer
}

// maxTee bounds the copy: auth's session responses are a few hundred bytes.
const maxTee = 64 << 10

func (t *teeRecorder) WriteHeader(code int) {
	t.status = code
	t.ResponseWriter.WriteHeader(code)
}

func (t *teeRecorder) Write(b []byte) (int, error) {
	if t.body.Len()+len(b) <= maxTee {
		t.body.Write(b)
	}
	return t.ResponseWriter.Write(b)
}

// session reads Better Auth's session response {"token": ..., "user": {...}};
// a 2FA challenge ({"twoFactorRedirect": true}) is not a session.
func (t *teeRecorder) session() (auth.User, bool) {
	if t.status != http.StatusOK {
		return auth.User{}, false
	}
	var res struct {
		Token string    `json:"token"`
		User  auth.User `json:"user"`
	}
	if err := json.Unmarshal(t.body.Bytes(), &res); err != nil || res.Token == "" || res.User.ID == "" {
		return auth.User{}, false
	}
	return res.User, true
}
