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

	totpKeyLength  = 20         // bytes: a 160-bit key, the size of an HMAC-SHA1 output
	totpOffsetMask = 0x0f       // RFC 4226 dynamic truncation: offset is the last byte's low nibble
	totpCodeMask   = 0x7fffffff // RFC 4226: keep the low 31 bits (drop the sign bit)
	totpModulus    = 1_000_000  // 10^totpDigits
)

var b32 = base32.StdEncoding.WithPadding(base32.NoPadding)

// newTOTPKey returns a random 160-bit TOTP key (RFC 4226 recommends >= 128).
func newTOTPKey() []byte {
	key := make([]byte, totpKeyLength)
	_, _ = rand.Read(key)
	return key
}

// totpCode is the RFC 6238 code for key at time step.
func totpCode(key []byte, step int64) string {
	var msg [8]byte
	binary.BigEndian.PutUint64(msg[:], uint64(step)) //nolint:gosec // steps are positive
	mac := hmac.New(sha1.New, key)
	mac.Write(msg[:])
	sum := mac.Sum(nil)
	off := sum[len(sum)-1] & totpOffsetMask
	bin := binary.BigEndian.Uint32(sum[off:off+4]) & totpCodeMask
	return fmt.Sprintf("%0*d", totpDigits, bin%totpModulus)
}

func totpStep(moment time.Time) int64 { return moment.Unix() / int64(totpPeriod/time.Second) }

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
	for digit := int64(-totpWindow); digit <= totpWindow; digit++ {
		// Check every step without early exit so timing does not reveal which matched.
		if subtle.ConstantTimeCompare([]byte(totpCode(key, cur+digit)), []byte(code)) == 1 {
			found, matched = cur+digit, 1
		}
	}
	return found, matched == 1
}

// totpURI is the otpauth:// URI authenticator apps read from a QR code.
func totpURI(key []byte, issuer, account string) string {
	label := url.PathEscape(issuer) + ":" + url.PathEscape(account)
	values := url.Values{}
	values.Set("secret", b32.EncodeToString(key))
	values.Set("issuer", issuer)
	values.Set("algorithm", "SHA1")
	values.Set("digits", fmt.Sprint(totpDigits))
	values.Set("period", fmt.Sprint(int(totpPeriod/time.Second)))
	return "otpauth://totp/" + label + "?" + values.Encode()
}

// sealTOTPKey encrypts a TOTP key with AES-256-GCM for storage, bound to the
// user so a row copied to another user does not decrypt.
func (auth *Auth) sealTOTPKey(userID string, key []byte) (string, error) {
	gcm, err := auth.totpCipher()
	if err != nil {
		return "", err
	}
	nonce := make([]byte, gcm.NonceSize())
	_, _ = rand.Read(nonce)
	return base64.RawStdEncoding.EncodeToString(gcm.Seal(nonce, nonce, key, []byte(userID))), nil
}

var errBadTOTPKey = errors.New("auth: stored TOTP secret does not decrypt")

func (auth *Auth) openTOTPKey(userID, sealed string) ([]byte, error) {
	gcm, err := auth.totpCipher()
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

func (auth *Auth) totpCipher() (cipher.AEAD, error) {
	block, err := aes.NewCipher(auth.dataKey(purposeTOTP))
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

const (
	backupCodeCount  = 10
	backupCodeLength = 10 // characters, split 5-5 by a dash
)

// newBackupCodes returns fresh one-time codes ("abcde-12345": 50 bits each).
func newBackupCodes() []string {
	codes := make([]string, backupCodeCount)
	for index := range codes {
		code := strings.ToLower(generateID(backupCodeLength))
		codes[index] = code[:5] + "-" + code[5:]
	}
	return codes
}

// hashBackupCode is the stored form of a backup code: a keyed hash bound to
// the user, so a database leak does not reveal usable codes.
func (auth *Auth) hashBackupCode(userID, code string) string {
	code = strings.ToLower(strings.ReplaceAll(strings.TrimSpace(code), " ", ""))
	if len(code) == 10 && !strings.Contains(code, "-") {
		code = code[:5] + "-" + code[5:]
	}
	mac := hmac.New(sha256.New, auth.dataKey(purposeBackup))
	mac.Write([]byte(userID + "\x00" + code))
	return base64.RawStdEncoding.EncodeToString(mac.Sum(nil))
}
