package config

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"testing"

	"github.com/jayleaton/ai-patterns-go/packages/secrets"
)

func env(kv map[string]string) func(string) string {
	return func(k string) string { return kv[k] }
}

func TestLoadDevelopmentDefaults(t *testing.T) {
	cfg, err := Load(env(map[string]string{
		"PUBLIC_URL":   "http://dev-box:3000/",
		"DATABASE_URL": "postgres://x",
		"AUTH_SECRET":  strings.Repeat("s", 32),
	}))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Env != Development || cfg.Addr != "0.0.0.0:3000" {
		t.Fatalf("env=%s addr=%s", cfg.Env, cfg.Addr)
	}
	if cfg.PublicURL.String() != "http://dev-box:3000" || cfg.InternalURL.String() != "http://dev-box:3000" {
		t.Fatalf("public=%s internal=%s", cfg.PublicURL, cfg.InternalURL)
	}
	if cfg.Features.EmailVerification || !cfg.Features.MobileDevOrigins {
		t.Fatalf("dev features = %+v", cfg.Features)
	}
	if got := Absolute(cfg.PublicURL, AppRoutes.VerifyEmail); got != "http://dev-box:3000/auth/verify-email" {
		t.Fatalf("Absolute = %s", got)
	}
}

func TestLoadProductionRequiresProviders(t *testing.T) {
	_, err := Load(env(map[string]string{"APP_ENV": "production", "PUBLIC_URL": "https://app.example.com/path"}))
	if err == nil {
		t.Fatal("want error")
	}
	for _, want := range []string{"PUBLIC_URL", "DATABASE_URL", "AUTH_SECRET", "RESEND_API_KEY"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error does not mention %s: %v", want, err)
		}
	}
}

func TestProductionFeatures(t *testing.T) {
	f := FeaturesFor(Production)
	if !f.EmailVerification || f.MobileDevOrigins {
		t.Fatalf("production features = %+v", f)
	}
}

func TestLoadSecretsProvider(t *testing.T) {
	base := map[string]string{"PUBLIC_URL": "http://dev-box:3000", "DATABASE_URL": "postgres://x", "AUTH_SECRET": strings.Repeat("s", 32)}
	with := func(kv map[string]string) map[string]string {
		m := map[string]string{}
		for k, v := range base {
			m[k] = v
		}
		for k, v := range kv {
			m[k] = v
		}
		return m
	}

	// Default env provider: RESEND_API_KEY from the env, read once.
	cfg, err := Load(env(with(map[string]string{"RESEND_API_KEY": "re_env"})))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Secrets.Provider() != SecretsEnv {
		t.Fatalf("provider = %s", cfg.Secrets.Provider())
	}
	if err := cfg.Secrets.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	defer cfg.Secrets.Stop()
	var got secrets.Secret
	cfg.Secrets.OnResendAPIKey(func(v secrets.Secret) error { got = v; return nil })
	if !got.Equal(secrets.NewSecret("re_env")) || cfg.Secrets.Healthy() != nil || cfg.Secrets.Status().Keys != 1 {
		t.Fatalf("resend key = %s, status = %+v", got.Fingerprint(), cfg.Secrets.Status())
	}

	cfg, err = Load(env(with(map[string]string{"SECRETS_PROVIDER": "doppler", "DOPPLER_TOKEN": "dp.st.x", "SECRETS_REFRESH_INTERVAL": "30s"})))
	if err != nil || cfg.Secrets.Provider() != SecretsDoppler {
		t.Fatalf("doppler: %v", err)
	}

	for _, tt := range []struct {
		kv   map[string]string
		want string
	}{
		{map[string]string{"SECRETS_PROVIDER": "vault"}, "SECRETS_PROVIDER must be env or doppler"},
		{map[string]string{"SECRETS_PROVIDER": "doppler"}, "DOPPLER_TOKEN is required"},
		{map[string]string{"SECRETS_PROVIDER": "doppler", "DOPPLER_TOKEN": "t", "DOPPLER_API_URL": "ftp://user:hunter2@x"}, "DOPPLER_API_URL must be an http(s) origin"},
		{map[string]string{"APP_ENV": "production", "PUBLIC_URL": "https://a.example", "SECRETS_PROVIDER": "doppler", "DOPPLER_TOKEN": "t", "DOPPLER_API_URL": "http://fake:8080"}, "must use https in production"},
		{map[string]string{"SECRETS_REFRESH_INTERVAL": "soon"}, "SECRETS_REFRESH_INTERVAL must be a positive duration"},
		{map[string]string{"SECRETS_REFRESH_INTERVAL": "1s"}, "SECRETS_REFRESH_INTERVAL must be at least 10s"},
		{map[string]string{"SECRETS_MAX_STALENESS": "90s"}, "SECRETS_MAX_STALENESS must be at least twice"},
	} {
		_, err := Load(env(with(tt.kv)))
		if err == nil || !strings.Contains(err.Error(), tt.want) {
			t.Errorf("%v: err = %v, want %q", tt.kv, err, tt.want)
		}
		if err != nil && strings.Contains(err.Error(), "hunter2") {
			t.Errorf("error echoes credentials: %v", err)
		}
	}
}

// A loaded Config prints, logs and marshals without its credentials, so
// logging it (or a struct holding it) can never leak them.
func TestConfigNeverPrintsSecrets(t *testing.T) {
	values := []string{"dbpass-4242", "auth-secret-" + strings.Repeat("z", 32), "re_resend_9999", "dp.st.doppler_7777"}
	cfg, err := Load(env(map[string]string{
		"PUBLIC_URL":       "http://dev-box:3000",
		"DATABASE_URL":     "postgres://app:" + values[0] + "@db:5432/app",
		"AUTH_SECRET":      values[1],
		"RESEND_API_KEY":   values[2],
		"SECRETS_PROVIDER": "doppler",
		"DOPPLER_TOKEN":    values[3],
	}))
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.DatabaseURL.Equal(secrets.NewSecret("postgres://app:"+values[0]+"@db:5432/app")) || !cfg.AuthSecret.Equal(secrets.NewSecret(values[1])) {
		t.Fatal("credentials not loaded")
	}
	var logged bytes.Buffer
	slog.New(slog.NewTextHandler(&logged, nil)).Info("cfg", "cfg", cfg)
	slog.New(slog.NewJSONHandler(&logged, nil)).Info("cfg", "cfg", cfg)
	j, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	for _, out := range []string{fmt.Sprintf("%v %+v %#v", cfg, cfg, cfg), logged.String(), string(j)} {
		for _, v := range values {
			if strings.Contains(out, v) {
				t.Errorf("config output contains %q: %s", v, out)
			}
		}
	}
}
