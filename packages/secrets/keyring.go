package secrets

import "strings"

// Keyring is a signing secret together with the values it replaced, read
// from three names: NAME (current, signs), NAME_PREVIOUS (still verify during
// a grace period) and NAME_REVOKED (must never verify again; kept only so
// data they protected can be re-keyed). Lists are comma-separated, newest
// first. Every value is trimmed.
type Keyring struct {
	Current  Secret
	Previous []Secret
	Revoked  []Secret
}

// KeyringNames returns the three names a Keyring is read from.
func KeyringNames(name string) []string {
	return []string{name, name + "_PREVIOUS", name + "_REVOKED"}
}

// ParseKeyring builds the Keyring called name from get (a snapshot or the
// env at boot).
func ParseKeyring(get func(name string) string, name string) Keyring {
	names := KeyringNames(name)
	keyring := Keyring{Current: NewSecret(strings.TrimSpace(get(names[0])))}
	for _, value := range splitList(get(names[1])) {
		keyring.Previous = append(keyring.Previous, NewSecret(value))
	}
	for _, value := range splitList(get(names[2])) {
		keyring.Revoked = append(keyring.Revoked, NewSecret(value))
	}
	return keyring
}

// Fingerprint names the whole keyring in logs without revealing it.
func (keyring Keyring) Fingerprint() string {
	fp := keyring.Current.Fingerprint()
	for _, secret := range keyring.Previous {
		fp += "," + secret.Fingerprint()
	}
	for _, secret := range keyring.Revoked {
		fp += ",revoked:" + secret.Fingerprint()
	}
	return fp
}

// Keyring returns the current Keyring called name (see ParseKeyring).
func (manager *Manager) Keyring(name string) (Keyring, error) {
	if err := manager.Healthy(); err != nil {
		return Keyring{}, err
	}
	manager.mu.RLock()
	snap := manager.snap
	manager.mu.RUnlock()
	return ParseKeyring(func(name string) string { secret, _ := snap.Get(name); return secret.value }, name), nil
}

// OnKeyring calls fn now (once a snapshot is published) and whenever any of
// the keyring's three names changes, once per distinct keyring: the names
// usually change together in one save. An error from fn is retried after the
// next fetch, like any Hook.
func (manager *Manager) OnKeyring(name string, fn func(Keyring) error) {
	var last string // fingerprint of the last keyring fn accepted; hooks run one at a time
	hook := func(Secret) error {
		keyring, err := manager.Keyring(name)
		if err != nil {
			return err
		}
		if fp := keyring.Fingerprint(); fp != last {
			if err := fn(keyring); err != nil {
				return err
			}
			last = fp
		}
		return nil
	}
	for _, keyringName := range KeyringNames(name) {
		manager.OnChange(keyringName, hook)
	}
}

func splitList(value string) []string {
	var out []string
	for key := range strings.SplitSeq(value, ",") {
		if key = strings.TrimSpace(key); key != "" {
			out = append(out, key)
		}
	}
	return out
}
