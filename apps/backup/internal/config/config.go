// Package config loads the backup job's environment, validated once at
// start. Nothing else in apps/backup reads os.Getenv. Credentials are
// secrets.Secret from SECRETS_PROVIDER (env or Doppler): every run reads the
// current values, so rotating them needs no redeploy.
package config

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/jayleaton/go-fast-stack/packages/backup"
	"github.com/jayleaton/go-fast-stack/packages/secrets"
)

// Secrets served by SECRETS_PROVIDER.
const (
	secretDatabaseURL      = "DATABASE_URL"       // the app database (backed up as "app")
	secretAdminDatabaseURL = "ADMIN_DATABASE_URL" // optional: the admin database ("admin")
	secretAccessKeyID      = "BACKUP_S3_ACCESS_KEY_ID"
	secretSecretAccessKey  = "BACKUP_S3_SECRET_ACCESS_KEY"
	secretEncryption       = "BACKUP_ENCRYPTION_KEY" // a keyring: _PREVIOUS still decrypts older backups
)

var liveSecrets = append([]string{secretDatabaseURL, secretAdminDatabaseURL, secretAccessKeyID, secretSecretAccessKey}, secrets.KeyringNames(secretEncryption)...)

// Store is where backups go: Dir when set (local), else the S3 bucket.
type Store struct {
	Dir       string // BACKUP_DIR
	Endpoint  string // BACKUP_S3_ENDPOINT
	Bucket    string // BACKUP_S3_BUCKET
	Region    string // BACKUP_S3_REGION (default auto)
	PathStyle bool   // BACKUP_S3_PATH_STYLE
}

// Config is the validated environment.
type Config struct {
	Production bool // APP_ENV=production: logs redact dev mail links too
	Store      Store
	Retention  backup.Retention // BACKUP_KEEP_DAILY (7), BACKUP_KEEP_WEEKLY (4)
	Schedule   backup.Schedule  // BACKUP_SCHEDULE: when `backup serve` runs (cron, UTC; default 0 3 * * *)
	Secrets    Secrets
	// RestoreURL is RESTORE_DATABASE_URL, read from the env only (never from
	// Doppler, never defaulted): a restore target is a deliberate choice.
	RestoreURL secrets.Secret
}

// Upper bounds on BACKUP_KEEP_DAILY / BACKUP_KEEP_WEEKLY.
const (
	maxKeepDaily  = 366 // a year of dailies
	maxKeepWeekly = 520 // ten years of weeklies
)

// Load reads and validates config; every problem is reported at once.
func Load(getenv func(string) string) (Config, error) {
	get := func(k string) string { return strings.TrimSpace(getenv(k)) }
	var errs []error
	cfg := Config{
		Production: get("APP_ENV") == "production",
		Store: Store{
			Dir: get("BACKUP_DIR"), Endpoint: get("BACKUP_S3_ENDPOINT"), Bucket: get("BACKUP_S3_BUCKET"),
			Region: or(get("BACKUP_S3_REGION"), "auto"),
		},
		Retention:  backup.DefaultRetention,
		RestoreURL: secrets.NewSecret(get("RESTORE_DATABASE_URL")),
	}
	var err error
	if cfg.Schedule, err = backup.ParseSchedule(or(get("BACKUP_SCHEDULE"), "0 3 * * *")); err != nil {
		errs = append(errs, fmt.Errorf("BACKUP_SCHEDULE: %w", err))
	}
	if cfg.Store.PathStyle, err = parseBool("BACKUP_S3_PATH_STYLE", get("BACKUP_S3_PATH_STYLE")); err != nil {
		errs = append(errs, err)
	}
	if cfg.Store.Dir == "" && (cfg.Store.Endpoint == "" || cfg.Store.Bucket == "") {
		errs = append(errs, errors.New("set BACKUP_S3_ENDPOINT and BACKUP_S3_BUCKET (an S3-compatible bucket), or BACKUP_DIR (a local directory)"))
	}
	if cfg.Retention.Daily, err = parseCount("BACKUP_KEEP_DAILY", get("BACKUP_KEEP_DAILY"), cfg.Retention.Daily, maxKeepDaily); err != nil {
		errs = append(errs, err)
	} else if cfg.Retention.Daily < 1 {
		errs = append(errs, errors.New("BACKUP_KEEP_DAILY must be at least 1"))
	}
	if cfg.Retention.Weekly, err = parseCount("BACKUP_KEEP_WEEKLY", get("BACKUP_KEEP_WEEKLY"), cfg.Retention.Weekly, maxKeepWeekly); err != nil {
		errs = append(errs, err)
	}
	opts := secrets.BootstrapOptions{Keys: liveSecrets, Validate: validateLive(cfg.Store.Dir != ""), Production: cfg.Production}
	m, problems := secrets.Bootstrap(get, opts)
	errs = append(errs, problems...)
	// A manager starts once; every run builds its own (same, validated
	// bootstrap), so each run reads the provider's current values.
	cfg.Secrets = Secrets{provider: m.Provider(), fresh: func() *secrets.Manager { m, _ := secrets.Bootstrap(get, opts); return m }}
	if len(errs) > 0 {
		return Config{}, fmt.Errorf("invalid backup config: %w", errors.Join(errs...))
	}
	return cfg, nil
}

func validateLive(localDir bool) func(get func(string) string) []error {
	return func(get func(string) string) []error {
		var errs []error
		if get(secretDatabaseURL) == "" {
			errs = append(errs, errors.New("DATABASE_URL (the app database) is required"))
		}
		k := secrets.ParseKeyring(get, secretEncryption)
		if k.Current.Len() < backup.MinKeyLength {
			errs = append(errs, errors.New("BACKUP_ENCRYPTION_KEY must be at least 32 characters (openssl rand -base64 32); keep a copy outside Railway"))
		}
		for i, v := range k.Previous {
			if v.Len() < backup.MinKeyLength {
				errs = append(errs, fmt.Errorf("BACKUP_ENCRYPTION_KEY_PREVIOUS entry %d must be at least 32 characters", i+1))
			}
		}
		if !localDir && (get(secretAccessKeyID) == "" || get(secretSecretAccessKey) == "") {
			errs = append(errs, errors.New("BACKUP_S3_ACCESS_KEY_ID and BACKUP_S3_SECRET_ACCESS_KEY are required for the S3 bucket"))
		}
		return errs
	}
}

// Secrets hands the job its credentials; cmd/backup reveals them where it
// builds the clients.
type Secrets struct {
	provider string
	fresh    func() *secrets.Manager
}

// Load fetches the current values (a run is short: no polling). Call it
// once per run: values rotated upstream apply to the next run.
func (s Secrets) Load(ctx context.Context) (Values, error) {
	m := s.fresh()
	if err := m.Start(ctx); err != nil {
		return Values{}, err
	}
	defer m.Stop()
	get := func(name string) secrets.Secret { v, _ := m.Get(name); return v.TrimSpace() }
	k, err := m.Keyring(secretEncryption)
	if err != nil {
		return Values{}, err
	}
	return Values{
		DatabaseURL: get(secretDatabaseURL), AdminDatabaseURL: get(secretAdminDatabaseURL),
		AccessKeyID: get(secretAccessKeyID), SecretAccessKey: get(secretSecretAccessKey),
		Encryption: k,
	}, nil
}

// Provider is SECRETS_PROVIDER.
func (s Secrets) Provider() string { return s.provider }

// Values are one run's credentials.
type Values struct {
	DatabaseURL      secrets.Secret
	AdminDatabaseURL secrets.Secret // zero: no admin database
	AccessKeyID      secrets.Secret
	SecretAccessKey  secrets.Secret
	Encryption       secrets.Keyring
}

// parseCount reads a retention count; the limits match deploy/platform.yaml's.
func parseCount(key, raw string, fallback, maxN int) (int, error) {
	if raw == "" {
		return fallback, nil
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 0 || n > maxN {
		return fallback, fmt.Errorf("%s must be a number from 0 to %d, got %q", key, maxN, raw)
	}
	return n, nil
}

func parseBool(key, raw string) (bool, error) {
	if raw == "" {
		return false, nil
	}
	b, err := strconv.ParseBool(raw)
	if err != nil {
		return false, fmt.Errorf("%s must be true or false, got %q", key, raw)
	}
	return b, nil
}

func or(v, fallback string) string {
	if v == "" {
		return fallback
	}
	return v
}
