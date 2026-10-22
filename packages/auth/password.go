package auth

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"golang.org/x/crypto/argon2"
	"golang.org/x/crypto/scrypt"
	"golang.org/x/text/unicode/norm"
)

// argonParams are argon2id cost parameters. They are stored in each hash, so
// raising them only affects new hashes.
type argonParams struct {
	Memory  uint32 // KiB
	Time    uint32
	Threads uint8
	SaltLen uint32
	KeyLen  uint32
}

// defaultArgonParams follow RFC 9106's second recommended option (64 MiB, t=3).
var defaultArgonParams = argonParams{Memory: 64 * 1024, Time: 3, Threads: 4, SaltLen: 16, KeyLen: 32}

// testArgonParams replace the defaults in test binaries (see argonParamsFor).
// At production cost every package whose tests sign users up spent ~1 GB and
// seconds per test binary under -race. Each hash records its own parameters,
// so hashes made either way verify either way.
var testArgonParams = argonParams{Memory: 64, Time: 1, Threads: 1, SaltLen: 16, KeyLen: 32}

// argonParamsFor returns the parameters New uses: the cheap test ones only
// inside a `go test` binary (testing.Testing), which a production binary
// never is.
func argonParamsFor(inTestBinary bool) argonParams {
	if inTestBinary {
		return testArgonParams
	}
	return defaultArgonParams
}

var errMalformedHash = errors.New("auth: malformed password hash")

// hashPassword returns a PHC-format argon2id hash:
// $argon2id$v=19$m=65536,t=3,p=4$<salt>$<key> (unpadded standard base64).
func hashPassword(password string, p argonParams) (string, error) {
	salt := make([]byte, p.SaltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	key := argon2.IDKey([]byte(password), salt, p.Time, p.Memory, p.Threads, p.KeyLen)
	b64 := base64.RawStdEncoding
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s",
		argon2.Version, p.Memory, p.Time, p.Threads, b64.EncodeToString(salt), b64.EncodeToString(key)), nil
}

// verifyPassword reports whether password matches hash, comparing in constant
// time. It accepts argon2id hashes and Better Auth's scrypt hashes (see
// isLegacyHash), so users imported from a Better Auth database can sign in.
func verifyPassword(password, hash string) (bool, error) {
	if isLegacyHash(hash) {
		return verifyScrypt(password, hash)
	}
	parts := strings.Split(hash, "$")
	if len(parts) != 6 || parts[0] != "" || parts[1] != "argon2id" {
		return false, errMalformedHash
	}
	var version int
	if _, err := fmt.Sscanf(parts[2], "v=%d", &version); err != nil || version != argon2.Version {
		return false, errMalformedHash
	}
	var p argonParams
	if _, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &p.Memory, &p.Time, &p.Threads); err != nil {
		return false, errMalformedHash
	}
	if p.Memory == 0 || p.Time == 0 || p.Threads == 0 || p.Memory > 1<<22 || p.Time > 64 {
		return false, errMalformedHash
	}
	b64 := base64.RawStdEncoding
	salt, err := b64.DecodeString(parts[4])
	if err != nil || len(salt) == 0 {
		return false, errMalformedHash
	}
	want, err := b64.DecodeString(parts[5])
	if err != nil || len(want) == 0 || len(want) > 128 {
		return false, errMalformedHash
	}
	got := argon2.IDKey([]byte(password), salt, p.Time, p.Memory, p.Threads, uint32(len(want))) //nolint:gosec // len(want) <= 128, checked above
	return subtle.ConstantTimeCompare(got, want) == 1, nil
}

// Better Auth's scrypt parameters (@better-auth/utils/password).
const (
	scryptN      = 16384
	scryptR      = 16
	scryptP      = 1
	scryptKeyLen = 64
)

// isLegacyHash reports whether hash is a Better Auth scrypt hash,
// "<hex salt>:<hex key>". SignInEmail rehashes these with argon2id.
func isLegacyHash(hash string) bool {
	return !strings.HasPrefix(hash, "$") && strings.Count(hash, ":") == 1
}

// verifyScrypt checks a Better Auth scrypt hash. Like Better Auth it uses the
// hex salt string itself as the salt and NFKC-normalises the password.
func verifyScrypt(password, hash string) (bool, error) {
	salt, keyHex, _ := strings.Cut(hash, ":")
	want, err := hex.DecodeString(keyHex)
	if salt == "" || err != nil || len(want) != scryptKeyLen {
		return false, errMalformedHash
	}
	got, err := scrypt.Key([]byte(norm.NFKC.String(password)), []byte(salt), scryptN, scryptR, scryptP, scryptKeyLen)
	if err != nil {
		return false, err
	}
	return subtle.ConstantTimeCompare(got, want) == 1, nil
}

// checkPasswordLength applies the configured length rules, counting characters
// (not bytes) like Better Auth.
func (a *Auth) checkPasswordLength(password string) error {
	n := utf8.RuneCountInString(password)
	if n < a.cfg.MinPasswordLength {
		return ErrPasswordTooShort
	}
	if n > a.cfg.MaxPasswordLength {
		return ErrPasswordTooLong
	}
	return nil
}
