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
	n := KeyringNames(name)
	k := Keyring{Current: NewSecret(strings.TrimSpace(get(n[0])))}
	for _, v := range splitList(get(n[1])) {
		k.Previous = append(k.Previous, NewSecret(v))
	}
	for _, v := range splitList(get(n[2])) {
		k.Revoked = append(k.Revoked, NewSecret(v))
	}
	return k
}

// Fingerprint names the whole keyring in logs without revealing it.
func (k Keyring) Fingerprint() string {
	fp := k.Current.Fingerprint()
	for _, p := range k.Previous {
		fp += "," + p.Fingerprint()
	}
	for _, r := range k.Revoked {
		fp += ",revoked:" + r.Fingerprint()
	}
	return fp
}

// Keyring returns the current Keyring called name (see ParseKeyring).
func (m *Manager) Keyring(name string) (Keyring, error) {
	if err := m.Healthy(); err != nil {
		return Keyring{}, err
	}
	m.mu.RLock()
	snap := m.snap
	m.mu.RUnlock()
	return ParseKeyring(func(n string) string { v, _ := snap.Get(n); return v.v }, name), nil
}

// OnKeyring calls fn now (once a snapshot is published) and whenever any of
// the keyring's three names changes, once per distinct keyring: the names
// usually change together in one save. An error from fn is retried after the
// next fetch, like any Hook.
func (m *Manager) OnKeyring(name string, fn func(Keyring) error) {
	var last string // fingerprint of the last keyring fn accepted; hooks run one at a time
	hook := func(Secret) error {
		k, err := m.Keyring(name)
		if err != nil {
			return err
		}
		if fp := k.Fingerprint(); fp != last {
			if err := fn(k); err != nil {
				return err
			}
			last = fp
		}
		return nil
	}
	for _, n := range KeyringNames(name) {
		m.OnChange(n, hook)
	}
}

func splitList(v string) []string {
	var out []string
	for p := range strings.SplitSeq(v, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}
