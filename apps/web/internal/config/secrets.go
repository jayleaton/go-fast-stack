package config

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/jayleaton/go-fast-stack/packages/secrets"
)

// SECRETS_PROVIDER values.
const (
	SecretsEnv     = secrets.ProviderEnv     // values from the process env, read once at boot (default)
	SecretsDoppler = secrets.ProviderDoppler // values polled from one Doppler config; rotate without a restart
)

// Live secrets: names served by the secrets provider instead of the env, so
// they rotate without a redeploy when the provider is remote. Each gets a
// typed accessor on Secrets. To add one, see AGENTS.md "Adding a secret".
// Only DOPPLER_TOKEN (the credential that reaches the provider) is not one.
const (
	secretResendAPIKey = "RESEND_API_KEY"
	secretDatabaseURL  = "DATABASE_URL"
	secretAuth         = "AUTH_SECRET" // a keyring: also AUTH_SECRET_PREVIOUS and AUTH_SECRET_REVOKED
)

var liveSecrets = slices.Concat([]string{secretResendAPIKey, secretDatabaseURL}, secrets.KeyringNames(secretAuth), socialSecretNames, storageSecrets)

// Secrets hands live secrets to the server through typed accessors; nothing
// else reads them. Load builds it (no I/O); serve and migrate call Start,
// which fetches the first snapshot, and Stop when done.
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

// DatabaseURL is the current DATABASE_URL (after Start), trimmed.
func (s Secrets) DatabaseURL() (secrets.Secret, error) { return trimmed(s.m.Get(secretDatabaseURL)) }

// OnDatabaseURL calls fn now with DATABASE_URL and again whenever it changes
// upstream. fn swaps the database pool; an error keeps the old pool (and is
// retried after the next fetch).
func (s Secrets) OnDatabaseURL(fn secrets.Hook) {
	s.m.OnChange(secretDatabaseURL, func(v secrets.Secret) error { u, _ := trimmed(v, nil); return fn(u) })
}

// AuthSecrets is the AUTH_SECRET keyring (after Start).
func (s Secrets) AuthSecrets() (secrets.Keyring, error) { return s.m.Keyring(secretAuth) }

// OnAuthSecrets calls fn now and whenever the AUTH_SECRET keyring changes.
// fn updates packages/auth (Auth.SetSecrets); an error keeps the old keyring.
func (s Secrets) OnAuthSecrets(fn func(secrets.Keyring) error) { s.m.OnKeyring(secretAuth, fn) }

// validateLive checks the live secrets of one snapshot (env at boot, every
// remote fetch before it is published). Messages name keys, never values.
func validateLive(env Env, social Social) func(get func(name string) string) []error {
	return func(get func(string) string) []error {
		var errs []error
		if get(secretDatabaseURL) == "" {
			errs = append(errs, errors.New("DATABASE_URL is required"))
		}
		errs = append(errs, validateKeyring(get, secretAuth)...)
		errs = append(errs, validateStorage(get)...)
		if env == Production && get(secretResendAPIKey) == "" {
			errs = append(errs, errors.New("RESEND_API_KEY is required in production"))
		}
		return append(errs, social.validate(get)...)
	}
}

// minSecretLength is the shortest signing/encryption secret accepted: 32
// characters, what `openssl rand -base64 32` comfortably exceeds.
const minSecretLength = 32

// validateKeyring requires a current value and at least 32 characters for
// every value of the keyring called name.
func validateKeyring(get func(string) string, name string) []error {
	var errs []error
	k := secrets.ParseKeyring(get, name)
	if k.Current.Len() < minSecretLength {
		errs = append(errs, fmt.Errorf("%s must be at least 32 characters (openssl rand -base64 32)", name))
	}
	for li, list := range [][]secrets.Secret{k.Previous, k.Revoked} {
		for i, v := range list {
			if v.Len() < minSecretLength {
				errs = append(errs, fmt.Errorf("%s entry %d must be at least 32 characters", secrets.KeyringNames(name)[li+1], i+1))
			}
		}
	}
	return errs
}

// loadSecrets builds the Manager from SECRETS_PROVIDER and its bootstrap
// vars. Every problem is appended to errs.
func loadSecrets(env Env, social Social, get func(key, fallback string) string, errs *[]error) Secrets {
	m, problems := secrets.Bootstrap(func(k string) string { return get(k, "") }, secrets.BootstrapOptions{
		Keys: liveSecrets, Validate: validateLive(env, social), Production: env == Production,
	})
	*errs = append(*errs, problems...)
	return Secrets{m}
}

func trimmed(v secrets.Secret, err error) (secrets.Secret, error) { return v.TrimSpace(), err }
