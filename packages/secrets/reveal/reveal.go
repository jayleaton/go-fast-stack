// Package reveal reads the value of a secrets.Secret.
//
// It is the only way to read a secret: Secret has no method that returns its
// value, so an interface cannot reach it either. Only apps/*/cmd, where clients
// are built (database.Open, auth.Config, NewResendMailer), may import this
// package; depguard rejects it everywhere else (see .golangci.yml).
package reveal

import (
	"github.com/jayleaton/go-fast-stack/packages/secrets"
	"github.com/jayleaton/go-fast-stack/packages/secrets/internal/access"
)

// Value returns the secret's value. Pass it straight to the client that needs
// it; never store, log or return it.
func Value(secret secrets.Secret) string {
	reveal, _ := access.Reveal(secret)
	return reveal
}

// Keyring returns a keyring's values (current signs, previous still verify,
// revoked never do), for the one consumer that needs them (auth's keys).
func Keyring(keyring secrets.Keyring) (current string, previous, revoked []string) {
	for _, secret := range keyring.Previous {
		previous = append(previous, Value(secret))
	}
	for _, secret := range keyring.Revoked {
		revoked = append(revoked, Value(secret))
	}
	return Value(keyring.Current), previous, revoked
}
