package config

import (
	"context"
	"errors"
	"fmt"

	"github.com/jayleaton/go-fast-stack/packages/secrets"
)

// minSecretLength is the shortest signing/encryption secret accepted: 32
// characters, what `openssl rand -base64 32` comfortably exceeds.
const minSecretLength = 32

// Live secrets of the admin service: served by SECRETS_PROVIDER (its own
// Doppler config and token) and rotated without a redeploy when the provider
// is remote. Only DOPPLER_TOKEN is not one. See the web app's
// config/secrets.go and AGENTS.md "Adding a secret".
const (
	secretAppDatabaseURL   = "DATABASE_URL"       // the app database (users being administered)
	secretAdminDatabaseURL = "ADMIN_DATABASE_URL" // the admin database (admins, audit log)
	secretAdminAuth        = "ADMIN_AUTH_SECRET"  // a keyring: also ADMIN_AUTH_SECRET_PREVIOUS and _REVOKED
	secretResendAPIKey     = "RESEND_API_KEY"
)

var liveSecrets = append([]string{secretAppDatabaseURL, secretAdminDatabaseURL, secretResendAPIKey}, secrets.KeyringNames(secretAdminAuth)...)

// Secrets hands live secrets to cmd/admin through typed accessors; nothing
// else reads them. Load builds it (no I/O); every command calls Start first.
type Secrets struct{ m *secrets.Manager }

// Start fetches and validates the first snapshot (failing without one), then
// polls until ctx ends or Stop is called.
func (s Secrets) Start(ctx context.Context) error { return s.m.Start(ctx) }

// Stop ends polling. Values already handed out keep working.
func (s Secrets) Stop() { s.m.Stop() }

// Healthy reports secrets.ErrNotLoaded or secrets.ErrStale (server /healthz).
func (s Secrets) Healthy() error { return s.m.Healthy() }

// Provider is SECRETS_PROVIDER.
func (s Secrets) Provider() string { return s.m.Provider() }

// AppDatabaseURL is the current DATABASE_URL (after Start), trimmed.
func (s Secrets) AppDatabaseURL() (secrets.Secret, error) {
	return trimmed(s.m.Get(secretAppDatabaseURL))
}

// OnAppDatabaseURL calls fn now and whenever DATABASE_URL changes upstream.
func (s Secrets) OnAppDatabaseURL(fn secrets.Hook) { s.onURL(secretAppDatabaseURL, fn) }

// AdminDatabaseURL is the current ADMIN_DATABASE_URL (after Start), trimmed.
func (s Secrets) AdminDatabaseURL() (secrets.Secret, error) {
	return trimmed(s.m.Get(secretAdminDatabaseURL))
}

// OnAdminDatabaseURL calls fn now and whenever ADMIN_DATABASE_URL changes upstream.
func (s Secrets) OnAdminDatabaseURL(fn secrets.Hook) { s.onURL(secretAdminDatabaseURL, fn) }

// AuthSecrets is the ADMIN_AUTH_SECRET keyring (after Start).
func (s Secrets) AuthSecrets() (secrets.Keyring, error) { return s.m.Keyring(secretAdminAuth) }

// OnAuthSecrets calls fn now and whenever the ADMIN_AUTH_SECRET keyring
// changes. fn updates the admin auth instance (Auth.SetSecrets).
func (s Secrets) OnAuthSecrets(fn func(secrets.Keyring) error) { s.m.OnKeyring(secretAdminAuth, fn) }

// OnResendAPIKey calls fn now with RESEND_API_KEY (a zero Secret when unset)
// and whenever it changes upstream. fn rebuilds the Resend client.
func (s Secrets) OnResendAPIKey(fn secrets.Hook) { s.m.OnChange(secretResendAPIKey, fn) }

func (s Secrets) onURL(name string, fn secrets.Hook) {
	s.m.OnChange(name, func(v secrets.Secret) error { u, _ := trimmed(v, nil); return fn(u) })
}

func trimmed(v secrets.Secret, err error) (secrets.Secret, error) { return v.TrimSpace(), err }

// validateLive checks one snapshot (env at boot, every remote fetch before
// it is published). appAuthSecret is the web app's AUTH_SECRET when the env
// has it (the admin's own Doppler config never does). Messages name keys,
// never values.
func validateLive(env Env, appAuthSecret string) func(get func(string) string) []error {
	return func(get func(string) string) []error {
		var errs []error
		if get(secretAppDatabaseURL) == "" {
			errs = append(errs, errors.New("DATABASE_URL (the app database) is required"))
		}
		switch get(secretAdminDatabaseURL) {
		case "":
			errs = append(errs, errors.New("ADMIN_DATABASE_URL is required"))
		case get(secretAppDatabaseURL):
			errs = append(errs, errors.New("ADMIN_DATABASE_URL must be a different database from DATABASE_URL"))
		}
		k := secrets.ParseKeyring(get, secretAdminAuth)
		if k.Current.Len() < minSecretLength {
			errs = append(errs, errors.New("ADMIN_AUTH_SECRET must be at least 32 characters (openssl rand -base64 32)"))
		} else if appAuthSecret != "" && k.Current.Equal(secrets.NewSecret(appAuthSecret)) {
			errs = append(errs, errors.New("ADMIN_AUTH_SECRET must differ from the app's AUTH_SECRET"))
		}
		for li, list := range [][]secrets.Secret{k.Previous, k.Revoked} {
			for i, v := range list {
				if v.Len() < minSecretLength {
					errs = append(errs, fmt.Errorf("%s entry %d must be at least 32 characters", secrets.KeyringNames(secretAdminAuth)[li+1], i+1))
				}
			}
		}
		if env == Production && get(secretResendAPIKey) == "" {
			errs = append(errs, errors.New("RESEND_API_KEY is required in production (admin invites are emailed)"))
		}
		return errs
	}
}

func loadSecrets(env Env, get func(key, fallback string) string, errs *[]error) Secrets {
	m, problems := secrets.Bootstrap(func(k string) string { return get(k, "") }, secrets.BootstrapOptions{
		Keys: liveSecrets, Validate: validateLive(env, get("AUTH_SECRET", "")), Production: env == Production,
	})
	*errs = append(*errs, problems...)
	return Secrets{m}
}
