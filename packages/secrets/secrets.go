// Package secrets serves secrets that can change while the server runs.
//
// A Provider fetches a whole Snapshot (every secret of one service and
// environment) in one request. A Manager polls the provider, validates each
// snapshot before publishing it, keeps the last good one when the provider is
// down, reports itself unhealthy once that copy is older than the staleness
// limit, and calls per-key change hooks so consumers can rebuild their clients:
//
//	m := secrets.NewManager(secrets.NewDoppler(secrets.NewSecret(token)), secrets.Options{Keys: []string{"RESEND_API_KEY"}})
//	if err := m.Start(ctx); err != nil { ... } // first fetch; fails boot without a valid snapshot
//	defer m.Stop()
//	m.OnChange("RESEND_API_KEY", func(v secrets.Secret) error { mailer.Swap(newMailer(reveal.Value(v))); return nil })
//
// Values are Secret, which formats, logs and marshals as "[redacted]". No
// method returns the value: only reveal.Value (package
// packages/secrets/reveal) does, and only apps/*/cmd may import it (depguard).
// Nothing in this package logs a value, a token or a provider response body. NewRedactingHandler wraps the binaries' slog
// handler so credentials that reach a log anyway are redacted. The package
// never reads the environment:
// apps/web/internal/config builds the Manager.
package secrets

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/jayleaton/go-fast-stack/packages/secrets/internal/access"
)

const redacted = "[redacted]"

// Secret is a secret value. Every way of printing it (fmt verbs, slog, JSON,
// text marshalling) yields "[redacted]". No method returns the value, so no
// interface can reach it either: reveal.Value is the only way to read it.
type Secret struct{ value string }

func init() {
	access.Reveal = func(input any) (string, bool) {
		secret, ok := input.(Secret)
		return secret.value, ok
	}
}

// NewSecret wraps v.
func NewSecret(value string) Secret { return Secret{value} }

// Len is the length of the value in bytes, for validation (minimum lengths)
// without revealing it.
func (secret Secret) Len() int { return len(secret.value) }

// TrimSpace returns the secret without leading and trailing white space.
func (secret Secret) TrimSpace() Secret { return Secret{strings.TrimSpace(secret.value)} }

// IsZero reports whether the secret is empty (unset or removed upstream).
func (secret Secret) IsZero() bool { return secret.value == "" }

// Equal compares two secrets in constant time.
func (secret Secret) Equal(other Secret) bool {
	return subtle.ConstantTimeCompare([]byte(secret.value), []byte(other.value)) == 1
}

// Fingerprint identifies the value in logs without revealing it: the first
// 4 bytes of an HMAC-SHA256 under a random per-process key, so it shows when a
// value changed within one process but cannot be compared across processes
// or brute-forced offline. "none" for an empty secret.
func (secret Secret) Fingerprint() string {
	if secret.value == "" {
		return "none"
	}
	mac := hmac.New(sha256.New, fingerprintKey)
	mac.Write([]byte(secret.value))
	return "fp:" + hex.EncodeToString(mac.Sum(nil)[:4])
}

// fingerprintKeyBytes sizes the per-process HMAC key: 256 bits, the size of
// SHA-256's output.
const fingerprintKeyBytes = 32

var fingerprintKey = func() []byte {
	data := make([]byte, fingerprintKeyBytes)
	_, _ = rand.Read(data) // never fails on supported platforms (crypto/rand docs)
	return data
}()

// Format implements fmt.Formatter for every verb, so %v, %s, %d, %x and %#v
// all print "[redacted]".
func (secret Secret) Format(state fmt.State, _ rune) { _, _ = io.WriteString(state, redacted) }

// String returns "[redacted]".
func (secret Secret) String() string { return redacted }

// LogValue implements slog.LogValuer.
func (secret Secret) LogValue() slog.Value { return slog.StringValue(redacted) }

// MarshalText implements encoding.TextMarshaler (and so JSON).
func (secret Secret) MarshalText() ([]byte, error) { return []byte(redacted), nil }

// Snapshot is every secret a provider returned for one fetch. It is immutable.
type Snapshot struct {
	values   map[string]Secret
	revision string
}

// NewSnapshot copies values into a Snapshot. revision is the provider's
// version of the whole set (an ETag) or "" when it has none.
func NewSnapshot(values map[string]string, revision string) Snapshot {
	snapshot := Snapshot{values: make(map[string]Secret, len(values)), revision: revision}
	for key, value := range values {
		snapshot.values[key] = Secret{value}
	}
	return snapshot
}

// Format prints names and revision only: fmt cannot reach Secret's own
// redaction through the unexported map, so Snapshot redacts itself.
func (snapshot Snapshot) Format(state fmt.State, _ rune) {
	_, _ = fmt.Fprintf(state, "secrets.Snapshot{names: %v, revision: %q}", snapshot.Names(), snapshot.revision)
}

// LogValue implements slog.LogValuer with names and revision only.
func (snapshot Snapshot) LogValue() slog.Value {
	return slog.GroupValue(slog.Any("names", snapshot.Names()), slog.String("revision", snapshot.revision))
}

// Get returns the named secret and whether the snapshot has it.
func (snapshot Snapshot) Get(name string) (Secret, bool) {
	secret, ok := snapshot.values[name]
	return secret, ok
}

// Names returns the secret names, sorted. Names are not secret.
func (snapshot Snapshot) Names() []string { return slices.Sorted(maps.Keys(snapshot.values)) }

// Len is the number of secrets.
func (snapshot Snapshot) Len() int { return len(snapshot.values) }

// Revision is the provider's version of the set, "" if it has none.
func (snapshot Snapshot) Revision() string { return snapshot.revision }

// Provider fetches the full set of secrets for one service and environment.
// Implementations must not put secret values, tokens or response bodies in
// errors. previous is the last snapshot the Manager accepted (zero before the
// first fetch); a provider that supports conditional requests may return it
// unchanged when the upstream set has not changed.
type Provider interface {
	Name() string
	Fetch(ctx context.Context, previous Snapshot) (Snapshot, error)
}

// Static is implemented by providers whose values never change after startup
// (Env). The Manager loads them once, never polls and never reports them stale.
type Static interface {
	Static()
}

// RetryAfter is implemented by provider errors that carry a server-requested
// wait (HTTP 429 Retry-After). The Manager waits at least that long, capped
// at Options.MaxBackoff.
type RetryAfter interface {
	error
	RetryAfter() time.Duration
}

var (
	// ErrNotLoaded: no snapshot has been accepted yet (Start not called or failed).
	ErrNotLoaded = errors.New("secrets: not loaded")
	// ErrStale: the last good snapshot is older than Options.MaxStaleness.
	ErrStale = errors.New("secrets: stale")
	// ErrInvalid: a fetched snapshot failed validation and was not published.
	ErrInvalid = errors.New("secrets: invalid snapshot")
)
