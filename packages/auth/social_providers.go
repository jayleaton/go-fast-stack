package auth

import (
	"context"
	"crypto/ecdsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// GoogleConfig turns on "Sign in with Google" (Better Auth's google social
// provider: authorization code flow with PKCE, scopes openid email profile).
type GoogleConfig struct {
	ClientID     string // OAuth client of type "Web application"
	ClientSecret string // its secret (a live secret: rotate with SetSocialSecrets)

	endpoints *oauthEndpoints // tests: a fake provider
}

// AppleConfig turns on "Sign in with Apple" (Better Auth's apple social
// provider: form_post response, client secret signed from the .p8 key and
// renewed before it expires).
type AppleConfig struct {
	ClientID   string // the Services ID (not the app's bundle ID)
	TeamID     string
	KeyID      string // rotates with PrivateKey (SetSocialSecrets)
	PrivateKey string // the .p8 key, PKCS#8 PEM; literal "\n" allowed

	endpoints *oauthEndpoints
}

// SocialSecrets are the provider credentials that rotate (live secrets).
type SocialSecrets struct {
	GoogleClientSecret string
	ApplePrivateKey    string
	AppleKeyID         string // changes together with ApplePrivateKey
}

// Provider HTTP calls.
const (
	providerTimeout  = 15 * time.Second
	maxTokenResponse = 1 << 20 // 1 MiB cap on a token endpoint answer
	decimal          = 10      // strconv base of expires_in values
	int64Bits        = 64      // strconv bit size of expires_in values
)

// oauthEndpoints are a provider's URLs; tests point them at httptest servers.
type oauthEndpoints struct {
	authorize, token, jwks string
	issuers                []string
}

var (
	googleEndpoints = oauthEndpoints{
		authorize: "https://accounts.google.com/o/oauth2/v2/auth",
		token:     "https://oauth2.googleapis.com/token",
		jwks:      "https://www.googleapis.com/oauth2/v3/certs",
		issuers:   []string{"https://accounts.google.com", "accounts.google.com"},
	}
	appleEndpoints = oauthEndpoints{
		authorize: "https://appleid.apple.com/auth/authorize",
		token:     "https://appleid.apple.com/auth/token",
		jwks:      "https://appleid.apple.com/auth/keys",
		issuers:   []string{"https://appleid.apple.com"},
	}
)

// Provider ids, as in the callback path /callback/{id} and "account"."providerId".
const (
	ProviderGoogle = "google"
	ProviderApple  = "apple"
)

// oauthTokens is a token endpoint response.
type oauthTokens struct {
	AccessToken           string
	RefreshToken          string
	IDToken               string
	AccessTokenExpiresAt  *time.Time
	RefreshTokenExpiresAt *time.Time
	Scopes                []string
}

// socialUser is who the provider says signed in.
type socialUser struct {
	ID            string
	Email         string
	EmailVerified bool
	Name          string
	Image         *string
}

// socialProvider is one configured provider.
type socialProvider struct {
	id            string
	clientID      string
	scopes        []string
	pkce          bool // send a code_challenge (Google); Apple only gets the verifier
	extraAuth     [][2]string
	responseType  string
	endpoints     oauthEndpoints
	keys          *jwksCache
	clientSecret  func(now time.Time) (string, error)
	nameFromExtra func(userJSON string) string // Apple's first-sign-in "user" form field
	client        *http.Client
}

// socialProviders holds the configured providers and their rotating secrets.
type socialProviders struct {
	byID    map[string]*socialProvider
	secrets atomic.Pointer[SocialSecrets]
	apple   *appleSecretSigner
}

func newSocialProviders(grant *GoogleConfig, ap *AppleConfig, client *http.Client, now func() time.Time) (*socialProviders, error) {
	if client == nil {
		client = &http.Client{Timeout: providerTimeout}
	}
	sp := &socialProviders{byID: map[string]*socialProvider{}}
	var initial SocialSecrets
	if grant != nil {
		if grant.ClientID == "" || grant.ClientSecret == "" {
			return nil, errors.New("auth: GoogleConfig needs ClientID and ClientSecret")
		}
		ep := googleEndpoints
		if grant.endpoints != nil {
			ep = *grant.endpoints
		}
		initial.GoogleClientSecret = grant.ClientSecret
		sp.byID[ProviderGoogle] = &socialProvider{
			id: ProviderGoogle, clientID: grant.ClientID,
			scopes:       []string{"email", "profile", "openid"},
			pkce:         true,
			extraAuth:    [][2]string{{"include_granted_scopes", "true"}},
			responseType: "code",
			endpoints:    ep,
			keys:         newJWKSCache(ep.jwks, client, now),
			clientSecret: func(time.Time) (string, error) { return sp.secrets.Load().GoogleClientSecret, nil },
			client:       client,
		}
	}
	if ap != nil {
		if ap.ClientID == "" || ap.TeamID == "" || ap.KeyID == "" || ap.PrivateKey == "" {
			return nil, errors.New("auth: AppleConfig needs ClientID, TeamID, KeyID and PrivateKey")
		}
		if _, err := parseES256Key(ap.PrivateKey); err != nil {
			return nil, err
		}
		ep := appleEndpoints
		if ap.endpoints != nil {
			ep = *ap.endpoints
		}
		initial.ApplePrivateKey, initial.AppleKeyID = ap.PrivateKey, ap.KeyID
		sp.apple = &appleSecretSigner{clientID: ap.ClientID, teamID: ap.TeamID}
		sp.byID[ProviderApple] = &socialProvider{
			id: ProviderApple, clientID: ap.ClientID,
			scopes:       []string{"email", "name"},
			extraAuth:    [][2]string{{"response_mode", "form_post"}},
			responseType: "code id_token",
			endpoints:    ep,
			keys:         newJWKSCache(ep.jwks, client, now),
			clientSecret: func(now time.Time) (string, error) {
				value := sp.secrets.Load()
				return sp.apple.secret(value.ApplePrivateKey, value.AppleKeyID, now)
			},
			nameFromExtra: appleName,
			client:        client,
		}
	}
	sp.secrets.Store(&initial)
	return sp, nil
}

// appleSecretSigner signs Apple's client secret (an ES256 JWT valid for 180
// days, Apple's maximum) and reuses it until it is within a day of expiry or
// the key changes.
type appleSecretSigner struct {
	clientID, teamID string

	mu      sync.Mutex
	key     string // the key id and PEM the cached secret was signed with
	secretV string
	expires time.Time
}

const (
	appleSecretTTL         = 180 * 24 * time.Hour // Apple's maximum client secret lifetime (six months)
	appleSecretRenewBefore = 24 * time.Hour       // sign a new one when the cached one is this close to expiry
)

func (signer *appleSecretSigner) secret(pemKey, keyID string, now time.Time) (string, error) {
	signer.mu.Lock()
	defer signer.mu.Unlock()
	if signer.secretV != "" && signer.key == keyID+"\x00"+pemKey && now.Before(signer.expires.Add(-appleSecretRenewBefore)) {
		return signer.secretV, nil
	}
	key, err := parseES256Key(pemKey)
	if err != nil {
		return "", err
	}
	secret, err := appleClientSecret(key, keyID, signer.teamID, signer.clientID, now)
	if err != nil {
		return "", err
	}
	signer.key, signer.secretV, signer.expires = keyID+"\x00"+pemKey, secret, now.Add(appleSecretTTL)
	return secret, nil
}

func appleClientSecret(key *ecdsa.PrivateKey, keyID, teamID, clientID string, now time.Time) (string, error) {
	return signES256(key, keyID, map[string]any{
		"iss": teamID,
		"iat": now.Unix(),
		"exp": now.Add(appleSecretTTL).Unix(),
		"aud": "https://appleid.apple.com",
		"sub": clientID,
	})
}

// appleName reads the name Apple posts once, on the first sign-in:
// {"name":{"firstName":"..","lastName":".."},"email":".."}.
func appleName(userJSON string) string {
	var user struct {
		Name struct {
			FirstName string `json:"firstName"`
			LastName  string `json:"lastName"`
		} `json:"name"`
	}
	if userJSON == "" || json.Unmarshal([]byte(userJSON), &user) != nil {
		return ""
	}
	return strings.TrimSpace(user.Name.FirstName + " " + user.Name.LastName)
}

// pkceChallenge is the S256 code challenge of verifier.
func pkceChallenge(verifier string) string {
	sum := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

// authorizationURL builds the provider's authorization URL.
func (provider *socialProvider) authorizationURL(state, verifier, nonce, redirectURI string) string {
	query := url.Values{
		"response_type": {provider.responseType},
		"client_id":     {provider.clientID},
		"state":         {state},
		"nonce":         {nonce},
		"scope":         {strings.Join(provider.scopes, " ")},
		"redirect_uri":  {redirectURI},
	}
	if provider.pkce {
		query.Set("code_challenge_method", "S256")
		query.Set("code_challenge", pkceChallenge(verifier))
	}
	for _, kv := range provider.extraAuth {
		query.Set(kv[0], kv[1])
	}
	return provider.endpoints.authorize + "?" + query.Encode()
}

// exchange trades an authorization code for tokens (client_secret_post, with
// the PKCE verifier, which Better Auth sends to Apple too).
func (provider *socialProvider) exchange(ctx context.Context, code, verifier, redirectURI string, now time.Time) (oauthTokens, error) {
	secret, err := provider.clientSecret(now)
	if err != nil {
		return oauthTokens{}, err
	}
	form := url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {code},
		"code_verifier": {verifier},
		"redirect_uri":  {redirectURI},
		"client_id":     {provider.clientID},
		"client_secret": {secret},
	}
	ctx, cancel := context.WithTimeout(ctx, providerTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, provider.endpoints.token, strings.NewReader(form.Encode()))
	if err != nil {
		return oauthTokens{}, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	res, err := provider.client.Do(req)
	if err != nil {
		return oauthTokens{}, fmt.Errorf("auth: %s token endpoint: %w", provider.id, err)
	}
	defer func() { _ = res.Body.Close() }()
	var body struct {
		AccessToken           string      `json:"access_token"`
		RefreshToken          string      `json:"refresh_token"`
		IDToken               string      `json:"id_token"`
		ExpiresIn             json.Number `json:"expires_in"`
		RefreshTokenExpiresIn json.Number `json:"refresh_token_expires_in"`
		Scope                 string      `json:"scope"`
		Error                 string      `json:"error"`
	}
	if err := json.NewDecoder(io.LimitReader(res.Body, maxTokenResponse)).Decode(&body); err != nil {
		return oauthTokens{}, fmt.Errorf("auth: %s token endpoint: status %d: %w", provider.id, res.StatusCode, err)
	}
	if res.StatusCode != http.StatusOK || body.Error != "" || body.IDToken == "" {
		return oauthTokens{}, fmt.Errorf("auth: %s token endpoint: status %d %q", provider.id, res.StatusCode, body.Error)
	}
	tokens := oauthTokens{AccessToken: body.AccessToken, RefreshToken: body.RefreshToken, IDToken: body.IDToken, Scopes: strings.Fields(body.Scope)}
	if value, err := strconv.ParseInt(body.ExpiresIn.String(), decimal, int64Bits); err == nil && value > 0 {
		at := now.Add(time.Duration(value) * time.Second)
		tokens.AccessTokenExpiresAt = &at
	}
	if value, err := strconv.ParseInt(body.RefreshTokenExpiresIn.String(), decimal, int64Bits); err == nil && value > 0 {
		at := now.Add(time.Duration(value) * time.Second)
		tokens.RefreshTokenExpiresAt = &at
	}
	return tokens, nil
}

// userFromIDToken verifies the code flow's ID token (this client, the
// flow's nonce) and maps its claims.
func (provider *socialProvider) userFromIDToken(ctx context.Context, token, nonce, userJSON string, now time.Time) (socialUser, error) {
	claims, err := verifyIDToken(ctx, token, provider.keys, idTokenRules{issuers: provider.endpoints.issuers, audiences: []string{provider.clientID}, nonce: nonce}, now)
	if err != nil {
		return socialUser{}, err
	}
	user := socialUser{ID: claims.Subject, Email: claims.Email, EmailVerified: bool(claims.EmailVerified), Name: claims.Name}
	if claims.Picture != "" {
		user.Image = &claims.Picture
	}
	if provider.nameFromExtra != nil {
		if length := provider.nameFromExtra(userJSON); length != "" {
			user.Name = length
		}
	}
	return user, nil
}
