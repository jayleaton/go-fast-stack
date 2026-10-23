// Package config loads the admin app's environment config, validated once at
// boot, plus its routes config. Nothing else in apps/admin reads os.Getenv.
// Secrets come through Secrets (packages/secrets) and rotate live.
//
// The admin app has two databases: its own (ADMIN_DATABASE_URL: admins,
// their sessions, the audit log) and the main app's (DATABASE_URL: the users
// it administers). Admins are never rows in the app's "user" table.
package config

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/jayleaton/go-fast-stack/packages/secrets"
)

// Env is the deployment environment (APP_ENV), shared with the web app.
type Env string

const (
	Development Env = "development"
	Test        Env = "test"
	Production  Env = "production"
)

// Config is the validated environment.
type Config struct {
	Env            Env
	Addr           string   // listen address, always 0.0.0.0:ADMIN_PORT (or PORT)
	PublicURL      *url.URL // ADMIN_PUBLIC_URL: origin admins reach the dashboard on (invite links, auth callbacks)
	TrustedOrigins []string // PublicURL plus ADMIN_TRUSTED_ORIGINS
	// Secrets: DATABASE_URL (the app database), ADMIN_DATABASE_URL (admins,
	// audit log), the ADMIN_AUTH_SECRET keyring (never the app's AUTH_SECRET)
	// and RESEND_API_KEY, from SECRETS_PROVIDER; they rotate live.
	Secrets Secrets
	// AuthSecretGrace is ADMIN_AUTH_SECRET_GRACE: how long a retired
	// ADMIN_AUTH_SECRET is accepted (0: packages/auth default).
	AuthSecretGrace time.Duration
	// BootstrapEmail is seeded as the first admin (without a password) by
	// `admin migrate`, which also sends its set-password link while it is
	// pending, at most once per link lifetime (72 h).
	BootstrapEmail string
	// RequireTwoFactor makes every admin enrol in TOTP before getting a
	// session. Defaults to on in production.
	RequireTwoFactor bool
	AppName          string // shown in the header and as the TOTP issuer
	// AuditReconcileAfter is how old a pending audit entry (an admin change
	// whose outcome was never written) must be before the reconciler
	// resolves it from the current state.
	AuditReconcileAfter time.Duration
	FromEmail           string
	ClientIPHeader      string // header the trusted proxy sets to the client IP; empty uses the TCP peer
}

// Load reads and validates config. getenv is os.Getenv outside tests.
// Every problem is reported at once so a bad deploy fails with the full list.
func Load(getenv func(string) string) (Config, error) {
	var errs []error
	get := func(key, fallback string) string {
		if value := strings.TrimSpace(getenv(key)); value != "" {
			return value
		}
		return fallback
	}

	cfg := Config{
		Env:            Env(get("APP_ENV", string(Development))),
		BootstrapEmail: strings.ToLower(get("ADMIN_BOOTSTRAP_EMAIL", "")),
		AppName:        get("ADMIN_APP_NAME", "Admin"),
		FromEmail:      get("FROM_EMAIL", "noreply@example.com"),
		ClientIPHeader: get("CLIENT_IP_HEADER", ""),
	}
	var perr error
	if cfg.AuditReconcileAfter, perr = time.ParseDuration(get("ADMIN_AUDIT_RECONCILE_AFTER", "2m")); perr != nil || cfg.AuditReconcileAfter < 10*time.Second {
		errs = append(errs, fmt.Errorf("ADMIN_AUDIT_RECONCILE_AFTER must be a duration of at least 10s, got %q", get("ADMIN_AUDIT_RECONCILE_AFTER", "")))
	}
	switch cfg.Env {
	case Development, Test, Production:
	default:
		errs = append(errs, fmt.Errorf("APP_ENV must be development, test or production, got %q", cfg.Env))
	}

	// ADMIN_PORT first: locally .env sets PORT for the web app. Railway sets
	// PORT per service.
	cfg.Addr = net.JoinHostPort("0.0.0.0", get("ADMIN_PORT", get("PORT", "3001")))

	var err error
	if cfg.PublicURL, err = parseOrigin("ADMIN_PUBLIC_URL", get("ADMIN_PUBLIC_URL", "")); err != nil {
		errs = append(errs, err)
	} else {
		cfg.TrustedOrigins = append(cfg.TrustedOrigins, cfg.PublicURL.String())
	}
	for key := range strings.SplitSeq(get("ADMIN_TRUSTED_ORIGINS", ""), ",") {
		if key = strings.TrimSpace(key); key != "" {
			cfg.TrustedOrigins = append(cfg.TrustedOrigins, key)
		}
	}

	if cfg.RequireTwoFactor, err = parseBool("ADMIN_REQUIRE_2FA", get("ADMIN_REQUIRE_2FA", ""), cfg.Env == Production); err != nil {
		errs = append(errs, err)
	}

	cfg.Secrets = loadSecrets(cfg.Env, get, &errs)
	if cfg.AuthSecretGrace, err = secrets.ParseDuration("ADMIN_AUTH_SECRET_GRACE", get("ADMIN_AUTH_SECRET_GRACE", ""), 0); err != nil {
		errs = append(errs, err)
	}
	if cfg.BootstrapEmail != "" && !strings.Contains(cfg.BootstrapEmail, "@") {
		errs = append(errs, fmt.Errorf("ADMIN_BOOTSTRAP_EMAIL must be an email address, got %q", cfg.BootstrapEmail))
	}

	if len(errs) > 0 {
		return Config{}, fmt.Errorf("invalid admin config: %w", errors.Join(errs...))
	}
	return cfg, nil
}

func parseOrigin(key, raw string) (*url.URL, error) {
	if raw == "" {
		return nil, fmt.Errorf("%s is required (e.g. http://<host>:3001)", key)
	}
	link, err := url.Parse(strings.TrimRight(raw, "/"))
	if err != nil || (link.Scheme != "http" && link.Scheme != "https") || link.Host == "" || link.Path != "" {
		return nil, fmt.Errorf("%s must be an http(s) origin without a path, got %q", key, raw)
	}
	return link, nil
}

func parseBool(key, raw string, fallback bool) (bool, error) {
	if raw == "" {
		return fallback, nil
	}
	flag, err := strconv.ParseBool(raw)
	if err != nil {
		return false, fmt.Errorf("%s must be true or false, got %q", key, raw)
	}
	return flag, nil
}
