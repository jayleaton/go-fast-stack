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

func valid() map[string]string {
	return map[string]string{
		"ADMIN_PUBLIC_URL":   "http://dev-box:3001/",
		"DATABASE_URL":       "postgres://x/app",
		"ADMIN_DATABASE_URL": "postgres://x/admin",
		"ADMIN_AUTH_SECRET":  strings.Repeat("a", 32),
	}
}

func TestLoadDevelopmentDefaults(t *testing.T) {
	kv := valid()
	kv["PORT"] = "3000" // the web app's; ADMIN_PORT unset falls back to it
	kv["ADMIN_BOOTSTRAP_EMAIL"] = " Root@Example.com "
	cfg, err := Load(env(kv))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Env != Development || cfg.Addr != "0.0.0.0:3000" || cfg.RequireTwoFactor {
		t.Fatalf("env=%s addr=%s 2fa=%v", cfg.Env, cfg.Addr, cfg.RequireTwoFactor)
	}
	if cfg.PublicURL.String() != "http://dev-box:3001" || cfg.TrustedOrigins[0] != "http://dev-box:3001" {
		t.Fatalf("public=%s trusted=%v", cfg.PublicURL, cfg.TrustedOrigins)
	}
	if cfg.BootstrapEmail != "root@example.com" || cfg.AppName != "Admin" {
		t.Fatalf("bootstrap=%q app=%q", cfg.BootstrapEmail, cfg.AppName)
	}
	kv["ADMIN_PORT"] = "3100"
	if cfg, _ = Load(env(kv)); cfg.Addr != "0.0.0.0:3100" {
		t.Fatalf("ADMIN_PORT ignored: %s", cfg.Addr)
	}
	if got := Absolute(cfg.PublicURL, UserPage("u 1")); got != "http://dev-box:3001/users/u%201" {
		t.Fatalf("Absolute = %s", got)
	}
}

func TestLoadProductionDefaultsTo2FA(t *testing.T) {
	kv := valid()
	kv["APP_ENV"], kv["RESEND_API_KEY"] = "production", "re_x"
	cfg, err := Load(env(kv))
	if err != nil || !cfg.RequireTwoFactor {
		t.Fatalf("production: 2fa=%v err=%v", cfg.RequireTwoFactor, err)
	}
	kv["ADMIN_REQUIRE_2FA"] = "false"
	if cfg, err = Load(env(kv)); err != nil || cfg.RequireTwoFactor {
		t.Fatalf("explicit false: 2fa=%v err=%v", cfg.RequireTwoFactor, err)
	}
}

func TestLoadReportsEveryProblem(t *testing.T) {
	_, err := Load(env(map[string]string{
		"APP_ENV": "production", "ADMIN_PUBLIC_URL": "https://admin.example.com/path",
		"ADMIN_REQUIRE_2FA": "maybe", "ADMIN_BOOTSTRAP_EMAIL": "root",
	}))
	if err == nil {
		t.Fatal("want error")
	}
	for _, want := range []string{"ADMIN_PUBLIC_URL", "DATABASE_URL", "ADMIN_DATABASE_URL", "ADMIN_AUTH_SECRET", "ADMIN_REQUIRE_2FA", "ADMIN_BOOTSTRAP_EMAIL", "RESEND_API_KEY"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error does not mention %s: %v", want, err)
		}
	}
}

func TestLoadRefusesSharedSecretsAndDatabases(t *testing.T) {
	kv := valid()
	kv["AUTH_SECRET"] = kv["ADMIN_AUTH_SECRET"]
	kv["ADMIN_DATABASE_URL"] = kv["DATABASE_URL"]
	_, err := Load(env(kv))
	if err == nil || !strings.Contains(err.Error(), "differ from the app's AUTH_SECRET") || !strings.Contains(err.Error(), "different database") {
		t.Fatalf("err = %v", err)
	}
}

func TestLoadLiveSecrets(t *testing.T) {
	kv := valid()
	kv["ADMIN_AUTH_SECRET_PREVIOUS"] = strings.Repeat("p", 32)
	kv["ADMIN_AUTH_SECRET_GRACE"] = "24h"
	cfg, err := Load(env(kv))
	if err != nil {
		t.Fatal(err)
	}
	if err := cfg.Secrets.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	defer cfg.Secrets.Stop()
	app, _ := cfg.Secrets.AppDatabaseURL()
	admin, _ := cfg.Secrets.AdminDatabaseURL()
	keys, err := cfg.Secrets.AuthSecrets()
	if err != nil || !app.Equal(secrets.NewSecret("postgres://x/app")) || !admin.Equal(secrets.NewSecret("postgres://x/admin")) || len(keys.Previous) != 1 || cfg.AuthSecretGrace.Hours() != 24 {
		t.Fatalf("app=%v admin=%v keys=%v grace=%s err=%v", app.IsZero(), admin.IsZero(), len(keys.Previous), cfg.AuthSecretGrace, err)
	}
	if cfg.Secrets.Provider() != "env" || cfg.Secrets.Healthy() != nil {
		t.Fatal("env provider not healthy")
	}
	kv["ADMIN_AUTH_SECRET_REVOKED"] = "short"
	if _, err := Load(env(kv)); err == nil || !strings.Contains(err.Error(), "ADMIN_AUTH_SECRET_REVOKED entry 1") {
		t.Fatalf("short revoked secret: %v", err)
	}
}

// A loaded Config prints, logs and marshals without its credentials, so
// logging it (or a struct holding it) can never leak them.
func TestConfigNeverPrintsSecrets(t *testing.T) {
	values := []string{"apppass-4242", "adminpass-5353", "admin-secret-" + strings.Repeat("z", 32), "re_resend_9999"}
	kv := valid()
	kv["DATABASE_URL"] = "postgres://app:" + values[0] + "@db/app"
	kv["ADMIN_DATABASE_URL"] = "postgres://admin:" + values[1] + "@db/admin"
	kv["ADMIN_AUTH_SECRET"] = values[2]
	kv["RESEND_API_KEY"] = values[3]
	cfg, err := Load(env(kv))
	if err != nil {
		t.Fatal(err)
	}
	if err := cfg.Secrets.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	defer cfg.Secrets.Stop()
	if keys, _ := cfg.Secrets.AuthSecrets(); !keys.Current.Equal(secrets.NewSecret(values[2])) {
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
