// Package reveal reads the value of a secrets.Secret.
//
// It is the only way to read a secret: Secret has no method that returns its
// value, so an interface cannot reach it either. Only apps/*/cmd, where clients
// are built (database.Open, auth.Config, NewResendMailer), may import this
// package; depguard rejects it everywhere else (see .golangci.yml).
package reveal

import (
	"github.com/jayleaton/ai-patterns-go/packages/secrets"
	"github.com/jayleaton/ai-patterns-go/packages/secrets/internal/access"
)

// Value returns the secret's value. Pass it straight to the client that needs
// it; never store, log or return it.
func Value(s secrets.Secret) string {
	v, _ := access.Reveal(s)
	return v
}

// Keyring returns a keyring's values (current signs, previous still verify,
// revoked never do), for the one consumer that needs them (auth's keys).
func Keyring(k secrets.Keyring) (current string, previous, revoked []string) {
	for _, p := range k.Previous {
		previous = append(previous, Value(p))
	}
	for _, r := range k.Revoked {
		revoked = append(revoked, Value(r))
	}
	return Value(k.Current), previous, revoked
}
