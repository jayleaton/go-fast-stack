package secrets

import (
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"
)

// SECRETS_PROVIDER values.
const (
	ProviderEnv     = "env"     // values from the process env, read once at boot (default)
	ProviderDoppler = "doppler" // values polled from one Doppler config; rotate without a restart
)

// MinRefreshInterval is the shortest SECRETS_REFRESH_INTERVAL Bootstrap
// accepts (Doppler Developer allows 120 reads/minute per workspace).
const MinRefreshInterval = 10 * time.Second

// BootstrapOptions configure Bootstrap.
type BootstrapOptions struct {
	Keys       []string                                   // names served by the provider (Options.Keys)
	Validate   func(get func(name string) string) []error // checks one snapshot or the boot env; messages must not hold values
	Production bool                                       // DOPPLER_API_URL must be https
}

// Bootstrap builds a Manager from SECRETS_PROVIDER and its bootstrap vars
// (DOPPLER_TOKEN, DOPPLER_API_URL, SECRETS_REFRESH_INTERVAL,
// SECRETS_MAX_STALENESS), read through get: an app's config.Load getter,
// trimmed, "" when unset. This package never reads the environment itself.
// It does no I/O. Every problem is returned at once; with the env provider
// that includes Validate's findings on the boot values.
func Bootstrap(get func(key string) string, options BootstrapOptions) (*Manager, []error) {
	var errs []error
	validate := func(get func(string) string) []error {
		if options.Validate == nil {
			return nil
		}
		return options.Validate(get)
	}
	opts := Options{
		Keys: options.Keys,
		Validate: func(snapshot Snapshot) error {
			return errors.Join(validate(func(name string) string { secret, _ := snapshot.Get(name); return strings.TrimSpace(secret.value) })...)
		},
	}
	var err error
	if opts.Interval, err = ParseDuration("SECRETS_REFRESH_INTERVAL", get("SECRETS_REFRESH_INTERVAL"), DefaultInterval); err != nil {
		errs = append(errs, err)
	} else if opts.Interval < MinRefreshInterval {
		errs = append(errs, fmt.Errorf("SECRETS_REFRESH_INTERVAL must be at least %s, got %s", MinRefreshInterval, opts.Interval))
	}
	if opts.MaxStaleness, err = ParseDuration("SECRETS_MAX_STALENESS", get("SECRETS_MAX_STALENESS"), DefaultMaxStaleness); err != nil {
		errs = append(errs, err)
	} else if opts.MaxStaleness < 2*opts.Interval {
		errs = append(errs, fmt.Errorf("SECRETS_MAX_STALENESS must be at least twice SECRETS_REFRESH_INTERVAL, got %s", opts.MaxStaleness))
	}

	var provider Provider
	name := get("SECRETS_PROVIDER")
	switch name {
	case "", ProviderEnv:
		values := map[string]string{}
		for _, key := range options.Keys {
			values[key] = get(key)
		}
		errs = append(errs, validate(func(name string) string { return values[name] })...)
		provider = NewEnv(values)
	case ProviderDoppler:
		doppler := NewDoppler(NewSecret(get("DOPPLER_TOKEN")))
		if doppler.Token.IsZero() {
			errs = append(errs, errors.New("DOPPLER_TOKEN is required when SECRETS_PROVIDER=doppler (a read-only service token for this service's config)"))
		}
		if value := get("DOPPLER_API_URL"); value != "" {
			doppler.APIURL = value
		}
		if link, err := url.Parse(doppler.APIURL); err != nil || (link.Scheme != "https" && link.Scheme != "http") || link.Host == "" || link.User != nil {
			errs = append(errs, errors.New("DOPPLER_API_URL must be an http(s) origin without credentials")) // value not echoed: it may hold some
		} else if options.Production && link.Scheme != "https" {
			errs = append(errs, errors.New("DOPPLER_API_URL must use https in production"))
		}
		provider = doppler
	default:
		errs = append(errs, fmt.Errorf("SECRETS_PROVIDER must be %s or %s, got %q", ProviderEnv, ProviderDoppler, name))
		provider = NewEnv(nil)
	}
	return NewManager(provider, opts), errs
}

// ParseDuration reads an optional positive duration setting for an app's
// config ("" is fallback); the error names key and the raw value.
func ParseDuration(key, raw string, fallback time.Duration) (time.Duration, error) {
	if raw == "" {
		return fallback, nil
	}
	duration, err := time.ParseDuration(strings.TrimSpace(raw))
	if err != nil || duration <= 0 {
		return fallback, fmt.Errorf("%s must be a positive duration like 60s or 15m, got %q", key, raw)
	}
	return duration, nil
}
