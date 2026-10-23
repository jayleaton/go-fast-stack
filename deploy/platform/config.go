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
	"strconv"
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

// DefaultDopplerProject is the Doppler project a template checkout uses.
const DefaultDopplerProject = "go-fast-stack"

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
	// Backups: a nightly encrypted pg_dump into a Railway storage bucket.
	Backups Backups `yaml:"backups"`
	// Unmanaged are extra variable names (or service/NAME patterns) Railway
	// keeps but IaC never sets or deletes: values you add in the dashboard.
	Unmanaged []string `yaml:"unmanaged_variables"`

	// ForceUpload deploys by `railway up` even though Repo is set: setup
	// uses it until Railway can read the repo (GitHub app not installed
	// yet). Not a platform.yaml key; PLATFORM_SOURCE=upload sets it.
	ForceUpload bool `yaml:"-"`
}

// Auto, as project or repo, is resolved from the git checkout (Resolve).
const Auto = "auto"

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
	// Project is the Doppler project (default DefaultDopplerProject); auto:
	// the Railway project's name.
	Project string `yaml:"project"`
}

// Email holds the sender address auth emails use.
type Email struct {
	From string `yaml:"from"`
}

// Backups configure the backup cron service (apps/backup).
type Backups struct {
	Enabled    bool   `yaml:"enabled"`
	Schedule   string `yaml:"schedule"`    // cron, UTC
	KeepDaily  int    `yaml:"keep_daily"`  // newest backup of each of the last N days
	KeepWeekly int    `yaml:"keep_weekly"` // newest of each of the last N ISO weeks
	Admin      bool   `yaml:"admin"`       // also the admin database (with admin: true)
	// Region is the bucket's region: sjc, iad, ams or sin (Railway bucket
	// regions, not service regions). Empty: the one nearest region:, or sjc.
	Region string `yaml:"region"`
	// Storage: railway (a Railway storage bucket, created by setup) or
	// external (any S3-compatible bucket, e.g. Cloudflare R2 or Backblaze B2,
	// off Railway: survives losing the project). External: set BACKUP_S3_*
	// on the backup service yourself; IaC leaves them alone.
	Storage string `yaml:"storage"`
}

// Backup storage kinds (backups.storage).
const (
	StorageRailway  = "railway"
	StorageExternal = "external"
)

// BackupS3Vars are the backup service's bucket settings: references to the
// Railway bucket, or set by hand with storage: external.
var BackupS3Vars = []string{"BACKUP_S3_ENDPOINT", "BACKUP_S3_BUCKET", "BACKUP_S3_REGION", "BACKUP_S3_ACCESS_KEY_ID", "BACKUP_S3_SECRET_ACCESS_KEY", "BACKUP_S3_PATH_STYLE"}

// BucketRegions are the regions Railway storage buckets can live in.
var BucketRegions = []string{"sjc", "iad", "ams", "sin"}

// BucketRegion is backups.region, or the bucket region nearest the
// services' region (us-west* sjc, us-east* iad, europe* ams, asia* sin).
func (c Config) BucketRegion() string {
	if c.Backups.Region != "" {
		return c.Backups.Region
	}
	for prefix, r := range map[string]string{"us-east": "iad", "europe": "ams", "asia": "sin"} {
		if strings.HasPrefix(c.Region, prefix) {
			return r
		}
	}
	return "sjc"
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
		Secrets:      Secrets{Provider: ProviderDoppler, Project: DefaultDopplerProject},
		Email:        Email{From: "noreply@example.com"},
		Backups:      Backups{Enabled: true, Schedule: "0 3 * * *", KeepDaily: 7, KeepWeekly: 4, Admin: true, Storage: StorageRailway},
		Resources: Resources{
			Web:   Size{CPU: 1, MemoryMB: 512, Replicas: 1},
			Admin: Size{CPU: 0.5, MemoryMB: 256, Replicas: 1},
		},
	}
	// Railway allows 1 to 32 characters.
	projectName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9 ._-]{0,31}$`)
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
	if c.Project != Auto && !projectName.MatchString(c.Project) {
		add("project must be a Railway project name (letters, digits, space . _ -; up to 32), got %q", c.Project)
	}
	if c.Repo != "" && c.Repo != Auto && !repoName.MatchString(c.Repo) {
		add("repo must be auto, the GitHub owner/name Railway builds from, or empty to always upload, got %q", c.Repo)
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
	if c.Has("production") && len(c.Environments) > 1 {
		for _, env := range c.Environments {
			if env != "production" && c.BranchFor(env) == c.BranchFor("production") {
				add("branches.%s: %q is production's branch too, so every push would deploy both; give %s its own branch (default %q)", env, c.BranchFor(env), env, DefaultPreviewBranch)
			}
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
	if c.Backups.Enabled {
		if !validCron(c.Backups.Schedule) {
			add("backups.schedule must be a 5-field cron expression in UTC (minute 0-59, hour 0-23, day 1-31, month 1-12, weekday 0-7; e.g. \"0 3 * * *\"), got %q", c.Backups.Schedule)
		}
		if c.Backups.KeepDaily < 1 || c.Backups.KeepDaily > 366 {
			add("backups.keep_daily must be between 1 and 366, got %d", c.Backups.KeepDaily)
		}
		if c.Backups.Storage != StorageRailway && c.Backups.Storage != StorageExternal {
			add("backups.storage must be %s or %s, got %q", StorageRailway, StorageExternal, c.Backups.Storage)
		}
		if c.Backups.Region != "" && !slices.Contains(BucketRegions, c.Backups.Region) {
			add("backups.region must be one of %v (Railway bucket regions), got %q", BucketRegions, c.Backups.Region)
		}
		if c.Backups.KeepWeekly < 0 || c.Backups.KeepWeekly > 520 {
			add("backups.keep_weekly must be between 0 and 520, got %d", c.Backups.KeepWeekly)
		}
	}
	for _, p := range c.Unmanaged {
		if !varPattern.MatchString(p) {
			add("unmanaged_variables: %q must be NAME, NAME_* or service/NAME", p)
		}
	}
	return errors.Join(errs...)
}

// validCron checks what the backup job's parser accepts (packages/backup
// ParseSchedule; deploy/ is its own module): 5 fields of *, numbers, ranges,
// lists and steps within each field's bounds.
func validCron(expr string) bool {
	f := strings.Fields(expr)
	if len(f) != 5 {
		return false
	}
	bounds := [5][2]int{{0, 59}, {0, 23}, {1, 31}, {1, 12}, {0, 7}}
	num := func(s string, lo, hi int) (int, bool) {
		n, err := strconv.Atoi(s)
		return n, err == nil && n >= lo && n <= hi
	}
	for i, field := range f {
		lo, hi := bounds[i][0], bounds[i][1]
		for part := range strings.SplitSeq(field, ",") {
			rng, step, hasStep := strings.Cut(part, "/")
			if hasStep {
				if n, err := strconv.Atoi(step); err != nil || n < 1 {
					return false
				}
			}
			if rng == "*" {
				continue
			}
			a, b, isRange := strings.Cut(rng, "-")
			x, ok := num(a, lo, hi)
			if !ok {
				return false
			}
			if isRange {
				if y, ok := num(b, lo, hi); !ok || y < x {
					return false
				}
			}
		}
	}
	return true
}

// Upload reports whether services have no GitHub source: code is shipped
// with `railway up` (make railway-up) instead of Railway building on push.
func (c Config) Upload() bool { return c.Repo == "" || c.ForceUpload }

// DefaultPreviewBranch is the branch staging (and qa) deploy when
// production is not the only environment and branches does not name one:
// merges to dev reach staging, and production only takes what reaches
// branch (main).
const DefaultPreviewBranch = "dev"

// BranchFor is the git branch env deploys: branches.<env>, else branch for
// production (and for every environment of a production-only project), else
// DefaultPreviewBranch.
func (c Config) BranchFor(env string) string {
	if branch := c.Branches[env]; branch != "" {
		return branch
	}
	if env == "production" || len(c.Environments) == 1 {
		return c.Branch
	}
	return DefaultPreviewBranch
}

// Has reports whether env is one of the configured environments.
func (c Config) Has(env string) bool { return slices.Contains(c.Environments, env) }
