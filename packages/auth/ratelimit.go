package auth

import (
	"context"
	"sync"
	"time"
)

// RateRule allows Max requests per Window for one key.
type RateRule struct {
	Window time.Duration
	Max    int
}

// Better Auth's default rules, applied per client IP and endpoint.
var (
	RuleSignIn        = RateRule{Window: ruleWindow, Max: ruleMax}  // sign-in, sign-up
	RuleEmailSending  = RateRule{Window: time.Minute, Max: ruleMax} // request-password-reset, send-verification-email
	RuleResetPassword = RateRule{Window: ruleWindow, Max: ruleMax}  // reset-password (token guessing)
)

const (
	ruleWindow = 10 * time.Second // Better Auth's window for sign-in and reset-password
	ruleMax    = 3                // requests per window in every default rule
)

// RateLimiter decides whether a request identified by key may proceed. Plug
// in a shared store (Valkey, Postgres) when running several instances.
type RateLimiter interface {
	// Allow records one request for key and returns 0 when it is allowed, or
	// how long to wait before retrying when it is not.
	Allow(ctx context.Context, key string, rule RateRule) (retryAfter time.Duration, err error)
}

// MemoryRateLimiter is a fixed-window in-process RateLimiter. Each instance
// counts separately, which is fine for a single server.
type MemoryRateLimiter struct {
	mu        sync.Mutex
	windows   map[string]rateWindow
	now       func() time.Time
	lastSweep time.Time
}

type rateWindow struct {
	start time.Time
	count int
	ttl   time.Duration
}

// NewMemoryRateLimiter returns an empty in-memory limiter.
func NewMemoryRateLimiter() *MemoryRateLimiter {
	return &MemoryRateLimiter{windows: map[string]rateWindow{}, now: time.Now}
}

// Allow implements RateLimiter.
func (m *MemoryRateLimiter) Allow(_ context.Context, key string, rule RateRule) (time.Duration, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := m.now()
	if now.Sub(m.lastSweep) > time.Minute {
		for k, w := range m.windows {
			if now.Sub(w.start) >= w.ttl {
				delete(m.windows, k)
			}
		}
		m.lastSweep = now
	}
	w, ok := m.windows[key]
	if !ok || now.Sub(w.start) >= rule.Window {
		m.windows[key] = rateWindow{start: now, count: 1, ttl: rule.Window}
		return 0, nil
	}
	if w.count >= rule.Max {
		return w.start.Add(rule.Window).Sub(now), nil
	}
	w.count++
	m.windows[key] = w
	return 0, nil
}

// rateLimit applies rule to the client IP of the current request. Requests
// without request metadata (Middleware not applied) are not limited.
func (a *Auth) rateLimit(ctx context.Context, endpoint string, rule RateRule) error {
	meta := a.meta(ctx)
	if meta == nil || meta.ip == "" {
		return nil
	}
	retry, err := a.limiter.Allow(ctx, meta.ip+"|"+endpoint, rule)
	if err != nil {
		// A broken limiter backend must not lock everyone out.
		a.log.ErrorContext(ctx, "auth: rate limiter failed", "err", err)
		return nil
	}
	if retry > 0 {
		return &RateLimitError{RetryAfter: retry}
	}
	return nil
}
