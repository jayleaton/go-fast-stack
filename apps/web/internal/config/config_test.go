package config

import (
	"strings"
	"testing"
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
	if cfg.Features.EmailVerification || !cfg.Features.DevAuth {
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
	if !f.EmailVerification || f.DevAuth {
		t.Fatalf("production features = %+v", f)
	}
}
