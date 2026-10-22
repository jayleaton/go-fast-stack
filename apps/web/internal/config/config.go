// Package config loads environment config, validated once at boot, plus the
// routes config and feature toggles. Nothing else reads os.Getenv. Secrets
// that rotate while the server runs come through Secrets (packages/secrets),
// which Load builds from SECRETS_PROVIDER.
package config

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"strings"

	"github.com/jayleaton/ai-patterns-go/packages/secrets"
)

// Env is the deployment environment (APP_ENV).
type Env string

const (
	Development Env = "development"
	Test        Env = "test"
	Production  Env = "production"
)

// Config is the validated environment. Credentials are secrets.Secret, so the
// whole Config prints, logs and marshals without them; cmd/server reveals each
// one where it builds the client that needs it.
type Config struct {
	Env            Env
	Addr           string         // listen address, always 0.0.0.0:PORT
	PublicURL      *url.URL       // origin people reach the app on (emails, auth callbacks)
	InternalURL    *url.URL       // origin the server uses to reach itself
	TrustedOrigins []string       // PublicURL plus TRUSTED_ORIGINS
	DatabaseURL    secrets.Secret // holds the database password; Reveal only in cmd/server
	AuthSecret     secrets.Secret // signs cookies and tokens; Reveal only in cmd/server
	Secrets        Secrets        // live secrets (RESEND_API_KEY) from SECRETS_PROVIDER; started by serve
	FromEmail      string
	ClientIPHeader string // header the trusted proxy sets to the client IP (auth rate limits); empty uses the TCP peer
	Features       Features
}

// Load reads and validates config. getenv is os.Getenv outside tests.
// Every problem is reported at once so a bad deploy fails with the full list.
func Load(getenv func(string) string) (Config, error) {
	var errs []error
	get := func(key, fallback string) string {
		if v := strings.TrimSpace(getenv(key)); v != "" {
			return v
		}
		return fallback
	}

	cfg := Config{
		Env:         Env(get("APP_ENV", string(Development))),
		DatabaseURL: secrets.NewSecret(get("DATABASE_URL", "")),
		AuthSecret:  secrets.NewSecret(get("AUTH_SECRET", "")),
		FromEmail:   get("FROM_EMAIL", "noreply@example.com"),
		// Only set behind a proxy that overwrites/appends it (Railway: X-Real-IP),
		// or clients could pick their own rate-limit bucket.
		ClientIPHeader: get("CLIENT_IP_HEADER", ""),
	}
	switch cfg.Env {
	case Development, Test, Production:
	default:
		errs = append(errs, fmt.Errorf("APP_ENV must be development, test or production, got %q", cfg.Env))
	}

	port := get("PORT", "3000")
	cfg.Addr = net.JoinHostPort("0.0.0.0", port)

	var err error
	if cfg.PublicURL, err = parseOrigin("PUBLIC_URL", get("PUBLIC_URL", "")); err != nil {
		errs = append(errs, err)
	}
	internal := get("INTERNAL_URL", get("PUBLIC_URL", ""))
	if cfg.InternalURL, err = parseOrigin("INTERNAL_URL", internal); err != nil {
		errs = append(errs, err)
	}
	if cfg.PublicURL != nil {
		cfg.TrustedOrigins = append(cfg.TrustedOrigins, cfg.PublicURL.String())
	}
	for o := range strings.SplitSeq(get("TRUSTED_ORIGINS", ""), ",") {
		if o = strings.TrimSpace(o); o != "" {
			cfg.TrustedOrigins = append(cfg.TrustedOrigins, o)
		}
	}

	if cfg.DatabaseURL.IsZero() {
		errs = append(errs, errors.New("DATABASE_URL is required"))
	}
	if cfg.AuthSecret.Len() < 32 {
		errs = append(errs, errors.New("AUTH_SECRET must be at least 32 characters (openssl rand -base64 32)"))
	}
	cfg.Secrets = loadSecrets(cfg.Env, get, &errs)

	cfg.Features = FeaturesFor(cfg.Env)
	if len(errs) > 0 {
		return Config{}, fmt.Errorf("invalid config: %w", errors.Join(errs...))
	}
	return cfg, nil
}

func parseOrigin(key, raw string) (*url.URL, error) {
	if raw == "" {
		return nil, fmt.Errorf("%s is required (e.g. http://<host>:3000)", key)
	}
	u, err := url.Parse(strings.TrimRight(raw, "/"))
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.Path != "" {
		return nil, fmt.Errorf("%s must be an http(s) origin without a path, got %q", key, raw)
	}
	return u, nil
}
