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
	maxRestarts = 5 // restartPolicyMaxRetries: restarts after a crash before Railway gives up
	// How long a new deployment has to answer /healthz before Railway keeps
	// the previous one. Web migrates in its pre-deploy, so it is healthy
	// within seconds. Admin deploys alongside web and cannot pass /healthz
	// until web's pre-deploy has migrated the app database (its boot refuses
	// an app database behind its migrations): its window covers that wait
	// plus the boot, so a deploy that adds app migrations is not failed.
	webHealthcheckSeconds   = 60
	adminHealthcheckSeconds = 200
	bytesPerMiB             = 1 << 20 // memoryBytes is in bytes; sizes are in MiB
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
func (config Config) SecretVars() []Secret {
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
	if config.Backups.Enabled {
		// Encrypts every backup (age). Keep a copy outside Railway: without
		// it a backup cannot be restored. _PREVIOUS decrypts older backups
		// after a rotation.
		out = append(out,
			Secret{ServiceBackup, "BACKUP_ENCRYPTION_KEY", Generated},
			Secret{ServiceBackup, "BACKUP_ENCRYPTION_KEY_PREVIOUS", Manual},
		)
		if config.Backups.Storage == StorageExternal {
			for _, backupS3Var := range BackupS3Vars {
				out = append(out, Secret{ServiceBackup, backupS3Var, Manual})
			}
		}
	}
	if config.Admin {
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
func (config Config) HasService(name string) bool {
	services := append(config.PublicServices(), ServicePostgres)
	if config.Admin {
		services = append(services, ServiceAdminPostgres)
	}
	if config.Backups.Enabled {
		services = append(services, ServiceBackup)
	}
	return slices.Contains(services, name)
}

// PublicServices are the services that get a Railway-generated domain.
func (config Config) PublicServices() []string {
	if config.Admin {
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
func (project Project) Graph() map[string]any {
	graph := project.Project.Graph()
	graph["variables"] = map[string]any{"managed": true, "ignore": project.Ignore}
	return graph
}

// Build builds the desired Railway graph for env: the web service and its
// Postgres, plus the admin service and its own Postgres when admin is on.
func (config Config) Build(env string) (Project, error) {
	if !config.Has(env) {
		return Project{}, fmt.Errorf("environment %q is not in deploy/platform.yaml environments %v", env, []string(config.Environments))
	}
	if config.Project == Auto || config.Repo == Auto {
		return Project{}, fmt.Errorf("project/repo: auto is unresolved: load platform.yaml with LoadResolved")
	}
	settings := config.Settings(env)
	db := config.postgres(ServicePostgres)
	webEnv := map[string]any{"DATABASE_URL": db.Env("DATABASE_URL")}
	setURL(webEnv, "PUBLIC_URL", settings.PublicURL)
	web := railway.ServiceNamed(ServiceWeb, config.service(env, app{
		dockerfile:  "Dockerfile",
		migrate:     "/server migrate",
		watch:       []any{"apps/web/**", "packages/**", "go.mod", "go.sum", "Dockerfile"},
		size:        config.Resources.Web,
		env:         webEnv,
		healthcheck: webHealthcheckSeconds,
	}))
	resources := []any{db, web}
	var adminBackupURL map[string]any
	if config.Admin {
		adminDB := config.postgres(ServiceAdminPostgres)
		vars := map[string]any{
			"DATABASE_URL":       db.Env("DATABASE_URL"),
			"ADMIN_DATABASE_URL": adminDB.Env("DATABASE_URL"),
		}
		setURL(vars, "ADMIN_PUBLIC_URL", settings.AdminPublicURL)
		if config.AdminBootstrapEmail != "" {
			vars["ADMIN_BOOTSTRAP_EMAIL"] = config.AdminBootstrapEmail
		}
		admin := railway.ServiceNamed(ServiceAdmin, config.service(env, app{
			dockerfile:  "apps/admin/Dockerfile",
			migrate:     "/admin migrate",
			watch:       []any{"apps/admin/**", "packages/**", "go.mod", "go.sum"},
			size:        config.Resources.Admin,
			env:         vars,
			healthcheck: adminHealthcheckSeconds,
		}))
		resources = append(resources, adminDB, admin)
		if config.Backups.Enabled && config.Backups.Admin {
			adminBackupURL = adminDB.Env("DATABASE_URL")
		}
	}
	if config.Backups.Enabled {
		resources = append(resources, config.backups(env, db.Env("DATABASE_URL"), adminBackupURL)...)
	}

	var ignore []string
	for _, secretVar := range config.SecretVars() {
		ignore = append(ignore, secretVar.Service+"/"+secretVar.Name)
	}
	// Doppler-owned secrets are setup's, never IaC's: kept while a service
	// still reads Railway, removed by setup once it reads Doppler (never by
	// an apply that could run before the switch).
	for _, service := range config.PublicServices() {
		for _, name := range RailwayCopies(service) {
			if pattern := service + "/" + name; !slices.Contains(ignore, pattern) {
				ignore = append(ignore, pattern)
			}
		}
	}
	// A pattern scoped to a service this environment does not have is an
	// error to Railway, not a no-op (docs/railway-go-iac-issues.md #18):
	// keep only the ones whose service is in the graph.
	for _, pattern := range config.Unmanaged {
		if service, _, scoped := strings.Cut(pattern, "/"); !scoped || config.HasService(service) {
			ignore = append(ignore, pattern)
		}
	}
	// Without admin_bootstrap_email, a value set by hand is left alone.
	if config.Admin && config.AdminBootstrapEmail == "" {
		ignore = append(ignore, ServiceAdmin+"/ADMIN_BOOTSTRAP_EMAIL")
	}
	// Values platform.yaml marks manual stay whatever the environment holds.
	ignore = append(ignore, config.HandSet(env)...)
	return Project{Project: railway.ProjectNamed(config.Project, resources), Ignore: ignore}, nil
}

// backups is the backup service and its storage bucket: it sleeps until
// backups.schedule (UTC), dumps the app database (and the admin one),
// encrypts the dumps, stores them in the bucket and prunes. Not a Railway
// cron: it stays up (a few MB) so people can `railway ssh` in to list,
// back up or restore over the private network. No domain, no healthcheck,
// no pre-deploy.
func (config Config) backups(env string, appURL, adminURL map[string]any) []any {
	vars := map[string]any{
		"APP_ENV":            "production",
		"DATABASE_URL":       appURL,
		"BACKUP_SCHEDULE":    config.Backups.Schedule,
		"BACKUP_KEEP_DAILY":  fmt.Sprint(config.Backups.KeepDaily),
		"BACKUP_KEEP_WEEKLY": fmt.Sprint(config.Backups.KeepWeekly),
	}
	var out []any
	if config.Backups.Storage == StorageRailway {
		bucket := railway.Bucket(BucketBackups, map[string]any{"region": config.BucketRegion()})
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
	if config.Region != "" {
		sc["replicas"] = map[string]any{config.Region: 1}
	}
	if !config.Upload() {
		sc["source"] = railway.Github(config.Repo, map[string]any{"branch": config.BranchFor(env)})
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
	// healthcheck is the seconds Railway waits for /healthz (healthcheckTimeout).
	healthcheck int
}

func (config Config) postgres(name string) railway.Resource {
	if config.Region == "" {
		return railway.Postgres(name)
	}
	return railway.Postgres(name, map[string]any{"region": config.Region})
}

// service is the shared shape of a Go app service: built from the repo root
// with its Dockerfile (shared packages need the whole module), migrations as
// the pre-deploy command, /healthz gating activation, capped resources.
func (config Config) service(env string, app app) railway.ServiceConfig {
	vars := map[string]any{
		"APP_ENV":          "production",
		"PORT":             fmt.Sprint(AppPort),
		"CLIENT_IP_HEADER": "X-Real-IP", // Railway's edge sets it; auth rate limits key on it
	}
	if from := config.Settings(env).FromEmail; from != ManualValue {
		vars["FROM_EMAIL"] = from
	}
	for key, value := range app.env {
		vars[key] = value
	}
	var replicas any = app.size.Replicas
	if config.Region != "" {
		replicas = map[string]any{config.Region: app.size.Replicas}
	}
	sc := railway.ServiceConfig{
		"build": map[string]any{
			"builder":        "DOCKERFILE",
			"dockerfilePath": app.dockerfile,
			"watchPatterns":  app.watch,
		},
		"preDeploy": app.migrate,
		// A deploy gate only: Railway checks it until the new deployment is
		// live, never afterwards. Keep it that way: /healthz answers 503 when
		// Doppler secrets go stale, and a liveness probe would turn a Doppler
		// outage into a restart loop. Restarts happen only on process exit.
		"healthcheck":        "/healthz",
		"healthcheckTimeout": app.healthcheck,
		"replicas":           replicas,
		"deploy": map[string]any{
			// restartPolicyType is left at Railway's default (ON_FAILURE):
			// declaring the default plans as a change forever.
			"restartPolicyMaxRetries": maxRestarts,
			"limitOverride": map[string]any{"containers": map[string]any{
				"cpu":         app.size.CPU,
				"memoryBytes": int64(app.size.MemoryMB) * bytesPerMiB,
			}},
		},
		"env": vars,
	}
	if !config.Upload() {
		sc["source"] = railway.Github(config.Repo, map[string]any{"branch": config.BranchFor(env)})
	} else {
		// Railway applies watch paths to `railway up` too: a re-upload with
		// no change under them is SKIPPED. Omitted (an empty list plans as a
		// change forever); railway-setup clears ones left from GitHub mode.
		delete(sc["build"].(map[string]any), "watchPatterns")
	}
	return sc
}
