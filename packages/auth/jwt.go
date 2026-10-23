package auth

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"
)

// Provider ID tokens (OIDC JWTs): verified with the provider's published
// keys (JWKS), RS256 or ES256 only, never "none" or HMAC. Standard library
// only, so social sign-in adds no dependency.

var errInvalidIDToken = errors.New("auth: invalid id token")

// idTokenClaims are the claims social sign-in reads from Google and Apple ID
// tokens. Apple sends email_verified as a boolean or the string "true".
type idTokenClaims struct {
	Issuer        string   `json:"iss"`
	Subject       string   `json:"sub"`
	Audience      audience `json:"aud"`
	Expires       int64    `json:"exp"`
	IssuedAt      int64    `json:"iat"`
	Nonce         string   `json:"nonce"`
	Email         string   `json:"email"`
	EmailVerified flexBool `json:"email_verified"`
	Name          string   `json:"name"`
	Picture       string   `json:"picture"`
}

// audience is the "aud" claim: one string or an array.
type audience []string

func (aud *audience) UnmarshalJSON(raw []byte) error {
	var one string
	if json.Unmarshal(raw, &one) == nil {
		*aud = audience{one}
		return nil
	}
	var many []string
	if err := json.Unmarshal(raw, &many); err != nil {
		return err
	}
	*aud = many
	return nil
}

// flexBool accepts true/false and "true"/"false".
type flexBool bool

func (flag *flexBool) UnmarshalJSON(raw []byte) error {
	switch strings.Trim(string(raw), `"`) {
	case "true":
		*flag = true
	default:
		*flag = false
	}
	return nil
}

// idTokenRules is what a provider's ID token must satisfy.
type idTokenRules struct {
	issuers   []string
	audiences []string // any of them
	nonce     string   // must match exactly (the flow always sends one)
}

const idTokenLeeway = time.Minute

// Sizes in compact JWS and JWK handling.
const (
	jwsSegments         = 3        // header.payload.signature
	maxIDTokenSize      = 16 << 10 // 16 KiB: longer tokens are refused before parsing
	p256CoordSize       = 32       // bytes in a P-256 coordinate and in each half of an ES256 signature
	es256SignatureSize  = 2 * p256CoordSize
	uncompressedPoint   = 4    // SEC 1 prefix byte of an uncompressed EC point
	maxRSAExponentBytes = 4    // an RSA public exponent that fits an int
	minRSAKeyBits       = 2048 // weaker provider keys are ignored
)

// verifyIDToken checks the signature against keys and the claims against
// rules at now.
func verifyIDToken(ctx context.Context, token string, keys *jwksCache, rules idTokenRules, now time.Time) (idTokenClaims, error) {
	parts := strings.Split(token, ".")
	if len(parts) != jwsSegments || len(token) > maxIDTokenSize {
		return idTokenClaims{}, errInvalidIDToken
	}
	var header struct {
		Alg string `json:"alg"`
		Kid string `json:"kid"`
	}
	if err := decodeSegment(parts[0], &header); err != nil {
		return idTokenClaims{}, errInvalidIDToken
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return idTokenClaims{}, errInvalidIDToken
	}
	key, err := keys.key(ctx, header.Kid)
	if err != nil {
		return idTokenClaims{}, err
	}
	digest := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	switch typed := key.(type) {
	case *rsa.PublicKey:
		if header.Alg != "RS256" || rsa.VerifyPKCS1v15(typed, crypto.SHA256, digest[:], sig) != nil {
			return idTokenClaims{}, errInvalidIDToken
		}
	case *ecdsa.PublicKey:
		if header.Alg != "ES256" || len(sig) != es256SignatureSize ||
			!ecdsa.Verify(typed, digest[:], new(big.Int).SetBytes(sig[:p256CoordSize]), new(big.Int).SetBytes(sig[p256CoordSize:])) {
			return idTokenClaims{}, errInvalidIDToken
		}
	default:
		return idTokenClaims{}, errInvalidIDToken
	}
	var claims idTokenClaims
	if err := decodeSegment(parts[1], &claims); err != nil {
		return idTokenClaims{}, errInvalidIDToken
	}
	switch {
	case !slices.Contains(rules.issuers, claims.Issuer),
		claims.Subject == "",
		!slices.ContainsFunc(claims.Audience, func(aud string) bool { return slices.Contains(rules.audiences, aud) }),
		claims.Expires == 0 || !now.Before(time.Unix(claims.Expires, 0).Add(idTokenLeeway)),
		claims.IssuedAt > now.Add(idTokenLeeway).Unix(),
		rules.nonce == "" || subtle.ConstantTimeCompare([]byte(claims.Nonce), []byte(rules.nonce)) != 1:
		return idTokenClaims{}, errInvalidIDToken
	}
	return claims, nil
}

func decodeSegment(seg string, value any) error {
	raw, err := base64.RawURLEncoding.DecodeString(seg)
	if err != nil {
		return err
	}
	return json.Unmarshal(raw, value)
}

// jwksCache fetches a provider's signing keys and keeps them for jwksTTL.
// An unknown kid refetches (key rotation), at most once per jwksMinRefetch;
// fetches are serialized, so an outage costs one request per minute.
type jwksCache struct {
	url    string
	client *http.Client
	now    func() time.Time

	mu        sync.Mutex
	keys      map[string]crypto.PublicKey
	fetched   time.Time // last successful fetch
	attempted time.Time // last fetch, successful or not
}

const (
	jwksTTL        = time.Hour   // how long fetched keys are trusted
	jwksMinRefetch = time.Minute // at most one fetch per this, even while fetches fail
	jwksMaxBody    = 256 << 10   // 256 KiB cap on a provider's key set
	jwksTimeout    = 10 * time.Second
)

func newJWKSCache(url string, client *http.Client, now func() time.Time) *jwksCache {
	return &jwksCache{url: url, client: client, now: now}
}

func (cache *jwksCache) key(ctx context.Context, kid string) (crypto.PublicKey, error) {
	cache.mu.Lock()
	defer cache.mu.Unlock()
	now := cache.now()
	if known, ok := cache.keys[kid]; ok && now.Sub(cache.fetched) < jwksTTL {
		return known, nil
	}
	if cache.attempted.IsZero() || now.Sub(cache.attempted) >= jwksMinRefetch {
		// One fetch per jwksMinRefetch, failed or not, detached from the
		// caller so one cancelled request cannot fail it for the rest.
		cache.attempted = now
		keys, err := cache.fetch(context.WithoutCancel(ctx))
		if err != nil {
			if known, ok := cache.keys[kid]; ok { // provider down: keep using what we have
				return known, nil
			}
			return nil, err
		}
		cache.keys, cache.fetched = keys, now
	}
	if known, ok := cache.keys[kid]; ok {
		return known, nil
	}
	return nil, errInvalidIDToken
}

func (cache *jwksCache) fetch(ctx context.Context) (map[string]crypto.PublicKey, error) {
	ctx, cancel := context.WithTimeout(ctx, jwksTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, cache.url, nil)
	if err != nil {
		return nil, err
	}
	res, err := cache.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("auth: fetching JWKS: %w", err)
	}
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("auth: fetching JWKS: status %d", res.StatusCode)
	}
	var set struct {
		Keys []struct {
			Kty      string `json:"kty"`
			Kid      string `json:"kid"`
			Use      string `json:"use"`
			Modulus  string `json:"n"`
			Exponent string `json:"e"`
			Crv      string `json:"crv"`
			CurveX   string `json:"x"`
			CurveY   string `json:"y"`
		} `json:"keys"`
	}
	if err := json.NewDecoder(io.LimitReader(res.Body, jwksMaxBody)).Decode(&set); err != nil {
		return nil, fmt.Errorf("auth: decoding JWKS: %w", err)
	}
	keys := map[string]crypto.PublicKey{}
	b64 := base64.RawURLEncoding
	for _, jwk := range set.Keys {
		if jwk.Use != "" && jwk.Use != "sig" {
			continue
		}
		switch jwk.Kty {
		case "RSA":
			modulus, err1 := b64.DecodeString(jwk.Modulus)
			exponent, err2 := b64.DecodeString(jwk.Exponent)
			if err1 != nil || err2 != nil || len(exponent) > maxRSAExponentBytes {
				continue
			}
			pub := &rsa.PublicKey{N: new(big.Int).SetBytes(modulus), E: int(new(big.Int).SetBytes(exponent).Int64())}
			if pub.N.BitLen() < minRSAKeyBits {
				continue
			}
			keys[jwk.Kid] = pub
		case "EC":
			coordX, err1 := b64.DecodeString(jwk.CurveX)
			coordY, err2 := b64.DecodeString(jwk.CurveY)
			if err1 != nil || err2 != nil || jwk.Crv != "P-256" {
				continue
			}
			raw := append(append([]byte{uncompressedPoint}, leftPad(coordX, p256CoordSize)...), leftPad(coordY, p256CoordSize)...)
			pub, err := ecdsa.ParseUncompressedPublicKey(elliptic.P256(), raw)
			if err != nil {
				continue
			}
			keys[jwk.Kid] = pub
		}
	}
	return keys, nil
}

func leftPad(raw []byte, length int) []byte {
	if len(raw) >= length {
		return raw
	}
	return append(make([]byte, length-len(raw)), raw...)
}

// parseES256Key reads a PKCS#8 PEM P-256 private key (Apple's .p8 file).
// Literal "\n" sequences are turned into newlines, as env vars often carry
// them that way.
func parseES256Key(pemText string) (*ecdsa.PrivateKey, error) {
	block, _ := pem.Decode([]byte(strings.ReplaceAll(pemText, `\n`, "\n")))
	if block == nil {
		return nil, errors.New("auth: Apple private key is not PEM")
	}
	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("auth: Apple private key: %w", err)
	}
	ec, ok := parsed.(*ecdsa.PrivateKey)
	if !ok || ec.Curve != elliptic.P256() {
		return nil, errors.New("auth: Apple private key must be an ES256 (P-256) key")
	}
	return ec, nil
}

// signES256 returns a compact JWS of claims signed with key (header alg
// ES256 and kid), with the signature as raw r||s like Node's ieee-p1363.
func signES256(key *ecdsa.PrivateKey, kid string, claims any) (string, error) {
	header, _ := json.Marshal(map[string]string{"alg": "ES256", "kid": kid})
	payload, err := json.Marshal(claims)
	if err != nil {
		return "", err
	}
	b64 := base64.RawURLEncoding
	unsigned := b64.EncodeToString(header) + "." + b64.EncodeToString(payload)
	digest := sha256.Sum256([]byte(unsigned))
	sigR, sigS, err := ecdsa.Sign(rand.Reader, key, digest[:])
	if err != nil {
		return "", err
	}
	sig := make([]byte, es256SignatureSize)
	sigR.FillBytes(sig[:p256CoordSize])
	sigS.FillBytes(sig[p256CoordSize:])
	return unsigned + "." + b64.EncodeToString(sig), nil
}
