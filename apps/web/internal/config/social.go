package config

import (
	"errors"

	"github.com/jayleaton/go-fast-stack/packages/secrets"
)

// Social sign-in (optional; README "Google and Apple sign-in"). A provider is
// on when its client id is set, and then needs its credentials. Client ids
// and the Apple team id are plain env; the Google client secret and the
// Apple key (with its key id, which changes with it) are live secrets.
const (
	secretGoogleClientSecret = "GOOGLE_CLIENT_SECRET"
	secretApplePrivateKey    = "APPLE_PRIVATE_KEY" // the .p8 key, PEM (literal "\n" allowed)
	secretAppleKeyID         = "APPLE_KEY_ID"      // not secret, but rotates with the key
)

var socialSecretNames = []string{secretGoogleClientSecret, secretApplePrivateKey, secretAppleKeyID}

// Social is the provider config. The callbacks to register with the
// providers are <PUBLIC_URL>/auth/callback/google and .../apple (UI route).
type Social struct {
	GoogleClientID string // GOOGLE_CLIENT_ID: an OAuth client of type "Web application"
	AppleClientID  string // APPLE_CLIENT_ID: the Services ID
	AppleTeamID    string // APPLE_TEAM_ID
}

// GoogleEnabled reports whether Google sign-in is configured.
func (social Social) GoogleEnabled() bool { return social.GoogleClientID != "" }

// AppleEnabled reports whether Apple sign-in is configured.
func (social Social) AppleEnabled() bool { return social.AppleClientID != "" }

func loadSocial(get func(key, fallback string) string, errs *[]error) Social {
	social := Social{
		GoogleClientID: get("GOOGLE_CLIENT_ID", ""),
		AppleClientID:  get("APPLE_CLIENT_ID", ""),
		AppleTeamID:    get("APPLE_TEAM_ID", ""),
	}
	if social.AppleEnabled() && social.AppleTeamID == "" {
		*errs = append(*errs, errors.New("APPLE_TEAM_ID is required when APPLE_CLIENT_ID is set"))
	}
	return social
}

// validate checks one snapshot's provider credentials: every enabled
// provider needs its own. Messages name keys, never values.
func (social Social) validate(get func(name string) string) []error {
	var errs []error
	if social.GoogleEnabled() && get(secretGoogleClientSecret) == "" {
		errs = append(errs, errors.New("GOOGLE_CLIENT_SECRET is required when GOOGLE_CLIENT_ID is set"))
	}
	if social.AppleEnabled() && (get(secretAppleKeyID) == "" || get(secretApplePrivateKey) == "") {
		errs = append(errs, errors.New("APPLE_KEY_ID and APPLE_PRIVATE_KEY are required when APPLE_CLIENT_ID is set"))
	}
	return errs
}

// SocialSecrets are the provider credentials; zero Secrets for unset ones.
type SocialSecrets struct {
	GoogleClientSecret, ApplePrivateKey, AppleKeyID secrets.Secret
}

// Fingerprint identifies the set for logs and change detection.
func (creds SocialSecrets) Fingerprint() string {
	return creds.GoogleClientSecret.Fingerprint() + "/" + creds.ApplePrivateKey.Fingerprint() + "/" + creds.AppleKeyID.Fingerprint()
}

// SocialSecrets returns the current provider credentials (after Start).
func (secretsSource Secrets) SocialSecrets() (SocialSecrets, error) {
	var out SocialSecrets
	var errs []error
	for name, dst := range map[string]*secrets.Secret{
		secretGoogleClientSecret: &out.GoogleClientSecret,
		secretApplePrivateKey:    &out.ApplePrivateKey,
		secretAppleKeyID:         &out.AppleKeyID,
	} {
		value, err := trimmed(secretsSource.manager.Get(name))
		*dst, errs = value, append(errs, err)
	}
	return out, errors.Join(errs...)
}

// OnSocialSecrets calls fn whenever the provider credentials change upstream
// (SocialSecrets gives the first values). fn swaps them into packages/auth
// (Auth.SetSocialSecrets); an error keeps the old ones until the next fetch.
func (secretsSource Secrets) OnSocialSecrets(fn func(SocialSecrets) error) {
	var last string // hooks run one at a time
	if cur, err := secretsSource.SocialSecrets(); err == nil {
		last = cur.Fingerprint()
	}
	hook := func(secrets.Secret) error {
		cur, err := secretsSource.SocialSecrets()
		if err != nil {
			return err
		}
		if fp := cur.Fingerprint(); fp != last {
			if err := fn(cur); err != nil {
				return err
			}
			last = fp
		}
		return nil
	}
	for _, name := range socialSecretNames {
		secretsSource.manager.OnChange(name, hook)
	}
}
