package auth

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha1" //nolint:gosec // RFC 6238 TOTP uses HMAC-SHA1; authenticator apps expect it
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base32"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"
)

// TOTP parameters every authenticator app supports (RFC 6238 defaults).
const (
	totpDigits = 6
	totpPeriod = 30 * time.Second
	totpWindow = 1 // steps accepted either side of now, for clock drift
)

var b32 = base32.StdEncoding.WithPadding(base32.NoPadding)

// newTOTPKey returns a random 160-bit TOTP key (RFC 4226 recommends >= 128).
func newTOTPKey() []byte {
	key := make([]byte, 20)
	_, _ = rand.Read(key)
	return key
}

// totpCode is the RFC 6238 code for key at time step.
func totpCode(key []byte, step int64) string {
	var msg [8]byte
	binary.BigEndian.PutUint64(msg[:], uint64(step)) //nolint:gosec // steps are positive
	m := hmac.New(sha1.New, key)
	m.Write(msg[:])
	sum := m.Sum(nil)
	off := sum[len(sum)-1] & 0x0f
	bin := binary.BigEndian.Uint32(sum[off:off+4]) & 0x7fffffff
	return fmt.Sprintf("%0*d", totpDigits, bin%1_000_000)
}

func totpStep(t time.Time) int64 { return t.Unix() / int64(totpPeriod/time.Second) }

// matchTOTP returns the time step code is valid for at now (within the
// window), or false. The caller rejects steps already used, so a code cannot
// be replayed while it is still valid.
func matchTOTP(key []byte, code string, now time.Time) (int64, bool) {
	code = strings.ReplaceAll(strings.TrimSpace(code), " ", "")
	if len(code) != totpDigits {
		return 0, false
	}
	cur := totpStep(now)
	found, matched := int64(0), 0
	for d := int64(-totpWindow); d <= totpWindow; d++ {
		// Check every step without early exit so timing does not reveal which matched.
		if subtle.ConstantTimeCompare([]byte(totpCode(key, cur+d)), []byte(code)) == 1 {
			found, matched = cur+d, 1
		}
	}
	return found, matched == 1
}

// totpURI is the otpauth:// URI authenticator apps read from a QR code.
func totpURI(key []byte, issuer, account string) string {
	label := url.PathEscape(issuer) + ":" + url.PathEscape(account)
	q := url.Values{}
	q.Set("secret", b32.EncodeToString(key))
	q.Set("issuer", issuer)
	q.Set("algorithm", "SHA1")
	q.Set("digits", fmt.Sprint(totpDigits))
	q.Set("period", fmt.Sprint(int(totpPeriod/time.Second)))
	return "otpauth://totp/" + label + "?" + q.Encode()
}

// derivedKey returns a 32-byte key for purpose, derived from the auth secret,
// so one AUTH_SECRET never doubles as two keys.
func (a *Auth) derivedKey(purpose string) []byte {
	m := hmac.New(sha256.New, []byte(a.cfg.Secret))
	m.Write([]byte("auth/" + purpose))
	return m.Sum(nil)
}

// sealTOTPKey encrypts a TOTP key with AES-256-GCM for storage, bound to the
// user so a row copied to another user does not decrypt.
func (a *Auth) sealTOTPKey(userID string, key []byte) (string, error) {
	gcm, err := a.totpCipher()
	if err != nil {
		return "", err
	}
	nonce := make([]byte, gcm.NonceSize())
	_, _ = rand.Read(nonce)
	return base64.RawStdEncoding.EncodeToString(gcm.Seal(nonce, nonce, key, []byte(userID))), nil
}

var errBadTOTPKey = errors.New("auth: stored TOTP secret does not decrypt (AUTH_SECRET changed?)")

func (a *Auth) openTOTPKey(userID, sealed string) ([]byte, error) {
	gcm, err := a.totpCipher()
	if err != nil {
		return nil, err
	}
	raw, err := base64.RawStdEncoding.DecodeString(sealed)
	if err != nil || len(raw) < gcm.NonceSize() {
		return nil, errBadTOTPKey
	}
	key, err := gcm.Open(nil, raw[:gcm.NonceSize()], raw[gcm.NonceSize():], []byte(userID))
	if err != nil {
		return nil, errBadTOTPKey
	}
	return key, nil
}

func (a *Auth) totpCipher() (cipher.AEAD, error) {
	block, err := aes.NewCipher(a.derivedKey("two-factor-secret"))
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

const backupCodeCount = 10

// newBackupCodes returns fresh one-time codes ("abcde-12345": 50 bits each).
func newBackupCodes() []string {
	codes := make([]string, backupCodeCount)
	for i := range codes {
		c := strings.ToLower(generateID(10))
		codes[i] = c[:5] + "-" + c[5:]
	}
	return codes
}

// hashBackupCode is the stored form of a backup code: a keyed hash bound to
// the user, so a database leak does not reveal usable codes.
func (a *Auth) hashBackupCode(userID, code string) string {
	code = strings.ToLower(strings.ReplaceAll(strings.TrimSpace(code), " ", ""))
	if len(code) == 10 && !strings.Contains(code, "-") {
		code = code[:5] + "-" + code[5:]
	}
	m := hmac.New(sha256.New, a.derivedKey("backup-code"))
	m.Write([]byte(userID + "\x00" + code))
	return base64.RawStdEncoding.EncodeToString(m.Sum(nil))
}
