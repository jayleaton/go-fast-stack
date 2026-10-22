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
func Bootstrap(get func(key string) string, o BootstrapOptions) (*Manager, []error) {
	var errs []error
	validate := func(get func(string) string) []error {
		if o.Validate == nil {
			return nil
		}
		return o.Validate(get)
	}
	opts := Options{
		Keys: o.Keys,
		Validate: func(s Snapshot) error {
			return errors.Join(validate(func(n string) string { v, _ := s.Get(n); return strings.TrimSpace(v.Reveal()) })...)
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

	var p Provider
	name := get("SECRETS_PROVIDER")
	switch name {
	case "", ProviderEnv:
		values := map[string]string{}
		for _, k := range o.Keys {
			values[k] = get(k)
		}
		errs = append(errs, validate(func(n string) string { return values[n] })...)
		p = NewEnv(values)
	case ProviderDoppler:
		d := NewDoppler(NewSecret(get("DOPPLER_TOKEN")))
		if d.Token.IsZero() {
			errs = append(errs, errors.New("DOPPLER_TOKEN is required when SECRETS_PROVIDER=doppler (a read-only service token for this service's config)"))
		}
		if v := get("DOPPLER_API_URL"); v != "" {
			d.APIURL = v
		}
		if u, err := url.Parse(d.APIURL); err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" || u.User != nil {
			errs = append(errs, errors.New("DOPPLER_API_URL must be an http(s) origin without credentials")) // value not echoed: it may hold some
		} else if o.Production && u.Scheme != "https" {
			errs = append(errs, errors.New("DOPPLER_API_URL must use https in production"))
		}
		p = d
	default:
		errs = append(errs, fmt.Errorf("SECRETS_PROVIDER must be %s or %s, got %q", ProviderEnv, ProviderDoppler, name))
		p = NewEnv(nil)
	}
	return NewManager(p, opts), errs
}

// ParseDuration reads an optional positive duration setting for an app's
// config ("" is fallback); the error names key and the raw value.
func ParseDuration(key, raw string, fallback time.Duration) (time.Duration, error) {
	if raw == "" {
		return fallback, nil
	}
	d, err := time.ParseDuration(strings.TrimSpace(raw))
	if err != nil || d <= 0 {
		return fallback, fmt.Errorf("%s must be a positive duration like 60s or 15m, got %q", key, raw)
	}
	return d, nil
}
