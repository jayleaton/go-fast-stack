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
	"time"

	"github.com/jayleaton/go-fast-stack/packages/secrets"
)

// Env is the deployment environment (APP_ENV).
type Env string

const (
	Development Env = "development"
	Test        Env = "test"
	Production  Env = "production"
)

// DefaultAppName is the product name when APP_NAME is unset.
const DefaultAppName = "My App"

// Config is the validated environment.
type Config struct {
	Env             Env
	AppName         string        // APP_NAME: the product name in the UI header and page titles
	Addr            string        // listen address, always 0.0.0.0:PORT
	PublicURL       *url.URL      // origin people reach the app on (emails, auth callbacks)
	InternalURL     *url.URL      // origin the server uses to reach itself
	TrustedOrigins  []string      // PublicURL plus TRUSTED_ORIGINS
	Secrets         Secrets       // live secrets (DATABASE_URL, AUTH_SECRET[_PREVIOUS], RESEND_API_KEY, STORAGE_S3_*) from SECRETS_PROVIDER
	AuthSecretGrace time.Duration // AUTH_SECRET_GRACE: how long a retired AUTH_SECRET is accepted (0: packages/auth default)
	FromEmail       string
	ClientIPHeader  string         // header the trusted proxy sets to the client IP (auth rate limits); empty uses the TCP peer
	Social          Social         // Google / Apple sign-in, each off until its client id is set
	Storage         StorageOptions // object storage region and addressing; the bucket and keys are live secrets (Secrets.Storage)
	Features        Features
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
		Env:       Env(get("APP_ENV", string(Development))),
		AppName:   get("APP_NAME", DefaultAppName),
		FromEmail: get("FROM_EMAIL", "noreply@example.com"),
		// Only set behind a proxy that overwrites/appends it (Railway: X-Real-IP),
		// or clients could pick their own rate-limit bucket.
		ClientIPHeader: get("CLIENT_IP_HEADER", ""),
	}
	switch cfg.Env {
	case Development, Test, Production:
	default:
		errs = append(errs, fmt.Errorf("APP_ENV must be development, test or production, got %q", cfg.Env))
	}
	// Unset, FROM_EMAIL falls back to the example sender, which no mail
	// provider will send from: every auth email would fail, only in the log.
	if cfg.Env == Production {
		switch {
		case get("FROM_EMAIL", "") == "":
			errs = append(errs, errors.New("FROM_EMAIL is required in production (a sender on your verified email domain)"))
		case !realSender(cfg.FromEmail):
			errs = append(errs, fmt.Errorf("FROM_EMAIL must be a sender on your verified email domain in production, got %q", cfg.FromEmail))
		}
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
	for key := range strings.SplitSeq(get("TRUSTED_ORIGINS", ""), ",") {
		if key = strings.TrimSpace(key); key != "" {
			cfg.TrustedOrigins = append(cfg.TrustedOrigins, key)
		}
	}

	if cfg.AuthSecretGrace, err = secrets.ParseDuration("AUTH_SECRET_GRACE", get("AUTH_SECRET_GRACE", ""), 0); err != nil {
		errs = append(errs, err)
	}
	cfg.Social = loadSocial(get, &errs)
	cfg.Storage = loadStorageOptions(get, &errs)
	cfg.Secrets = loadSecrets(cfg.Env, cfg.Social, get, &errs)

	cfg.Features = FeaturesFor(cfg.Env)
	if len(errs) > 0 {
		return Config{}, fmt.Errorf("invalid config: %w", errors.Join(errs...))
	}
	return cfg, nil
}

// realSender reports whether addr ("a@b.c" or "Name <a@b.c>") is an email
// address off the reserved example and test domains (RFC 2606, RFC 6761).
func realSender(addr string) bool {
	addr = strings.TrimSuffix(strings.TrimSpace(addr), ">")
	at := strings.LastIndex(addr, "@")
	if at <= 0 || at == len(addr)-1 {
		return false
	}
	domain := strings.TrimSuffix(strings.ToLower(addr[at+1:]), ".")
	for _, reserved := range []string{"example.com", "example.net", "example.org", "example", "test", "invalid", "localhost"} {
		if domain == reserved || strings.HasSuffix(domain, "."+reserved) {
			return false
		}
	}
	return true
}

func parseOrigin(key, raw string) (*url.URL, error) {
	if raw == "" {
		return nil, fmt.Errorf("%s is required (e.g. http://<host>:3000)", key)
	}
	link, err := url.Parse(strings.TrimRight(raw, "/"))
	if err != nil || (link.Scheme != "http" && link.Scheme != "https") || link.Host == "" || link.Path != "" {
		return nil, fmt.Errorf("%s must be an http(s) origin without a path, got %q", key, raw)
	}
	return link, nil
}
