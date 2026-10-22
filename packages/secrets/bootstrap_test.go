package secrets_test

import (
	"errors"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/jayleaton/ai-patterns-go/packages/secrets"
	"github.com/jayleaton/ai-patterns-go/packages/secrets/reveal"
)

func getter(kv map[string]string) func(string) string {
	return func(k string) string { return kv[k] }
}

func TestBootstrapReportsEveryProblem(t *testing.T) {
	t.Parallel()

	requireKey := func(get func(string) string) []error {
		if get(key) == "" {
			return []error{errors.New(key + " is required")}
		}
		return nil
	}
	for _, tt := range []struct {
		kv         map[string]string
		production bool
		want       []string
	}{
		{map[string]string{}, false, []string{key + " is required"}},
		{map[string]string{"SECRETS_PROVIDER": "vault"}, false, []string{`SECRETS_PROVIDER must be env or doppler, got "vault"`}},
		{map[string]string{"SECRETS_PROVIDER": "doppler", "DOPPLER_API_URL": "ftp://user:hunter2@x"}, false, []string{"DOPPLER_TOKEN is required", "DOPPLER_API_URL must be an http(s) origin"}},
		{map[string]string{"SECRETS_PROVIDER": "doppler", "DOPPLER_TOKEN": "t", "DOPPLER_API_URL": "http://fake:1"}, true, []string{"must use https in production"}},
		{map[string]string{key: "x", "SECRETS_REFRESH_INTERVAL": "soon", "SECRETS_MAX_STALENESS": "-1m"}, false, []string{"SECRETS_REFRESH_INTERVAL must be a positive duration", "SECRETS_MAX_STALENESS must be a positive duration"}},
		{map[string]string{key: "x", "SECRETS_REFRESH_INTERVAL": "1s"}, false, []string{"at least 10s"}},
		{map[string]string{key: "x", "SECRETS_MAX_STALENESS": "90s"}, false, []string{"at least twice"}},
	} {
		_, errs := secrets.Bootstrap(getter(tt.kv), secrets.BootstrapOptions{Keys: []string{key}, Validate: requireKey, Production: tt.production})
		joined := errors.Join(errs...)
		for _, w := range tt.want {
			if joined == nil || !strings.Contains(joined.Error(), w) {
				t.Errorf("%v: %v, want %q", tt.kv, joined, w)
			}
		}
		if joined != nil && strings.Contains(joined.Error(), "hunter2") {
			t.Errorf("error echoes credentials: %v", joined)
		}
	}

	m, errs := secrets.Bootstrap(getter(map[string]string{"SECRETS_PROVIDER": "doppler", "DOPPLER_TOKEN": "t"}), secrets.BootstrapOptions{Keys: []string{key}})
	if len(errs) != 0 || m.Provider() != "doppler" {
		t.Fatalf("doppler: %v %s", errs, m.Provider())
	}
	if d, err := secrets.ParseDuration("X", "", time.Minute); err != nil || d != time.Minute {
		t.Fatalf("ParseDuration default: %s %v", d, err)
	}
}

func TestBootstrapValidatesTheSnapshot(t *testing.T) {
	t.Parallel()

	validate := func(get func(string) string) []error {
		if !strings.HasPrefix(get(key), "re_") {
			return []error{errors.New(key + " must start with re_")}
		}
		return nil
	}
	// The same validator runs on the boot values (reported by Bootstrap) and
	// on every snapshot before it is published (Start, each fetch).
	m, errs := secrets.Bootstrap(getter(map[string]string{key: "re_ok"}), secrets.BootstrapOptions{Keys: []string{key}, Validate: validate})
	if len(errs) != 0 {
		t.Fatal(errs)
	}
	if err := m.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	m.Stop()
	bad, errs := secrets.Bootstrap(getter(map[string]string{key: "nope"}), secrets.BootstrapOptions{Keys: []string{key}, Validate: validate})
	if len(errs) != 1 {
		t.Fatalf("boot errors = %v", errs)
	}
	if err := bad.Start(t.Context()); !errors.Is(err, secrets.ErrInvalid) || !strings.Contains(err.Error(), "must start with re_") {
		t.Fatalf("Start = %v", err)
	}
}

func TestKeyring(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {
		a, b, c := strings.Repeat("a", 32), strings.Repeat("b", 32), strings.Repeat("c", 32)
		f := newFake(map[string]string{"SIGNING": " " + a + "\n"})
		log, logs := newLogger()
		m := start(t, dopplerFor(f), secrets.Options{Keys: secrets.KeyringNames("SIGNING"), Logger: log})

		var seen []string
		m.OnKeyring("SIGNING", func(k secrets.Keyring) error {
			cur, prev, rev := reveal.Keyring(k)
			seen = append(seen, cur+"|"+strings.Join(prev, ",")+"|"+strings.Join(rev, ","))
			return nil
		})
		// All three names change in one save: one call with the new keyring.
		f.set(map[string]string{"SIGNING": b, "SIGNING_PREVIOUS": a + " , ", "SIGNING_REVOKED": c})
		if err := m.Refresh(t.Context()); err != nil {
			t.Fatal(err)
		}
		want := []string{a + "||", b + "|" + a + "|" + c}
		if strings.Join(seen, ";") != strings.Join(want, ";") {
			t.Fatalf("keyrings seen %q, want %q", seen, want)
		}
		k, err := m.Keyring("SIGNING")
		if err != nil {
			t.Fatal(err)
		}
		if fp := k.Fingerprint(); strings.Count(fp, "fp:") != 3 || !strings.Contains(fp, "revoked:fp:") || strings.Contains(fp, a) {
			t.Fatalf("fingerprint %s", fp)
		}
		if strings.Contains(logs.String(), a) || strings.Contains(logs.String(), b) {
			t.Fatal("keyring values logged")
		}
		m.Stop()
		advance(time.Hour)
		if _, err := m.Keyring("SIGNING"); !errors.Is(err, secrets.ErrStale) {
			t.Fatalf("stale keyring: %v", err)
		}
	})
}
