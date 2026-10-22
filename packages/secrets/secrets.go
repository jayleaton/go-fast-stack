// Package secrets serves secrets that can change while the server runs.
//
// A Provider fetches a whole Snapshot (every secret of one service and
// environment) in one request. A Manager polls the provider, validates each
// snapshot before publishing it, keeps the last good one when the provider is
// down, reports itself unhealthy once that copy is older than the staleness
// limit, and calls per-key change hooks so consumers can rebuild their clients:
//
//	m := secrets.NewManager(secrets.NewDoppler(token), secrets.Options{Keys: []string{"RESEND_API_KEY"}})
//	if err := m.Start(ctx); err != nil { ... } // first fetch; fails boot without a valid snapshot
//	defer m.Stop()
//	m.OnChange("RESEND_API_KEY", func(v secrets.Secret) error { mailer.Swap(newMailer(v.Reveal())); return nil })
//
// Values are Secret, which formats, logs and marshals as "[redacted]"; only
// Reveal returns the value. Nothing in this package logs a value, a token or a
// provider response body. The package never reads the environment:
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
	"time"
)

const redacted = "[redacted]"

// Secret is a secret value. Every way of printing it (fmt verbs, slog, JSON,
// text marshalling) yields "[redacted]"; Reveal returns the value.
type Secret struct{ v string }

// NewSecret wraps v.
func NewSecret(v string) Secret { return Secret{v} }

// Reveal returns the value. Pass it straight to the client that needs it.
func (s Secret) Reveal() string { return s.v }

// IsZero reports whether the secret is empty (unset or removed upstream).
func (s Secret) IsZero() bool { return s.v == "" }

// Equal compares two secrets in constant time.
func (s Secret) Equal(o Secret) bool {
	return subtle.ConstantTimeCompare([]byte(s.v), []byte(o.v)) == 1
}

// Fingerprint identifies the value in logs without revealing it: the first
// 4 bytes of an HMAC-SHA256 under a random per-process key, so it shows when a
// value changed within one process but cannot be compared across processes
// or brute-forced offline. "none" for an empty secret.
func (s Secret) Fingerprint() string {
	if s.v == "" {
		return "none"
	}
	mac := hmac.New(sha256.New, fingerprintKey)
	mac.Write([]byte(s.v))
	return "fp:" + hex.EncodeToString(mac.Sum(nil)[:4])
}

var fingerprintKey = func() []byte {
	k := make([]byte, 32)
	_, _ = rand.Read(k) // never fails on supported platforms (crypto/rand docs)
	return k
}()

// Format implements fmt.Formatter for every verb, so %v, %s, %d, %x and %#v
// all print "[redacted]".
func (s Secret) Format(f fmt.State, _ rune) { _, _ = io.WriteString(f, redacted) }

// String returns "[redacted]".
func (s Secret) String() string { return redacted }

// LogValue implements slog.LogValuer.
func (s Secret) LogValue() slog.Value { return slog.StringValue(redacted) }

// MarshalText implements encoding.TextMarshaler (and so JSON).
func (s Secret) MarshalText() ([]byte, error) { return []byte(redacted), nil }

// Snapshot is every secret a provider returned for one fetch. It is immutable.
type Snapshot struct {
	values   map[string]Secret
	revision string
}

// NewSnapshot copies values into a Snapshot. revision is the provider's
// version of the whole set (an ETag) or "" when it has none.
func NewSnapshot(values map[string]string, revision string) Snapshot {
	s := Snapshot{values: make(map[string]Secret, len(values)), revision: revision}
	for k, v := range values {
		s.values[k] = Secret{v}
	}
	return s
}

// Format prints names and revision only: fmt cannot reach Secret's own
// redaction through the unexported map, so Snapshot redacts itself.
func (s Snapshot) Format(f fmt.State, _ rune) {
	_, _ = fmt.Fprintf(f, "secrets.Snapshot{names: %v, revision: %q}", s.Names(), s.revision)
}

// LogValue implements slog.LogValuer with names and revision only.
func (s Snapshot) LogValue() slog.Value {
	return slog.GroupValue(slog.Any("names", s.Names()), slog.String("revision", s.revision))
}

// Get returns the named secret and whether the snapshot has it.
func (s Snapshot) Get(name string) (Secret, bool) {
	v, ok := s.values[name]
	return v, ok
}

// Names returns the secret names, sorted. Names are not secret.
func (s Snapshot) Names() []string { return slices.Sorted(maps.Keys(s.values)) }

// Len is the number of secrets.
func (s Snapshot) Len() int { return len(s.values) }

// Revision is the provider's version of the set, "" if it has none.
func (s Snapshot) Revision() string { return s.revision }

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
