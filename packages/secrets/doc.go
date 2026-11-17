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
// # Providers
//
// SECRETS_PROVIDER picks one: env (the default) reads the process
// environment once at boot, so a change needs a redeploy; doppler polls one
// Doppler config per service and environment with a read-only service token
// (DOPPLER_TOKEN) every SECRETS_REFRESH_INTERVAL (60s, at least 10s). The
// Infisical and 1Password providers are stubs outside the default build.
// DOPPLER_TOKEN itself rotates only with a redeploy. In development keep env
// and .env, or run doppler run -- make dev.
//
// # Rotating live
//
// Each consumer picks the mechanism that fits it, registered in apps/*/cmd:
//
//   - An outbound API key (RESEND_API_KEY, the STORAGE_S3_* set) rebuilds its
//     client and swaps it in atomically (auth.SwapMailer); storage first
//     probes the new client and keeps the old one if the probe fails.
//   - DATABASE_URL goes through database.Pool.Swap: probe the new pool, check
//     it is migrated as far as the binary, switch atomically, drain the old
//     one. A URL that fails the probe is refused and the old pool serves on.
//     For a zero-downtime password change, alternate two login roles that
//     both act as one owner role, so the idle one takes the new password.
//   - A signing key is a Keyring (AUTH_SECRET with _PREVIOUS, _REVOKED and
//     a grace of AUTH_SECRET_GRACE; auth.Auth.SetSecrets): the newest signs,
//     previous ones verify until their grace ends, revoked ones are rejected
//     at once. Data at rest sits under a data key the secret only wraps, so a
//     rotation rewraps it. Rotate by saving the new value and the old one as
//     _PREVIOUS together; for a leak, put the old one in _REVOKED instead.
//
// # Hooks
//
// A hook gets the new value and builds from it; it never caches the value
// elsewhere. On a zero value it falls back or keeps the old client. An error
// keeps the previous client, and the manager calls the hook again after each
// later fetch. A hook must not call back into the manager (OnChange,
// Refresh, Stop). Log Fingerprint, never the value: a fingerprint is an HMAC
// under a per-process key, so it compares only within one process.
//
// # Failures
//
// Without a valid first snapshot the server does not boot. After that a
// failed or invalid fetch never replaces the last good set: the manager
// retries with backoff (honouring Retry-After), and once the set is older
// than SECRETS_MAX_STALENESS the app's /healthz answers 503. /healthz is a
// readiness check; never restart on it, or a provider outage becomes a
// restart loop.
//
// # Values never leak
//
// Values are Secret, which formats, logs and marshals as "[redacted]". No
// method returns the value: only reveal.Value (package
// packages/secrets/reveal) does, and only apps/*/cmd may import it (depguard).
// Nothing in this package logs a value, a token or a provider response body.
// NewRedactingHandler wraps the binaries' slog handler so credentials that
// reach a log anyway are redacted. The package never reads the environment:
// apps/*/internal/config builds the Manager.
package secrets
