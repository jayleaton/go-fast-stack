// Package platform reads deploy/platform.yaml, the one file a team edits to
// decide its Railway project (admin yes/no, environments, previews, sizes),
// and turns it into the Railway Infrastructure as Code graph for one
// environment. It is pure: no network, no env reads; the IaC entry point
// (deploy/.railway/railway.go) and the setup command (deploy/cmd/railway-setup)
// both call it, so `railway config plan` and `make railway-plan` agree.
package platform

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"regexp"
	"slices"
	"strings"

	"go.yaml.in/yaml/v3"
)

// The environment profiles a template user picks from. Order is deploy
// order: setup applies staging before qa before production.
var Profiles = [][]string{
	{"production"},
	{"staging", "production"},
	{"staging", "qa", "production"},
}

// Secret providers the web and admin apps understand (SECRETS_PROVIDER).
const (
	ProviderDoppler = "doppler"
	ProviderEnv     = "env"
)

// Config is deploy/platform.yaml.
type Config struct {
	Version      int               `yaml:"version"`
	Project      string            `yaml:"project"`
	Workspace    string            `yaml:"workspace"`
	Repo         string            `yaml:"repo"`
	Branch       string            `yaml:"branch"`
	Branches     map[string]string `yaml:"branches"`
	Admin        bool              `yaml:"admin"`
	Environments Environments      `yaml:"environments"`
	Previews     bool              `yaml:"previews"`
	Region       string            `yaml:"region"`
	Secrets      Secrets           `yaml:"secrets"`
	Email        Email             `yaml:"email"`
	// AdminBootstrapEmail is seeded as the first admin by `admin migrate`.
	AdminBootstrapEmail string    `yaml:"admin_bootstrap_email"`
	Resources           Resources `yaml:"resources"`
	// Unmanaged are extra variable names (or service/NAME patterns) Railway
	// keeps but IaC never sets or deletes: values you add in the dashboard.
	Unmanaged []string `yaml:"unmanaged_variables"`
}

// Environments accepts a YAML list or a comma-separated string.
type Environments []string

// UnmarshalYAML implements yaml.Unmarshaler.
func (e *Environments) UnmarshalYAML(n *yaml.Node) error {
	var list []string
	if n.Kind == yaml.ScalarNode {
		for s := range strings.SplitSeq(n.Value, ",") {
			list = append(list, strings.TrimSpace(s))
		}
	} else if err := n.Decode(&list); err != nil {
		return err
	}
	*e = list
	return nil
}

// Secrets picks the live-secrets provider.
type Secrets struct {
	Provider string `yaml:"provider"`
}

// Email holds the sender address auth emails use.
type Email struct {
	From string `yaml:"from"`
}

// Resources are the per-service caps. Railway bills what a container
// actually uses; the caps only bound a runaway process.
type Resources struct {
	Web   Size `yaml:"web"`
	Admin Size `yaml:"admin"`
}

// Size is one service's replica count and per-replica limits.
type Size struct {
	CPU      float64 `yaml:"cpu"`       // vCPU cap per replica
	MemoryMB int     `yaml:"memory_mb"` // memory cap per replica
	Replicas int     `yaml:"replicas"`
}

var (
	defaults = Config{
		Version:      1,
		Branch:       "main",
		Environments: Environments{"production"},
		Secrets:      Secrets{Provider: ProviderDoppler},
		Email:        Email{From: "noreply@example.com"},
		Resources: Resources{
			Web:   Size{CPU: 1, MemoryMB: 512, Replicas: 1},
			Admin: Size{CPU: 0.5, MemoryMB: 256, Replicas: 1},
		},
	}
	projectName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9 ._-]{0,62}$`)
	repoName    = regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`)
	varPattern  = regexp.MustCompile(`^([A-Za-z0-9 ._-]+/)?[A-Za-z_*][A-Za-z0-9_*]*$`)
)

// Load reads and validates the file at path.
func Load(path string) (Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return Config{}, err
	}
	return Parse(b)
}

// Parse decodes platform.yaml, fills defaults and validates it. Unknown keys
// are errors, so a typo never silently falls back to a default. Every
// problem is reported at once.
func Parse(b []byte) (Config, error) {
	cfg := defaults
	cfg.Resources = Resources{} // zero: filled per field below
	dec := yaml.NewDecoder(bytes.NewReader(b))
	dec.KnownFields(true)
	if err := dec.Decode(&cfg); err != nil {
		return Config{}, fmt.Errorf("platform.yaml: %w", err)
	}
	cfg.Resources.Web = cfg.Resources.Web.or(defaults.Resources.Web)
	cfg.Resources.Admin = cfg.Resources.Admin.or(defaults.Resources.Admin)
	if err := cfg.Validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

func (s Size) or(d Size) Size {
	if s.CPU == 0 {
		s.CPU = d.CPU
	}
	if s.MemoryMB == 0 {
		s.MemoryMB = d.MemoryMB
	}
	if s.Replicas == 0 {
		s.Replicas = d.Replicas
	}
	return s
}

// Validate reports every problem in cfg.
func (c Config) Validate() error {
	var errs []error
	add := func(format string, a ...any) { errs = append(errs, fmt.Errorf(format, a...)) }
	if c.Version != 1 {
		add("version must be 1, got %d", c.Version)
	}
	if !projectName.MatchString(c.Project) {
		add("project must be a Railway project name (letters, digits, space . _ -; up to 63), got %q", c.Project)
	}
	if c.Repo != "" && !repoName.MatchString(c.Repo) {
		add("repo must be the GitHub owner/name Railway builds from (or empty to upload with make railway-up), got %q", c.Repo)
	}
	if strings.TrimSpace(c.Branch) == "" {
		add("branch must not be empty")
	}
	if !slices.ContainsFunc(Profiles, func(p []string) bool { return slices.Equal(p, c.Environments) }) {
		add("environments must be one of production | staging,production | staging,qa,production, got %q", strings.Join(c.Environments, ","))
	}
	for env, branch := range c.Branches {
		if !slices.Contains(c.Environments, env) {
			add("branches.%s: %q is not in environments", env, env)
		}
		if strings.TrimSpace(branch) == "" {
			add("branches.%s must not be empty", env)
		}
	}
	if c.Previews && !c.Has("staging") {
		add("previews need a staging environment to copy (environments: staging,production): a preview must never start from production")
	}
	switch c.Secrets.Provider {
	case ProviderDoppler, ProviderEnv:
	default:
		add("secrets.provider must be %s or %s, got %q", ProviderDoppler, ProviderEnv, c.Secrets.Provider)
	}
	if !strings.Contains(c.Email.From, "@") {
		add("email.from must be an email address, got %q", c.Email.From)
	}
	if c.AdminBootstrapEmail != "" && !strings.Contains(c.AdminBootstrapEmail, "@") {
		add("admin_bootstrap_email must be an email address, got %q", c.AdminBootstrapEmail)
	}
	for name, s := range map[string]Size{"web": c.Resources.Web, "admin": c.Resources.Admin} {
		if s.CPU < 0.1 || s.CPU > 32 {
			add("resources.%s.cpu must be between 0.1 and 32 vCPU, got %g", name, s.CPU)
		}
		if s.MemoryMB < 64 || s.MemoryMB > 32*1024 {
			add("resources.%s.memory_mb must be between 64 and 32768, got %d", name, s.MemoryMB)
		}
		if s.Replicas < 1 || s.Replicas > 50 {
			add("resources.%s.replicas must be between 1 and 50, got %d", name, s.Replicas)
		}
	}
	for _, p := range c.Unmanaged {
		if !varPattern.MatchString(p) {
			add("unmanaged_variables: %q must be NAME, NAME_* or service/NAME", p)
		}
	}
	return errors.Join(errs...)
}

// Upload reports whether services have no GitHub source: code is shipped
// with `railway up` (make railway-up) instead of Railway building on push.
func (c Config) Upload() bool { return c.Repo == "" }

// BranchFor is the git branch env deploys.
func (c Config) BranchFor(env string) string {
	if b := c.Branches[env]; b != "" {
		return b
	}
	return c.Branch
}

// Has reports whether env is one of the configured environments.
func (c Config) Has(env string) bool { return slices.Contains(c.Environments, env) }
