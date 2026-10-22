//go:build secrets_infisical

package secrets

import (
	"context"
	"errors"
)

// Infisical is a placeholder for an Infisical provider. It is excluded from
// the default build (tag secrets_infisical) and not selectable through
// SECRETS_PROVIDER until it is implemented.
//
// TODO(secrets): implement once a team picks Infisical. Plan:
//   - Prefer plain net/http against the REST API, as Doppler does, to keep the
//     binary small; only take github.com/infisical/go-sdk if the API needs it.
//   - Universal Auth: POST /api/v1/auth/universal-auth/login with ClientID and
//     ClientSecret, cache the access token until its TTL (renewing a token is
//     not refreshing secrets; Fetch still lists every poll).
//   - One snapshot per Fetch: list every secret of ProjectID/Environment/Path
//     in a single request; reject a partial page rather than publish it.
//   - Machine identity is read-only on one environment. Free plan counts
//     humans and machines together (5 identities): document the cost.
//   - Errors carry status codes only (see Doppler.Fetch).
//   - Add "infisical" to config.Load and a fake-server test like doppler_test.go.
type Infisical struct {
	SiteURL      string // https://app.infisical.com or a self-hosted origin
	ClientID     string
	ClientSecret string
	ProjectID    string
	Environment  string // e.g. "prod"
	Path         string // default "/"
}

var _ Provider = (*Infisical)(nil)

// Name returns "infisical".
func (*Infisical) Name() string { return "infisical" }

// Fetch is not implemented.
func (*Infisical) Fetch(context.Context, Snapshot) (Snapshot, error) {
	return Snapshot{}, errors.New("infisical: provider not implemented")
}
