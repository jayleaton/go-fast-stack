package auth

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/url"
	"strings"
	"time"
)

const idAlphabet = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"

// generateID returns a random [a-zA-Z0-9] string like Better Auth's generateId
// (32 chars for ids and session tokens, 24 for reset tokens).
func generateID(n int) string {
	// 248 is the largest multiple of 62 below 256: rejecting bytes >= 248
	// keeps every character equally likely.
	out := make([]byte, 0, n)
	buf := make([]byte, n+n/4+8)
	for len(out) < n {
		if _, err := rand.Read(buf); err != nil {
			panic(err) // crypto/rand.Read never fails on supported platforms
		}
		for _, b := range buf {
			if b < 248 && len(out) < n {
				out = append(out, idAlphabet[b%62])
			}
		}
	}
	return string(out)
}

func (a *Auth) mac(value string) []byte {
	m := hmac.New(sha256.New, []byte(a.cfg.Secret))
	m.Write([]byte(value))
	return m.Sum(nil)
}

// signValue returns value + "." + base64(HMAC-SHA256(secret, value)), the
// better-call signed cookie format (before URL encoding).
func (a *Auth) signValue(value string) string {
	return value + "." + base64.StdEncoding.EncodeToString(a.mac(value))
}

// verifySigned checks a "value.signature" string and returns value. The input
// may still be URL-encoded (cookie or bearer header).
func (a *Auth) verifySigned(signed string) (string, bool) {
	if strings.Contains(signed, "%") {
		decoded, err := url.PathUnescape(signed)
		if err != nil {
			return "", false
		}
		signed = decoded
	}
	i := strings.LastIndexByte(signed, '.')
	if i < 1 {
		return "", false
	}
	value, sig := signed[:i], signed[i+1:]
	if len(sig) != 44 || !strings.HasSuffix(sig, "=") {
		return "", false
	}
	got, err := base64.StdEncoding.DecodeString(sig)
	if err != nil || !hmac.Equal(got, a.mac(value)) {
		return "", false
	}
	return value, true
}

// parseBearer extracts the session token from an Authorization header value.
// Like Better Auth's bearer plugin it accepts the signed token (as returned in
// the set-auth-token header) or the raw session token (as returned in the
// sign-in body). A signed token with a bad signature is rejected.
func (a *Auth) parseBearer(header string) (string, bool) {
	if len(header) < 7 || !strings.EqualFold(header[:7], "bearer ") {
		return "", false
	}
	token := strings.TrimSpace(header[7:])
	if token == "" {
		return "", false
	}
	if strings.Contains(token, ".") {
		return a.verifySigned(token)
	}
	if !isTokenString(token) {
		return "", false
	}
	return token, true
}

// isTokenString reports whether s looks like a generated token, so garbage
// never reaches the database.
func isTokenString(s string) bool {
	if len(s) == 0 || len(s) > 255 {
		return false
	}
	for i := range len(s) {
		if !strings.ContainsRune(idAlphabet, rune(s[i])) {
			return false
		}
	}
	return true
}

// Email verification tokens are stateless HS256 JWTs signed with the secret,
// as in Better Auth: {"email": ..., "iat": ..., "exp": ...}.

var jwtHeader = base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"HS256","typ":"JWT"}`))

type verificationClaims struct {
	Email    string `json:"email"`
	UpdateTo string `json:"updateTo,omitempty"`
	IssuedAt int64  `json:"iat"`
	Expires  int64  `json:"exp"`
}

func (a *Auth) signVerificationToken(email string, ttl time.Duration) string {
	now := a.now()
	payload, _ := json.Marshal(verificationClaims{
		Email:    strings.ToLower(email),
		IssuedAt: now.Unix(),
		Expires:  now.Add(ttl).Unix(),
	})
	unsigned := jwtHeader + "." + base64.RawURLEncoding.EncodeToString(payload)
	return unsigned + "." + base64.RawURLEncoding.EncodeToString(a.mac(unsigned))
}

var errTokenExpired = errors.New("token expired")

// parseVerificationToken verifies the JWT and returns its email. It returns
// errTokenExpired for an expired but otherwise valid token.
func (a *Auth) parseVerificationToken(token string) (string, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 || len(token) > 2048 {
		return "", ErrInvalidToken
	}
	rawHeader, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return "", ErrInvalidToken
	}
	var header struct {
		Alg string `json:"alg"`
	}
	if json.Unmarshal(rawHeader, &header) != nil || header.Alg != "HS256" {
		return "", ErrInvalidToken
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil || !hmac.Equal(sig, a.mac(parts[0]+"."+parts[1])) {
		return "", ErrInvalidToken
	}
	rawClaims, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return "", ErrInvalidToken
	}
	var claims verificationClaims
	if json.Unmarshal(rawClaims, &claims) != nil || claims.Email == "" || claims.Expires == 0 {
		return "", ErrInvalidToken
	}
	// Change-email tokens (updateTo) belong to a flow this package does not
	// implement yet; refuse them rather than half-handle them.
	if claims.UpdateTo != "" {
		return "", ErrInvalidToken
	}
	if a.now().Unix() >= claims.Expires {
		return "", errTokenExpired
	}
	return strings.ToLower(claims.Email), nil
}
