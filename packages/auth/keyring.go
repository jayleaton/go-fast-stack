package auth

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"
)

// AUTH_SECRET rotation without signing anyone out.
//
// Signing: everything is signed with the current secret; signatures made with
// a previous secret (Config.PreviousSecrets, SetSecrets) are still accepted
// until SecretGrace after that secret was retired. The retirement time is
// stored in "authKey", so every replica enforces the same window, and a secret
// that has expired can't be brought back by listing it again. Session and
// trust-device cookies (and signed bearer tokens) that verify with a previous
// secret are re-signed with the current one on use.
//
// Data at rest: TOTP secrets and backup-code hashes use data keys stored
// wrapped by the auth secret. A rotation rewraps those two rows, so 2FA keeps
// working however often the secret changes. The data keys start out equal to
// the keys earlier releases derived from the secret that sealed the existing
// rows, so no stored row is rewritten and a rolling deploy stays consistent.
// AUTH_SECRET_REVOKED names secrets that must never verify again but may
// still be needed to unwrap a data key after an emergency rotation.

const (
	purposeTOTP   = "two-factor-secret"
	purposeBackup = "backup-code"
)

type signingKey struct {
	secret []byte
	kid    string
	until  time.Time // previous keys: accepted while now < until; zero for the current key
}

type keyring struct {
	current  signingKey
	previous []signingKey
}

// dataKeys are the loaded data keys and, per purpose, the kid of the secret
// that wraps the stored copy.
type dataKeys struct {
	totp, backup []byte
	wrappedBy    map[string]string
}

// accepted returns the keys a signature may verify with at now, current first.
func (r *keyring) accepted(now time.Time) []signingKey {
	out := []signingKey{r.current}
	for _, k := range r.previous {
		if now.Before(k.until) {
			out = append(out, k)
		}
	}
	return out
}

// kidOf names a secret without revealing it (safe to log and store).
func kidOf(secret []byte) string {
	return base64.RawURLEncoding.EncodeToString(hmacSum(secret, "auth/key-id")[:12])
}

func hmacSum(key []byte, value string) []byte {
	m := hmac.New(sha256.New, key)
	m.Write([]byte(value))
	return m.Sum(nil)
}

// mac signs value with the current secret.
func (a *Auth) mac(value string) []byte { return hmacSum(a.ring.Load().current.secret, value) }

// checkMAC reports whether sig is value's MAC under an accepted secret, and
// whether that secret is the current one.
func (a *Auth) checkMAC(value string, sig []byte) (ok, current bool) {
	for i, k := range a.ring.Load().accepted(a.now()) {
		if hmac.Equal(sig, hmacSum(k.secret, value)) {
			return true, i == 0
		}
	}
	return false, false
}

// dataKey returns the key protecting purpose's data at rest.
func (a *Auth) dataKey(purpose string) []byte {
	if purpose == purposeBackup {
		return a.data.Load().backup
	}
	return a.data.Load().totp
}

// legacyDataKey is how data keys were derived before "authKey" existed.
func legacyDataKey(secret []byte, purpose string) []byte {
	return hmacSum(secret, "auth/"+purpose)
}

// Secrets is the full set of auth secrets for SetSecrets.
type Secrets struct {
	Current  string   // AUTH_SECRET: signs; wraps the data keys
	Previous []string // AUTH_SECRET_PREVIOUS: still verify until SecretGrace after they were first retired
	Revoked  []string // AUTH_SECRET_REVOKED: never verify again; only unwrap data keys after an emergency rotation
}

// SetSecrets replaces the auth secrets while the server runs. Current signs
// from now on; Previous still verify until SecretGrace after they were first
// retired; anything else, including a replaced current secret listed
// nowhere, is rejected at once (the emergency revoke: users sign in again).
// The data-at-rest keys are rewrapped with Current, so 2FA keeps working.
//
// The key state is stored in one transaction. If that fails, the new keyring
// still takes effect in this process (a revoke must never wait on the
// database) and the error is returned so the caller retries.
func (a *Auth) SetSecrets(ctx context.Context, s Secrets) error {
	if err := validateSecrets(s); err != nil {
		return err
	}
	if err := a.syncKeys(ctx, s); err != nil {
		return err
	}
	r := a.ring.Load()
	a.log.Info("auth: secrets updated", "current_kid", r.current.kid, "accepted_previous", len(r.accepted(a.now()))-1, "revoked", len(s.Revoked), "grace", a.cfg.SecretGrace)
	return nil
}

const minSecretLength = 32 // bytes, for every current, previous and revoked secret

func validateSecrets(s Secrets) error {
	if len(s.Current) < minSecretLength {
		return errors.New("auth: Secret must be at least 32 bytes")
	}
	for i, p := range s.Previous {
		if len(p) < minSecretLength {
			return fmt.Errorf("auth: PreviousSecrets[%d] must be at least 32 bytes", i)
		}
	}
	for i, p := range s.Revoked {
		if len(p) < minSecretLength {
			return fmt.Errorf("auth: RevokedSecrets[%d] must be at least 32 bytes", i)
		}
	}
	return nil
}

// plan is a keyring about to be published, plus what must be marked revoked.
type plan struct {
	ring        *keyring
	revoked     []signingKey // marked expired in "authKey"; still unwrap data keys
	unlistedKid string       // the replaced current secret when nothing lists it
}

func (a *Auth) planKeys(s Secrets, now time.Time) plan {
	key := func(secret string) signingKey { return signingKey{secret: []byte(secret), kid: kidOf([]byte(secret))} }
	p := plan{ring: &keyring{current: key(s.Current)}}
	seen := map[string]bool{p.ring.current.kid: true}
	for _, r := range s.Revoked { // revoked wins over previous
		if k := key(r); !seen[k.kid] {
			seen[k.kid] = true
			p.revoked = append(p.revoked, k)
		}
	}
	for _, prev := range s.Previous {
		if k := key(prev); !seen[k.kid] {
			seen[k.kid] = true
			k.until = now.Add(a.cfg.SecretGrace) // provisional; persistKeys replaces it with the stored window
			p.ring.previous = append(p.ring.previous, k)
		}
	}
	if old := a.ring.Load(); old != nil && !seen[old.current.kid] {
		p.unlistedKid = old.current.kid
		p.revoked = append(p.revoked, old.current)
	}
	return p
}

// syncKeys stores the key state, loads the data keys and publishes both.
func (a *Auth) syncKeys(ctx context.Context, s Secrets) error {
	a.keysMu.Lock()
	defer a.keysMu.Unlock()
	now := a.now()
	p := a.planKeys(s, now)
	if p.unlistedKid != "" {
		a.log.Warn("auth: AUTH_SECRET changed and AUTH_SECRET_PREVIOUS does not list the old value: everything it signed is rejected now (users sign in again)",
			"revoked_kid", p.unlistedKid)
	}
	data, err := a.persistKeys(ctx, p, now)
	if err != nil {
		if a.ring.Load() != nil { // running: apply in memory anyway; the data keys in memory stay valid
			a.ring.Store(p.ring)
		}
		return err
	}
	a.ring.Store(p.ring)
	a.data.Store(data)
	return nil
}

func (a *Auth) persistKeys(ctx context.Context, p plan, now time.Time) (*dataKeys, error) {
	tx, err := a.store.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("auth: store key state: %w", err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	expired := now.Add(-a.cfg.SecretGrace - time.Second)

	// Current: promoting a secret still inside its window (pre-staged, or a
	// rollback) clears its retirement; an expired or revoked one stays so.
	if _, err := tx.Exec(ctx, `
		INSERT INTO "authKey" ("id", "createdAt", "updatedAt") VALUES ($1, $2, $2)
		ON CONFLICT ("id") DO UPDATE SET "updatedAt" = $2,
			"retiredAt" = CASE WHEN "authKey"."retiredAt" > $3 THEN NULL ELSE "authKey"."retiredAt" END`,
		"secret:"+p.ring.current.kid, now, expired); err != nil {
		return nil, fmt.Errorf("auth: record current secret: %w", err)
	}
	for i := range p.ring.previous {
		k := &p.ring.previous[i]
		var retired time.Time
		if err := tx.QueryRow(ctx, `
			INSERT INTO "authKey" ("id", "retiredAt", "createdAt", "updatedAt") VALUES ($1, $2, $2, $2)
			ON CONFLICT ("id") DO UPDATE SET "retiredAt" = COALESCE("authKey"."retiredAt", EXCLUDED."retiredAt"), "updatedAt" = EXCLUDED."updatedAt"
			RETURNING "retiredAt"`, "secret:"+k.kid, now).Scan(&retired); err != nil {
			return nil, fmt.Errorf("auth: record previous secret: %w", err)
		}
		k.until = retired.Add(a.cfg.SecretGrace)
	}
	for _, k := range p.revoked {
		if _, err := tx.Exec(ctx, `
			INSERT INTO "authKey" ("id", "retiredAt", "createdAt", "updatedAt") VALUES ($1, $2, $3, $3)
			ON CONFLICT ("id") DO UPDATE SET "retiredAt" = LEAST(COALESCE("authKey"."retiredAt", EXCLUDED."retiredAt"), EXCLUDED."retiredAt"), "updatedAt" = $3`,
			"secret:"+k.kid, expired, now); err != nil {
			return nil, fmt.Errorf("auth: record revoked secret: %w", err)
		}
	}

	data, err := a.loadDataKeys(ctx, tx, p)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("auth: store key state: %w", err)
	}
	return data, nil
}

func (a *Auth) loadDataKeys(ctx context.Context, q querier, p plan) (*dataKeys, error) {
	ring := p.ring
	var initial []byte // secret the first data keys derive from, looked up once
	keys, wrappedBy := map[string][]byte{}, map[string]string{}
	for _, purpose := range []string{purposeTOTP, purposeBackup} {
		id := "data:" + a.cfg.CookiePrefix + ":" + purpose
		kid, wrapped, err := readDataKey(ctx, q, id)
		if errors.Is(err, pgx.ErrNoRows) {
			if initial == nil {
				if initial, err = a.initialSecret(ctx, q, p); err != nil {
					return nil, err
				}
			}
			// Derived, not random: replicas still on the previous release
			// seal new 2FA rows with exactly this key during a rolling deploy.
			w, werr := wrapKey(ring.current.secret, id, legacyDataKey(initial, purpose))
			if werr != nil {
				return nil, werr
			}
			if _, werr := q.Exec(ctx, `
				INSERT INTO "authKey" ("id", "kid", "wrapped", "createdAt", "updatedAt") VALUES ($1, $2, $3, now(), now())
				ON CONFLICT ("id") DO NOTHING`, id, ring.current.kid, w); werr != nil {
				return nil, fmt.Errorf("auth: store data key: %w", werr)
			}
			kid, wrapped, err = readDataKey(ctx, q, id) // another replica may have won the insert
		}
		if err != nil {
			return nil, fmt.Errorf("auth: read data key: %w", err)
		}
		key, err := a.unwrapDataKey(p, purpose, id, kid, wrapped)
		if err != nil {
			return nil, err
		}
		if kid != ring.current.kid { // wrapped by an older secret: move it to the current one
			w, err := wrapKey(ring.current.secret, id, key)
			if err != nil {
				return nil, err
			}
			if _, err := q.Exec(ctx, `
				UPDATE "authKey" SET "kid" = $2, "wrapped" = $3, "updatedAt" = now() WHERE "id" = $1 AND "kid" = $4`,
				id, ring.current.kid, w, kid); err != nil {
				return nil, fmt.Errorf("auth: rewrap data key: %w", err)
			}
		}
		keys[purpose], wrappedBy[purpose] = key, ring.current.kid
	}
	return &dataKeys{totp: keys[purposeTOTP], backup: keys[purposeBackup], wrappedBy: wrappedBy}, nil
}

// unwrapDataKey opens a data key with the secret named by kid: current,
// previous (expired or not) or revoked. A running process still holds the
// key it loaded last time (whatever secret wrapped it then), so an emergency
// rotation keeps 2FA working even before AUTH_SECRET_REVOKED is set.
func (a *Auth) unwrapDataKey(p plan, purpose, id, kid, wrapped string) ([]byte, error) {
	for _, k := range slices.Concat([]signingKey{p.ring.current}, p.ring.previous, p.revoked) {
		if k.kid == kid {
			return unwrapKey(k.secret, id, wrapped)
		}
	}
	if loaded := a.data.Load(); loaded != nil && loaded.wrappedBy[purpose] == kid {
		return a.dataKey(purpose), nil
	}
	return nil, fmt.Errorf("auth: the %s key is wrapped by a secret this process doesn't have (kid %s): list it in AUTH_SECRET_PREVIOUS, or in AUTH_SECRET_REVOKED if it leaked", purpose, kid)
}

func readDataKey(ctx context.Context, q querier, id string) (kid, wrapped string, err error) {
	var k, w *string
	err = q.QueryRow(ctx, `SELECT "kid", "wrapped" FROM "authKey" WHERE "id" = $1`, id).Scan(&k, &w)
	if err == nil && (k == nil || w == nil) {
		err = fmt.Errorf("auth: data key %s is incomplete", id)
	}
	if err != nil {
		return "", "", err
	}
	return *k, *w, nil
}

// initialSecret picks the secret the first data keys derive from: the one
// that sealed most of a sample of existing 2FA rows (written before "authKey"
// existed), preferring the current secret; the current one when there are none.
func (a *Auth) initialSecret(ctx context.Context, q querier, p plan) ([]byte, error) {
	rows, err := q.Query(ctx, `SELECT "userId", "secret" FROM "twoFactor" ORDER BY "createdAt" DESC LIMIT 100`)
	if err != nil {
		return nil, fmt.Errorf("auth: inspect two-factor rows: %w", err)
	}
	type row struct{ userID, sealed string }
	sample, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (row, error) {
		var x row
		return x, r.Scan(&x.userID, &x.sealed)
	})
	if err != nil {
		return nil, fmt.Errorf("auth: inspect two-factor rows: %w", err)
	}
	if len(sample) == 0 {
		return p.ring.current.secret, nil
	}
	best, bestN := p.ring.current.secret, 0
	for _, k := range slices.Concat([]signingKey{p.ring.current}, p.ring.previous, p.revoked) {
		n := 0
		for _, r := range sample {
			if _, err := openSealed(legacyDataKey(k.secret, purposeTOTP), r.userID, r.sealed); err == nil {
				n++
			}
		}
		if n > bestN {
			best, bestN = k.secret, n
		}
	}
	if bestN == 0 {
		a.log.Warn("auth: existing two-factor secrets decrypt with no configured auth secret; those users must re-enrol")
	}
	return best, nil
}

// wrapKey seals key under a key-encryption key derived from secret, bound to id.
func wrapKey(secret []byte, id string, key []byte) (string, error) {
	gcm, err := gcmFor(hmacSum(secret, "auth/key-wrap"))
	if err != nil {
		return "", err
	}
	nonce := make([]byte, gcm.NonceSize())
	_, _ = rand.Read(nonce)
	return base64.RawStdEncoding.EncodeToString(gcm.Seal(nonce, nonce, key, []byte(id))), nil
}

func unwrapKey(secret []byte, id, wrapped string) ([]byte, error) {
	key, err := openSealed(hmacSum(secret, "auth/key-wrap"), id, wrapped)
	if err != nil {
		return nil, fmt.Errorf("auth: data key %s does not unwrap", id)
	}
	return key, nil
}

// openSealed opens base64(nonce || AES-GCM ciphertext) with aad.
func openSealed(key []byte, aad, sealed string) ([]byte, error) {
	gcm, err := gcmFor(key)
	if err != nil {
		return nil, err
	}
	raw, err := base64.RawStdEncoding.DecodeString(sealed)
	if err != nil || len(raw) < gcm.NonceSize() {
		return nil, errors.New("auth: malformed sealed value")
	}
	return gcm.Open(nil, raw[:gcm.NonceSize()], raw[gcm.NonceSize():], []byte(aad))
}

func gcmFor(key []byte) (cipher.AEAD, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}
