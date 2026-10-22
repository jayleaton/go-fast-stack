package config

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/jayleaton/ai-patterns-go/packages/secrets"
)

// SECRETS_PROVIDER values.
const (
	SecretsEnv     = "env"     // values from the process env, read once at boot (default)
	SecretsDoppler = "doppler" // values polled from one Doppler config; rotate without a restart
)

// Live secrets: names served by the secrets provider instead of the env, so
// they rotate without a redeploy when the provider is remote. Each gets a
// typed accessor on Secrets. To add one, see AGENTS.md "Adding a secret".
const secretResendAPIKey = "RESEND_API_KEY"

var liveSecrets = []string{secretResendAPIKey}

const minRefreshInterval = 10 * time.Second // Doppler Developer allows 120 reads/minute per workspace

// Secrets hands live secrets to the server through typed accessors; nothing
// else reads them. Load builds it (no I/O); serve calls Start, which fetches
// the first snapshot, and Stop on shutdown. `server migrate` never starts it.
type Secrets struct{ m *secrets.Manager }

// Start fetches and validates the first snapshot (failing boot without one),
// then polls until ctx ends or Stop is called.
func (s Secrets) Start(ctx context.Context) error { return s.m.Start(ctx) }

// Stop ends polling. Values already handed out keep working.
func (s Secrets) Stop() { s.m.Stop() }

// Healthy reports secrets.ErrNotLoaded or secrets.ErrStale (server /healthz).
func (s Secrets) Healthy() error { return s.m.Healthy() }

// Status is for logs and metrics; it holds no values.
func (s Secrets) Status() secrets.Status { return s.m.Status() }

// Provider is SECRETS_PROVIDER.
func (s Secrets) Provider() string { return s.m.Provider() }

// OnResendAPIKey calls fn now with RESEND_API_KEY (a zero Secret when unset)
// and again whenever it changes upstream. fn rebuilds the Resend client.
func (s Secrets) OnResendAPIKey(fn secrets.Hook) { s.m.OnChange(secretResendAPIKey, fn) }

// loadSecrets builds the Manager from SECRETS_PROVIDER and its bootstrap
// vars. Every problem is appended to errs.
func loadSecrets(env Env, get func(key, fallback string) string, errs *[]error) Secrets {
	opts := secrets.Options{Keys: liveSecrets}
	if env == Production {
		opts.Required = []string{secretResendAPIKey}
	}
	var err error
	if opts.Interval, err = parseDuration("SECRETS_REFRESH_INTERVAL", get("SECRETS_REFRESH_INTERVAL", ""), secrets.DefaultInterval); err != nil {
		*errs = append(*errs, err)
	} else if opts.Interval < minRefreshInterval {
		*errs = append(*errs, fmt.Errorf("SECRETS_REFRESH_INTERVAL must be at least %s, got %s", minRefreshInterval, opts.Interval))
	}
	if opts.MaxStaleness, err = parseDuration("SECRETS_MAX_STALENESS", get("SECRETS_MAX_STALENESS", ""), secrets.DefaultMaxStaleness); err != nil {
		*errs = append(*errs, err)
	} else if opts.MaxStaleness < 2*opts.Interval {
		*errs = append(*errs, fmt.Errorf("SECRETS_MAX_STALENESS must be at least twice SECRETS_REFRESH_INTERVAL, got %s", opts.MaxStaleness))
	}

	var p secrets.Provider
	switch name := get("SECRETS_PROVIDER", SecretsEnv); name {
	case SecretsEnv:
		values := map[string]string{}
		for _, k := range liveSecrets {
			values[k] = get(k, "")
		}
		if env == Production && values[secretResendAPIKey] == "" {
			*errs = append(*errs, errors.New("RESEND_API_KEY is required in production"))
		}
		p = secrets.NewEnv(values)
	case SecretsDoppler:
		d := secrets.NewDoppler(secrets.NewSecret(get("DOPPLER_TOKEN", "")))
		if d.Token.IsZero() {
			*errs = append(*errs, errors.New("DOPPLER_TOKEN is required when SECRETS_PROVIDER=doppler (a read-only service token for this service's config)"))
		}
		d.APIURL = get("DOPPLER_API_URL", secrets.DefaultDopplerURL)
		if u, err := url.Parse(d.APIURL); err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" || u.User != nil {
			*errs = append(*errs, errors.New("DOPPLER_API_URL must be an http(s) origin without credentials")) // value not echoed: it may hold some
		} else if env == Production && u.Scheme != "https" {
			*errs = append(*errs, errors.New("DOPPLER_API_URL must use https in production"))
		}
		p = d
	default:
		*errs = append(*errs, fmt.Errorf("SECRETS_PROVIDER must be %s or %s, got %q", SecretsEnv, SecretsDoppler, name))
		p = secrets.NewEnv(nil)
	}
	return Secrets{secrets.NewManager(p, opts)}
}

func parseDuration(key, raw string, fallback time.Duration) (time.Duration, error) {
	if raw == "" {
		return fallback, nil
	}
	d, err := time.ParseDuration(strings.TrimSpace(raw))
	if err != nil || d <= 0 {
		return fallback, fmt.Errorf("%s must be a positive duration like 60s or 15m, got %q", key, raw)
	}
	return d, nil
}
