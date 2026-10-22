//go:build secrets_onepassword

package secrets

import (
	"context"
	"errors"
)

// OnePassword is a placeholder for a 1Password provider. It is excluded from
// the default build (tag secrets_onepassword) and not selectable through
// SECRETS_PROVIDER until it is implemented.
//
// TODO(secrets): implement once a team picks 1Password. Plan:
//   - Service account token, read-only on one vault (or one Environment) per
//     service and environment.
//   - The official github.com/1password/onepassword-sdk-go pulls in a Wasm
//     runtime (Extism/wazero): measure binary size and memory against the
//     CGO-disabled static image before adopting it. Desktop-app auth does not
//     work with CGO disabled.
//   - One snapshot per Fetch: read the whole Environment
//     (client.Environments().GetVariables, beta) or resolve a fixed list of
//     op:// references in one batch.
//   - Rate limits are per account per day (Teams: 5,000 requests): derive the
//     poll Interval from replicas × services, e.g. 5 minutes, not 60 s.
//   - Errors carry status codes only (see Doppler.Fetch).
//   - Add "1password" to config.Load and a fake-server test like doppler_test.go.
type OnePassword struct {
	ServiceAccountToken string
	EnvironmentID       string            // 1Password Environment, or
	References          map[string]string // name -> op://vault/item/field
}

var _ Provider = (*OnePassword)(nil)

// Name returns "1password".
func (*OnePassword) Name() string { return "1password" }

// Fetch is not implemented.
func (*OnePassword) Fetch(context.Context, Snapshot) (Snapshot, error) {
	return Snapshot{}, errors.New("1password: provider not implemented")
}
