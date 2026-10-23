package platform

import (
	"fmt"
	"slices"
	"strings"

	railway "github.com/railwayapp/railway-go-sdk"
)

// Railway service names. References between services use them
// (${{postgres.DATABASE_URL}}), so they are part of the contract: renaming
// one is a destructive change to every environment.
const (
	ServiceWeb           = "web"
	ServicePostgres      = "postgres"
	ServiceAdmin         = "admin"
	ServiceAdminPostgres = "admin-postgres"
	ServiceBackup        = "backup"  // the nightly backup cron (backups: in platform.yaml)
	BucketBackups        = "backups" // its Railway storage bucket
)

// AppPort is the port both apps listen on; public domains target it.
const AppPort = 3000

// Railway deploy settings shared by every service.
const (
	maxRestarts        = 5       // restartPolicyMaxRetries: restarts after a crash before Railway gives up
	healthcheckSeconds = 60      // how long a new deployment has to answer /healthz
	bytesPerMiB        = 1 << 20 // memoryBytes is in bytes; sizes are in MiB
)

// SecretKind says how setup provisions a bootstrap variable.
type SecretKind int

const (
	// Generated: 48 random bytes, created once per service and environment,
	// never copied between environments. Not sealed: rotating an auth secret
	// needs its old value (AUTH_SECRET_PREVIOUS / _REVOKED, see docs/secrets.md),
	// and a sealed value can never be read back.
	Generated SecretKind = iota
	// Provider: SECRETS_PROVIDER, set by setup from whether this service and
	// environment has a real DOPPLER_TOKEN yet (doppler) or not (env).
	Provider
	// DopplerToken: a read-only Doppler service token for this service's
	// config. Setup leaves a placeholder; `make railway-secrets` seals the
	// real one.
	DopplerToken
	// Operator: supplied by the person running setup (RESEND_API_KEY from
	// their shell); never generated. Sealed.
	Operator
	// Manual: set by hand during a rotation (AUTH_SECRET_PREVIOUS, ...);
	// setup never writes it, IaC must never delete it.
	Manual
)

// Secret is a bootstrap variable on one service. These are kept out of the
// IaC graph (project variables.ignore), so IaC never prints, diffs or deletes
// them; setup owns them.
type Secret struct {
	Service string
	Name    string
	Kind    SecretKind
}

// DopplerPlaceholder marks a DOPPLER_TOKEN nobody has set yet.
const DopplerPlaceholder = "REPLACE_WITH_DOPPLER_SERVICE_TOKEN"

// ResendPlaceholder lets production boot before anyone has a Resend key:
// the app requires RESEND_API_KEY there, and sending with this one fails.
const ResendPlaceholder = "re_REPLACE_WITH_RESEND_API_KEY"

// SecretVars lists every bootstrap variable for the configured services.
func (c Config) SecretVars() []Secret {
	out := []Secret{
		{ServiceWeb, "AUTH_SECRET", Generated},
		{ServiceWeb, "AUTH_SECRET_PREVIOUS", Manual},
		{ServiceWeb, "AUTH_SECRET_REVOKED", Manual},
		{ServiceWeb, "SECRETS_PROVIDER", Provider},
		{ServiceWeb, "RESEND_API_KEY", Operator},
	}
	// DOPPLER_TOKEN is listed (ignored by IaC) even with provider env, so
	// switching provider never plans deleting a real token.
	out = append(out, Secret{ServiceWeb, "DOPPLER_TOKEN", DopplerToken})
	if c.Backups.Enabled {
		// Encrypts every backup (age). Keep a copy outside Railway: without
		// it a backup cannot be restored. _PREVIOUS decrypts older backups
		// after a rotation.
		out = append(out,
			Secret{ServiceBackup, "BACKUP_ENCRYPTION_KEY", Generated},
			Secret{ServiceBackup, "BACKUP_ENCRYPTION_KEY_PREVIOUS", Manual},
		)
		if c.Backups.Storage == StorageExternal {
			for _, v := range BackupS3Vars {
				out = append(out, Secret{ServiceBackup, v, Manual})
			}
		}
	}
	if c.Admin {
		out = append(out,
			Secret{ServiceAdmin, "ADMIN_AUTH_SECRET", Generated},
			Secret{ServiceAdmin, "ADMIN_AUTH_SECRET_PREVIOUS", Manual},
			Secret{ServiceAdmin, "ADMIN_AUTH_SECRET_REVOKED", Manual},
			Secret{ServiceAdmin, "SECRETS_PROVIDER", Provider},
			Secret{ServiceAdmin, "RESEND_API_KEY", Operator},
			Secret{ServiceAdmin, "DOPPLER_TOKEN", DopplerToken},
		)
	}
	return out
}

// HasService reports whether the graph has a service with this name.
func (c Config) HasService(name string) bool {
	services := append(c.PublicServices(), ServicePostgres)
	if c.Admin {
		services = append(services, ServiceAdminPostgres)
	}
	if c.Backups.Enabled {
		services = append(services, ServiceBackup)
	}
	return slices.Contains(services, name)
}

// PublicServices are the services that get a Railway-generated domain.
func (c Config) PublicServices() []string {
	if c.Admin {
		return []string{ServiceWeb, ServiceAdmin}
	}
	return []string{ServiceWeb}
}

// Project is what deploy/.railway/railway.go returns to the Railway CLI. The
// CLI calls Graph(). It wraps railway.Project because the Go SDK cannot set
// the project-level variables policy (see docs/railway-go-iac-issues.md).
type Project struct {
	railway.Project
	Ignore []string
}

// Graph is the IaC graph the CLI plans and applies.
func (p Project) Graph() map[string]any {
	g := p.Project.Graph()
	g["variables"] = map[string]any{"managed": true, "ignore": p.Ignore}
	return g
}

// Build builds the desired Railway graph for env: the web service and its
// Postgres, plus the admin service and its own Postgres when admin is on.
func (c Config) Build(env string) (Project, error) {
	if !c.Has(env) {
		return Project{}, fmt.Errorf("environment %q is not in deploy/platform.yaml environments %v", env, []string(c.Environments))
	}
	if c.Project == Auto || c.Repo == Auto {
		return Project{}, fmt.Errorf("project/repo: auto is unresolved: load platform.yaml with LoadResolved")
	}
	settings := c.Settings(env)
	db := c.postgres(ServicePostgres)
	webEnv := map[string]any{"DATABASE_URL": db.Env("DATABASE_URL")}
	setURL(webEnv, "PUBLIC_URL", settings.PublicURL)
	web := railway.ServiceNamed(ServiceWeb, c.service(env, app{
		dockerfile: "Dockerfile",
		migrate:    "/server migrate",
		watch:      []any{"apps/web/**", "packages/**", "go.mod", "go.sum", "Dockerfile"},
		size:       c.Resources.Web,
		env:        webEnv,
	}))
	resources := []any{db, web}
	var adminBackupURL map[string]any
	if c.Admin {
		adminDB := c.postgres(ServiceAdminPostgres)
		vars := map[string]any{
			"DATABASE_URL":       db.Env("DATABASE_URL"),
			"ADMIN_DATABASE_URL": adminDB.Env("DATABASE_URL"),
		}
		setURL(vars, "ADMIN_PUBLIC_URL", settings.AdminPublicURL)
		if c.AdminBootstrapEmail != "" {
			vars["ADMIN_BOOTSTRAP_EMAIL"] = c.AdminBootstrapEmail
		}
		admin := railway.ServiceNamed(ServiceAdmin, c.service(env, app{
			dockerfile: "apps/admin/Dockerfile",
			migrate:    "/admin migrate",
			watch:      []any{"apps/admin/**", "packages/**", "go.mod", "go.sum"},
			size:       c.Resources.Admin,
			env:        vars,
		}))
		resources = append(resources, adminDB, admin)
		if c.Backups.Enabled && c.Backups.Admin {
			adminBackupURL = adminDB.Env("DATABASE_URL")
		}
	}
	if c.Backups.Enabled {
		resources = append(resources, c.backups(env, db.Env("DATABASE_URL"), adminBackupURL)...)
	}

	var ignore []string
	for _, s := range c.SecretVars() {
		ignore = append(ignore, s.Service+"/"+s.Name)
	}
	// A pattern scoped to a service this environment does not have is an
	// error to Railway, not a no-op (docs/railway-go-iac-issues.md #18):
	// keep only the ones whose service is in the graph.
	for _, pattern := range c.Unmanaged {
		if service, _, scoped := strings.Cut(pattern, "/"); !scoped || c.HasService(service) {
			ignore = append(ignore, pattern)
		}
	}
	// Without admin_bootstrap_email, a value set by hand is left alone.
	if c.Admin && c.AdminBootstrapEmail == "" {
		ignore = append(ignore, ServiceAdmin+"/ADMIN_BOOTSTRAP_EMAIL")
	}
	// Values platform.yaml marks manual stay whatever the environment holds.
	ignore = append(ignore, c.HandSet(env)...)
	return Project{Project: railway.ProjectNamed(c.Project, resources), Ignore: ignore}, nil
}

// backups is the backup service and its storage bucket: it sleeps until
// backups.schedule (UTC), dumps the app database (and the admin one),
// encrypts the dumps, stores them in the bucket and prunes. Not a Railway
// cron: it stays up (a few MB) so people can `railway ssh` in to list,
// back up or restore over the private network. No domain, no healthcheck,
// no pre-deploy.
func (c Config) backups(env string, appURL, adminURL map[string]any) []any {
	vars := map[string]any{
		"APP_ENV":            "production",
		"DATABASE_URL":       appURL,
		"BACKUP_SCHEDULE":    c.Backups.Schedule,
		"BACKUP_KEEP_DAILY":  fmt.Sprint(c.Backups.KeepDaily),
		"BACKUP_KEEP_WEEKLY": fmt.Sprint(c.Backups.KeepWeekly),
	}
	var out []any
	if c.Backups.Storage == StorageRailway {
		bucket := railway.Bucket(BucketBackups, map[string]any{"region": c.BucketRegion()})
		for name, output := range map[string]string{
			"BACKUP_S3_ENDPOINT": "ENDPOINT", "BACKUP_S3_BUCKET": "BUCKET", "BACKUP_S3_REGION": "REGION",
			"BACKUP_S3_ACCESS_KEY_ID": "ACCESS_KEY_ID", "BACKUP_S3_SECRET_ACCESS_KEY": "SECRET_ACCESS_KEY",
		} {
			vars[name] = bucket.Env(output)
		}
		out = append(out, bucket)
	}
	if adminURL != nil {
		vars["ADMIN_DATABASE_URL"] = adminURL
	}
	const backupMemoryMB = 512 // pg_dump of a large table
	sc := railway.ServiceConfig{
		"build": map[string]any{
			"builder":        "DOCKERFILE",
			"dockerfilePath": "apps/backup/Dockerfile",
			"watchPatterns":  []any{"apps/backup/**", "packages/backup/**", "packages/secrets/**", "go.mod", "go.sum"},
		},
		"deploy": map[string]any{
			"restartPolicyMaxRetries": maxRestarts,
			"limitOverride": map[string]any{"containers": map[string]any{
				"cpu":         1,
				"memoryBytes": int64(backupMemoryMB) * bytesPerMiB,
			}},
		},
		"env": vars,
	}
	if c.Region != "" {
		sc["replicas"] = map[string]any{c.Region: 1}
	}
	if !c.Upload() {
		sc["source"] = railway.Github(c.Repo, map[string]any{"branch": c.BranchFor(env)})
	} else {
		delete(sc["build"].(map[string]any), "watchPatterns")
	}
	return append(out, railway.ServiceNamed(ServiceBackup, sc))
}

// setURL sets name to the origin value names, unless it is manual (left
// out of the graph and ignored).
func setURL(vars map[string]any, name, value string) {
	switch value {
	case ManualValue:
	case RailwayDomain:
		vars[name] = "https://${{RAILWAY_PUBLIC_DOMAIN}}"
	default:
		vars[name] = value
	}
}

type app struct {
	dockerfile string
	migrate    string
	watch      []any
	size       Size
	env        map[string]any
}

func (c Config) postgres(name string) railway.Resource {
	if c.Region == "" {
		return railway.Postgres(name)
	}
	return railway.Postgres(name, map[string]any{"region": c.Region})
}

// service is the shared shape of a Go app service: built from the repo root
// with its Dockerfile (shared packages need the whole module), migrations as
// the pre-deploy command, /healthz gating activation, capped resources.
func (c Config) service(env string, a app) railway.ServiceConfig {
	vars := map[string]any{
		"APP_ENV":          "production",
		"PORT":             fmt.Sprint(AppPort),
		"CLIENT_IP_HEADER": "X-Real-IP", // Railway's edge sets it; auth rate limits key on it
	}
	if from := c.Settings(env).FromEmail; from != ManualValue {
		vars["FROM_EMAIL"] = from
	}
	for k, v := range a.env {
		vars[k] = v
	}
	var replicas any = a.size.Replicas
	if c.Region != "" {
		replicas = map[string]any{c.Region: a.size.Replicas}
	}
	sc := railway.ServiceConfig{
		"build": map[string]any{
			"builder":        "DOCKERFILE",
			"dockerfilePath": a.dockerfile,
			"watchPatterns":  a.watch,
		},
		"preDeploy": a.migrate,
		// A deploy gate only: Railway checks it until the new deployment is
		// live, never afterwards. Keep it that way: /healthz answers 503 when
		// Doppler secrets go stale, and a liveness probe would turn a Doppler
		// outage into a restart loop. Restarts happen only on process exit.
		"healthcheck":        "/healthz",
		"healthcheckTimeout": healthcheckSeconds,
		"replicas":           replicas,
		"deploy": map[string]any{
			// restartPolicyType is left at Railway's default (ON_FAILURE):
			// declaring the default plans as a change forever.
			"restartPolicyMaxRetries": maxRestarts,
			"limitOverride": map[string]any{"containers": map[string]any{
				"cpu":         a.size.CPU,
				"memoryBytes": int64(a.size.MemoryMB) * bytesPerMiB,
			}},
		},
		"env": vars,
	}
	if !c.Upload() {
		sc["source"] = railway.Github(c.Repo, map[string]any{"branch": c.BranchFor(env)})
	} else {
		// Railway applies watch paths to `railway up` too: a re-upload with
		// no change under them is SKIPPED. Omitted (an empty list plans as a
		// change forever); railway-setup clears ones left from GitHub mode.
		delete(sc["build"].(map[string]any), "watchPatterns")
	}
	return sc
}
