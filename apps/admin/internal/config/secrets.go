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
type Secrets struct{ manager *secrets.Manager }

// Start fetches and validates the first snapshot (failing without one), then
// polls until ctx ends or Stop is called.
func (accessors Secrets) Start(ctx context.Context) error { return accessors.manager.Start(ctx) }

// Stop ends polling. Values already handed out keep working.
func (accessors Secrets) Stop() { accessors.manager.Stop() }

// Healthy reports secrets.ErrNotLoaded or secrets.ErrStale (server /healthz).
func (accessors Secrets) Healthy() error { return accessors.manager.Healthy() }

// Provider is SECRETS_PROVIDER.
func (accessors Secrets) Provider() string { return accessors.manager.Provider() }

// AppDatabaseURL is the current DATABASE_URL (after Start), trimmed.
func (accessors Secrets) AppDatabaseURL() (secrets.Secret, error) {
	return trimmed(accessors.manager.Get(secretAppDatabaseURL))
}

// OnAppDatabaseURL calls fn now and whenever DATABASE_URL changes upstream.
func (accessors Secrets) OnAppDatabaseURL(fn secrets.Hook) { accessors.onURL(secretAppDatabaseURL, fn) }

// AdminDatabaseURL is the current ADMIN_DATABASE_URL (after Start), trimmed.
func (accessors Secrets) AdminDatabaseURL() (secrets.Secret, error) {
	return trimmed(accessors.manager.Get(secretAdminDatabaseURL))
}

// OnAdminDatabaseURL calls fn now and whenever ADMIN_DATABASE_URL changes upstream.
func (accessors Secrets) OnAdminDatabaseURL(fn secrets.Hook) {
	accessors.onURL(secretAdminDatabaseURL, fn)
}

// AuthSecrets is the ADMIN_AUTH_SECRET keyring (after Start).
func (accessors Secrets) AuthSecrets() (secrets.Keyring, error) {
	return accessors.manager.Keyring(secretAdminAuth)
}

// OnAuthSecrets calls fn now and whenever the ADMIN_AUTH_SECRET keyring
// changes. fn updates the admin auth instance (Auth.SetSecrets).
func (accessors Secrets) OnAuthSecrets(fn func(secrets.Keyring) error) {
	accessors.manager.OnKeyring(secretAdminAuth, fn)
}

// OnResendAPIKey calls fn now with RESEND_API_KEY (a zero Secret when unset)
// and whenever it changes upstream. fn rebuilds the Resend client.
func (accessors Secrets) OnResendAPIKey(fn secrets.Hook) {
	accessors.manager.OnChange(secretResendAPIKey, fn)
}

func (accessors Secrets) onURL(name string, fn secrets.Hook) {
	accessors.manager.OnChange(name, func(updated secrets.Secret) error { trimmedURL, _ := trimmed(updated, nil); return fn(trimmedURL) })
}

func trimmed(secret secrets.Secret, err error) (secrets.Secret, error) {
	return secret.TrimSpace(), err
}

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
		keyring := secrets.ParseKeyring(get, secretAdminAuth)
		if keyring.Current.Len() < minSecretLength {
			errs = append(errs, errors.New("ADMIN_AUTH_SECRET must be at least 32 characters (openssl rand -base64 32)"))
		} else if appAuthSecret != "" && keyring.Current.Equal(secrets.NewSecret(appAuthSecret)) {
			errs = append(errs, errors.New("ADMIN_AUTH_SECRET must differ from the app's AUTH_SECRET"))
		}
		for li, list := range [][]secrets.Secret{keyring.Previous, keyring.Revoked} {
			for index, secret := range list {
				if secret.Len() < minSecretLength {
					errs = append(errs, fmt.Errorf("%s entry %d must be at least 32 characters", secrets.KeyringNames(secretAdminAuth)[li+1], index+1))
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
	manager, problems := secrets.Bootstrap(func(key string) string { return get(key, "") }, secrets.BootstrapOptions{
		Keys: liveSecrets, Validate: validateLive(env, get("AUTH_SECRET", "")), Production: env == Production,
	})
	*errs = append(*errs, problems...)
	return Secrets{manager}
}
