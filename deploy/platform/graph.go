package platform

import (
	"fmt"

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
)

// AppPort is the port both apps listen on; public domains target it.
const AppPort = 3000

// SecretKind says how setup provisions a bootstrap variable.
type SecretKind int

const (
	// Generated: 48 random bytes, created once per service and environment
	// and sealed. Never copied between environments.
	Generated SecretKind = iota
	// Provider: SECRETS_PROVIDER, set by setup from whether this service and
	// environment has a real DOPPLER_TOKEN yet (doppler) or not (env).
	Provider
	// DopplerToken: a read-only Doppler service token for this service's
	// config. Setup leaves a placeholder; `make railway-secrets` seals the
	// real one.
	DopplerToken
	// Operator: supplied by the person running setup (RESEND_API_KEY from
	// their shell); never generated.
	Operator
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

// SecretVars lists every bootstrap variable for the configured services.
func (c Config) SecretVars() []Secret {
	out := []Secret{
		{ServiceWeb, "AUTH_SECRET", Generated},
		{ServiceWeb, "SECRETS_PROVIDER", Provider},
		{ServiceWeb, "RESEND_API_KEY", Operator},
	}
	// DOPPLER_TOKEN is listed (ignored by IaC) even with provider env, so
	// switching provider never plans deleting a real token.
	out = append(out, Secret{ServiceWeb, "DOPPLER_TOKEN", DopplerToken})
	if c.Admin {
		out = append(out,
			Secret{ServiceAdmin, "ADMIN_AUTH_SECRET", Generated},
			Secret{ServiceAdmin, "SECRETS_PROVIDER", Provider},
			Secret{ServiceAdmin, "RESEND_API_KEY", Operator},
			Secret{ServiceAdmin, "DOPPLER_TOKEN", DopplerToken},
		)
	}
	return out
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
	db := c.postgres(ServicePostgres)
	web := railway.ServiceNamed(ServiceWeb, c.service(env, app{
		dockerfile: "Dockerfile",
		migrate:    "/server migrate",
		watch:      []any{"apps/web/**", "packages/**", "go.mod", "go.sum", "Dockerfile"},
		size:       c.Resources.Web,
		env: map[string]any{
			"PUBLIC_URL":   "https://${{RAILWAY_PUBLIC_DOMAIN}}",
			"DATABASE_URL": db.Env("DATABASE_URL"),
		},
	}))
	resources := []any{db, web}
	if c.Admin {
		adminDB := c.postgres(ServiceAdminPostgres)
		vars := map[string]any{
			"ADMIN_PUBLIC_URL":   "https://${{RAILWAY_PUBLIC_DOMAIN}}",
			"DATABASE_URL":       db.Env("DATABASE_URL"),
			"ADMIN_DATABASE_URL": adminDB.Env("DATABASE_URL"),
		}
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
	}

	var ignore []string
	for _, s := range c.SecretVars() {
		ignore = append(ignore, s.Service+"/"+s.Name)
	}
	ignore = append(ignore, c.Unmanaged...)
	return Project{Project: railway.ProjectNamed(c.Project, resources), Ignore: ignore}, nil
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
		"FROM_EMAIL":       c.Email.From,
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
		"preDeploy":          a.migrate,
		"healthcheck":        "/healthz",
		"healthcheckTimeout": 60,
		"replicas":           replicas,
		"deploy": map[string]any{
			// restartPolicyType is left at Railway's default (ON_FAILURE):
			// declaring the default plans as a change forever.
			"restartPolicyMaxRetries": 5,
			"limitOverride": map[string]any{"containers": map[string]any{
				"cpu":         a.size.CPU,
				"memoryBytes": int64(a.size.MemoryMB) << 20,
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
