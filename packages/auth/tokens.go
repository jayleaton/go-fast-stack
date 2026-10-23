package auth

import (
	"crypto/hmac"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/url"
	"strings"
	"time"
)

const idAlphabet = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"

// Lengths passed to generateID, as Better Auth's generateId.
const (
	idLength         = 32 // row ids, session, challenge and trust-device tokens
	resetTokenLength = 24 // password reset tokens
)

// idReadSlack is extra random bytes per read beyond n/4, so one read almost
// always yields n accepted characters.
const idReadSlack = 8

// generateID returns a random [a-zA-Z0-9] string like Better Auth's generateId
// (32 chars for ids and session tokens, 24 for reset tokens).
func generateID(n int) string {
	// 248 is the largest multiple of 62 below 256: rejecting bytes >= 248
	// keeps every character equally likely.
	out := make([]byte, 0, n)
	buf := make([]byte, n+n/4+idReadSlack)
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

// signValue returns value + "." + base64(HMAC-SHA256(secret, value)), the
// better-call signed cookie format (before URL encoding).
func (a *Auth) signValue(value string) string {
	return value + "." + base64.StdEncoding.EncodeToString(a.mac(value))
}

// verifySigned checks a "value.signature" string and returns value. The input
// may still be URL-encoded (cookie or bearer header).
func (a *Auth) verifySigned(signed string) (string, bool) {
	v, ok, _ := a.verifySignedKey(signed)
	return v, ok
}

// verifySignedKey is verifySigned that also reports whether the signature
// used the current secret (false: re-sign it).
func (a *Auth) verifySignedKey(signed string) (value string, ok, current bool) {
	if strings.Contains(signed, "%") {
		decoded, err := url.PathUnescape(signed)
		if err != nil {
			return "", false, false
		}
		signed = decoded
	}
	i := strings.LastIndexByte(signed, '.')
	if i < 1 {
		return "", false, false
	}
	value, sig := signed[:i], signed[i+1:]
	if len(sig) != 44 || !strings.HasSuffix(sig, "=") {
		return "", false, false
	}
	got, err := base64.StdEncoding.DecodeString(sig)
	if err != nil {
		return "", false, false
	}
	if ok, current := a.checkMAC(value, got); ok {
		return value, true, current
	}
	return "", false, false
}

// parseBearer extracts the session token from an Authorization header value.
// Like Better Auth's bearer plugin it accepts the signed token (as returned in
// the set-auth-token header) or the raw session token (as returned in the
// sign-in body). A signed token with a bad signature is rejected; resign is
// true for a signed token made with a previous secret.
func (a *Auth) parseBearer(header string) (token string, ok, resign bool) {
	if len(header) < 7 || !strings.EqualFold(header[:7], "bearer ") {
		return "", false, false
	}
	token = strings.TrimSpace(header[7:])
	if token == "" {
		return "", false, false
	}
	if strings.Contains(token, ".") {
		v, ok, current := a.verifySignedKey(token)
		return v, ok, ok && !current
	}
	if !isTokenString(token) {
		return "", false, false
	}
	return token, true, false
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
// as in Better Auth: {"email": ..., "iat": ..., "exp": ...}. They also carry
// "cred", a fingerprint of the password hash the link was issued for, so a
// link stops working when someone else's sign-up replaces that password.
// Change-email links are Better Auth's "change-email-verification" tokens:
// {"email": <current>, "updateTo": <new>, "requestType": ...}, plus "uid" and
// a "jti" nonce that must still exist as a verification row, which makes them
// single-use and revocable.

var jwtHeader = base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"HS256","typ":"JWT"}`))

type verificationClaims struct {
	Email       string `json:"email"`
	UpdateTo    string `json:"updateTo,omitempty"`
	RequestType string `json:"requestType,omitempty"`
	Cred        string `json:"cred,omitempty"`
	UserID      string `json:"uid,omitempty"`
	Nonce       string `json:"jti,omitempty"`
	IssuedAt    int64  `json:"iat"`
	Expires     int64  `json:"exp"`
}

// credFingerprint identifies a password hash without revealing it ("" for
// users without a password).
func (a *Auth) credFingerprint(hash *string) string {
	if hash == nil || *hash == "" {
		return ""
	}
	return base64.RawURLEncoding.EncodeToString(a.mac("credential:" + *hash)[:16])
}

// credMatches reports whether cred is hash's fingerprint under an accepted
// secret (links issued before a rotation keep working).
func (a *Auth) credMatches(cred string, hash *string) bool {
	if hash == nil || *hash == "" {
		return cred == ""
	}
	got, err := base64.RawURLEncoding.DecodeString(cred)
	if err != nil || len(got) != 16 {
		return false
	}
	for _, k := range a.ring.Load().accepted(a.now()) {
		if hmac.Equal(got, hmacSum(k.secret, "credential:"+*hash)[:16]) {
			return true
		}
	}
	return false
}

// changeEmailRequest is Better Auth's requestType for a link sent to the new
// address that, when opened, switches the account to it.
const changeEmailRequest = "change-email-verification"

func (a *Auth) signVerificationToken(email, cred string, ttl time.Duration) string {
	return a.signClaims(verificationClaims{Email: strings.ToLower(email), Cred: cred}, ttl)
}

// signChangeEmailToken signs a change-email link moving userID from email to
// newEmail; nonce names its verification row.
func (a *Auth) signChangeEmailToken(userID, email, newEmail, nonce string, ttl time.Duration) string {
	return a.signClaims(verificationClaims{
		Email:       strings.ToLower(email),
		UpdateTo:    strings.ToLower(newEmail),
		RequestType: changeEmailRequest,
		UserID:      userID,
		Nonce:       nonce,
	}, ttl)
}

func (a *Auth) signClaims(c verificationClaims, ttl time.Duration) string {
	now := a.now()
	c.IssuedAt, c.Expires = now.Unix(), now.Add(ttl).Unix()
	payload, _ := json.Marshal(c) //nolint:gosec // cred is a keyed fingerprint, not a secret
	unsigned := jwtHeader + "." + base64.RawURLEncoding.EncodeToString(payload)
	return unsigned + "." + base64.RawURLEncoding.EncodeToString(a.mac(unsigned))
}

var errTokenExpired = errors.New("token expired")

// parseVerificationToken verifies the JWT and returns its claims with emails
// lowercased. It returns errTokenExpired for an expired but otherwise valid
// token.
func (a *Auth) parseVerificationToken(token string) (verificationClaims, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 || len(token) > 2048 {
		return verificationClaims{}, ErrInvalidToken
	}
	rawHeader, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return verificationClaims{}, ErrInvalidToken
	}
	var header struct {
		Alg string `json:"alg"`
	}
	if json.Unmarshal(rawHeader, &header) != nil || header.Alg != "HS256" {
		return verificationClaims{}, ErrInvalidToken
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return verificationClaims{}, ErrInvalidToken
	}
	if ok, _ := a.checkMAC(parts[0]+"."+parts[1], sig); !ok {
		return verificationClaims{}, ErrInvalidToken
	}
	rawClaims, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return verificationClaims{}, ErrInvalidToken
	}
	var claims verificationClaims
	if json.Unmarshal(rawClaims, &claims) != nil || claims.Email == "" || claims.Expires == 0 {
		return verificationClaims{}, ErrInvalidToken
	}
	// Only the change-email-verification flow may carry updateTo; Better
	// Auth's other change-email variants are not implemented.
	if (claims.UpdateTo != "") != (claims.RequestType == changeEmailRequest) {
		return verificationClaims{}, ErrInvalidToken
	}
	if a.now().Unix() >= claims.Expires {
		return verificationClaims{}, errTokenExpired
	}
	claims.Email, claims.UpdateTo = strings.ToLower(claims.Email), strings.ToLower(claims.UpdateTo)
	return claims, nil
}
