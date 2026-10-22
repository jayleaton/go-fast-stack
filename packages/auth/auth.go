// Package auth is a Go-native port of the Better Auth core: email + password
// accounts, email verification, password reset and database sessions, using
// Better Auth's table names, endpoint paths, JSON shapes and cookie format.
//
// Wire it up once at startup:
//
//	if err := auth.Migrate(ctx, pool); err != nil { ... }
//	a, err := auth.New(cfg, pool, mailer)
//	mux.Handle("/api/auth/", a.Handler())
//	handler := a.Middleware(mux)
//
// HTML handlers call the service methods (SignInEmail, SignUpEmail, ...)
// directly and set the cookie with SetSessionCookie.
package auth

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// BasePath is where Handler serves the JSON endpoints.
const BasePath = "/api/auth"

// Defaults mirror Better Auth's.
const (
	DefaultSessionTTL        = 7 * 24 * time.Hour
	DefaultSessionUpdateAge  = 24 * time.Hour
	DefaultFreshAge          = 24 * time.Hour
	DefaultInviteTTL         = 72 * time.Hour
	DefaultTrustDeviceTTL    = 30 * 24 * time.Hour
	DefaultMinPasswordLength = 8
	DefaultMaxPasswordLength = 128
	// VerificationTokenTTL is how long an email verification link stays valid.
	VerificationTokenTTL = 24 * time.Hour
	// ResetPasswordTokenTTL is how long a password reset link stays valid.
	ResetPasswordTokenTTL = time.Hour
)

// Config configures an Auth. Zero values fall back to the defaults above.
type Config struct {
	Secret                   string        // AUTH_SECRET: signs cookies and verification tokens, at least 32 bytes
	BaseURL                  string        // public origin used in email links and as a trusted origin, e.g. https://app.example.com
	RequireEmailVerification bool          // block sign-in until the email is verified and send a link on sign-up
	SessionTTL               time.Duration // session lifetime, refreshed on use (default 7 days)
	SessionUpdateAge         time.Duration // how often a used session is extended (default 1 day)
	TrustedOrigins           []string      // extra origins allowed to call state-changing endpoints, incl. mobile schemes (see MobileTrustedOrigins)

	FreshAge          time.Duration // how recent a session must be to change the email (default 1 day)
	MinPasswordLength int           // default 8
	MaxPasswordLength int           // default 128
	LoginPath         string        // where RequireUser redirects HTML requests (default "/login")
	RateLimiter       RateLimiter   // limits sign-in/sign-up/reset per client IP (default in-memory, Better Auth's rules)
	ClientIPHeader    string        // header set by the trusted proxy carrying the client IP (e.g. "X-Forwarded-For"); empty uses the TCP peer
	Logger            *slog.Logger  // default slog.Default()
	MailTimeout       time.Duration // timeout for sending one email (default 30s)

	CookiePrefix string           // cookie name prefix (default "better-auth"): a second instance in the same browser needs its own
	AppName      string           // TOTP issuer default (default: BaseURL host)
	TwoFactor    *TwoFactorConfig // nil turns 2FA off: endpoints answer 404, sign-in is unchanged
	InviteTTL    time.Duration    // how long a set-password (invite) link stays valid (default 72h)
}

// MobileTrustedOrigins returns the deep-link origins of the Expo app: its
// scheme always, plus Expo Go / dev-client wildcards outside production.
func MobileTrustedOrigins(production bool) []string {
	origins := []string{"betterstack://"}
	if !production {
		origins = append(origins, "exp://**", "exp+go://**")
	}
	return origins
}

// Mailer delivers the auth emails. url is the absolute link to put in the
// email. Use NewDevMailer in development and NewResendMailer in production.
type Mailer interface {
	SendVerificationEmail(ctx context.Context, to, url string) error
	SendPasswordResetEmail(ctx context.Context, to, url string) error
	// SendEmailChangeNotice tells the current address that a change to
	// newEmail was requested (the confirmation link goes to newEmail).
	SendEmailChangeNotice(ctx context.Context, to, newEmail string) error
	// SendSetPasswordEmail invites to to choose a password (single-use link).
	SendSetPasswordEmail(ctx context.Context, to, url string) error
}

// User is a row of the "user" table, serialised like Better Auth's user object.
type User struct {
	ID            string    `json:"id"`
	Name          string    `json:"name"`
	Email         string    `json:"email"`
	EmailVerified bool      `json:"emailVerified"`
	Image         *string   `json:"image"`
	CreatedAt     time.Time `json:"createdAt"`
	UpdatedAt     time.Time `json:"updatedAt"`
}

// Session is a row of the "session" table. Token is the bearer credential.
type Session struct {
	ID        string    `json:"id"`
	ExpiresAt time.Time `json:"expiresAt"`
	Token     string    `json:"token,omitempty"` // empty in ListSessions: other sessions' tokens are never exposed
	CreatedAt time.Time `json:"createdAt"`
	UpdatedAt time.Time `json:"updatedAt"`
	IPAddress *string   `json:"ipAddress"`
	UserAgent *string   `json:"userAgent"`
	UserID    string    `json:"userId"`
}

// Auth is the configured auth service. Safe for concurrent use.
type Auth struct {
	cfg        Config
	store      *store
	mailer     Mailer
	log        *slog.Logger
	limiter    RateLimiter
	baseOrigin string
	secure     bool
	trusted    []string
	argon      argonParams
	dummyHash  string
	now        func() time.Time
	mail       sync.WaitGroup
	mailMu     sync.RWMutex // sendMail holds it shared so Wait never races an Add
}

// New validates cfg and returns an Auth backed by pool. Run Migrate first.
func New(cfg Config, pool *pgxpool.Pool, mailer Mailer) (*Auth, error) {
	if len(cfg.Secret) < 32 {
		return nil, errors.New("auth: Secret must be at least 32 bytes")
	}
	if pool == nil {
		return nil, errors.New("auth: pool is required")
	}
	if mailer == nil {
		return nil, errors.New("auth: mailer is required")
	}
	base, err := url.Parse(strings.TrimRight(cfg.BaseURL, "/"))
	if err != nil || (base.Scheme != "http" && base.Scheme != "https") || base.Host == "" {
		return nil, fmt.Errorf("auth: BaseURL must be an absolute http(s) URL, got %q", cfg.BaseURL)
	}
	if cfg.SessionTTL <= 0 {
		cfg.SessionTTL = DefaultSessionTTL
	}
	if cfg.SessionUpdateAge <= 0 {
		cfg.SessionUpdateAge = DefaultSessionUpdateAge
	}
	if cfg.FreshAge <= 0 {
		cfg.FreshAge = DefaultFreshAge
	}
	if cfg.MinPasswordLength <= 0 {
		cfg.MinPasswordLength = DefaultMinPasswordLength
	}
	if cfg.MaxPasswordLength <= 0 {
		cfg.MaxPasswordLength = DefaultMaxPasswordLength
	}
	if cfg.LoginPath == "" {
		cfg.LoginPath = "/login"
	}
	if cfg.RateLimiter == nil {
		cfg.RateLimiter = NewMemoryRateLimiter()
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.MailTimeout <= 0 {
		cfg.MailTimeout = 30 * time.Second
	}
	if cfg.CookiePrefix == "" {
		cfg.CookiePrefix = "better-auth"
	}
	if strings.ContainsAny(cfg.CookiePrefix, " ;,=\"\t") {
		return nil, fmt.Errorf("auth: CookiePrefix %q is not a valid cookie name", cfg.CookiePrefix)
	}
	if cfg.AppName == "" {
		cfg.AppName = base.Hostname()
	}
	if cfg.InviteTTL <= 0 {
		cfg.InviteTTL = DefaultInviteTTL
	}
	if tf := cfg.TwoFactor; tf != nil {
		copied := *tf
		if copied.Issuer == "" {
			copied.Issuer = cfg.AppName
		}
		if copied.TrustDeviceTTL == 0 {
			copied.TrustDeviceTTL = DefaultTrustDeviceTTL
		}
		cfg.TwoFactor = &copied
	}

	a := &Auth{
		cfg:        cfg,
		store:      &store{pool: pool},
		mailer:     mailer,
		log:        cfg.Logger,
		limiter:    cfg.RateLimiter,
		baseOrigin: originOf(base),
		secure:     base.Scheme == "https",
		argon:      defaultArgonParams,
		now:        time.Now,
	}
	a.trusted = append([]string{a.baseOrigin}, cfg.TrustedOrigins...)
	if a.dummyHash, err = hashPassword("dummy-password-for-timing", a.argon); err != nil {
		return nil, err
	}
	return a, nil
}

// Shutdown waits for emails still being sent in the background, or for ctx.
// Requests arriving meanwhile block before queueing mail until it returns.
func (a *Auth) Shutdown(ctx context.Context) error {
	done := make(chan struct{})
	go func() {
		a.mailMu.Lock()
		a.mail.Wait()
		a.mailMu.Unlock()
		close(done)
	}()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// sendMail sends in the background so responses take the same time whether
// or not an email went out (no account enumeration by timing).
func (a *Auth) sendMail(ctx context.Context, kind, to string, send func(ctx context.Context) error) {
	a.mailMu.RLock()
	defer a.mailMu.RUnlock()
	a.mail.Go(func() {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), a.cfg.MailTimeout)
		defer cancel()
		if err := send(ctx); err != nil {
			a.log.ErrorContext(ctx, "auth: sending email failed", "kind", kind, "to", to, "err", err)
		}
	})
}

// authURL returns an absolute URL under BasePath, e.g. authURL("/verify-email").
func (a *Auth) authURL(path string) string {
	return a.baseOrigin + BasePath + path
}
