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
	// PerEnvironment sets what one environment's apps see as their origin
	// and sender (EnvSettings); environments not listed use the defaults.
	PerEnvironment map[string]EnvSettings `yaml:"per_environment"`
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
func (environments *Environments) UnmarshalYAML(node *yaml.Node) error {
	var list []string
	if node.Kind == yaml.ScalarNode {
		for key := range strings.SplitSeq(node.Value, ",") {
			list = append(list, strings.TrimSpace(key))
		}
	} else if err := node.Decode(&list); err != nil {
		return err
	}
	*environments = list
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

// EnvSettings are one environment's values for variables IaC would
// otherwise derive. Each is "" (the default), a value, or ManualValue: set
// by hand in Railway (a custom domain's origin, a sender on a verified
// domain), so IaC leaves the variable out of the graph and adds it to
// variables.ignore. Apply then neither sets, resets nor deletes it.
type EnvSettings struct {
	// PublicURL is the web app's PUBLIC_URL: "" or "railway" for the
	// Railway-generated domain, an https origin (a custom domain), or manual.
	PublicURL string `yaml:"public_url"`
	// AdminPublicURL is the admin app's ADMIN_PUBLIC_URL, the same way.
	AdminPublicURL string `yaml:"admin_public_url"`
	// FromEmail is FROM_EMAIL on every app service: "" for email.from, an
	// address, or manual.
	FromEmail string `yaml:"from_email"`
	// Secrets is how far this environment has moved to Doppler (with
	// secrets.provider: doppler). "" or doppler switches its services:
	// SECRETS_PROVIDER=doppler (a redeploy), and once the new deployment is
	// healthy the Railway copies of the secrets are removed. staged fills
	// its Doppler configs and seals DOPPLER_TOKEN without a deploy, leaving
	// SECRETS_PROVIDER and the Railway secrets as they are: an environment
	// not switched yet (production until a release switches it).
	Secrets string `yaml:"secrets"`
	// VolumeSnapshots overrides backups.volume_snapshots for this
	// environment (e.g. [daily] on staging); [] turns them off.
	VolumeSnapshots []string `yaml:"volume_snapshots"`
}

// Per-environment secrets modes (EnvSettings.Secrets).
const (
	SecretsSwitched = "doppler"
	SecretsStaged   = "staged"
)

// DopplerMode is env's secrets mode: "" when the provider is not Doppler,
// else SecretsSwitched or SecretsStaged.
func (config Config) DopplerMode(env string) string {
	if config.Secrets.Provider != ProviderDoppler {
		return ""
	}
	if config.PerEnvironment[env].Secrets == SecretsStaged {
		return SecretsStaged
	}
	return SecretsSwitched
}

// RailwayDomain is the PUBLIC_URL value for the Railway-generated domain.
const RailwayDomain = "railway"

// ManualValue is the EnvSettings value for a variable set by hand in Railway.
const ManualValue = "manual"

// Settings returns env's settings with the defaults filled in.
func (config Config) Settings(env string) EnvSettings {
	settings := config.PerEnvironment[env]
	if settings.PublicURL == "" {
		settings.PublicURL = RailwayDomain
	}
	if settings.AdminPublicURL == "" {
		settings.AdminPublicURL = RailwayDomain
	}
	if settings.FromEmail == "" {
		settings.FromEmail = config.Email.From
	}
	return settings
}

// HandSet lists env's variables IaC leaves alone because platform.yaml
// marks them manual, as "service/NAME". Plan and apply print them.
func (config Config) HandSet(env string) []string {
	settings := config.Settings(env)
	var out []string
	if settings.PublicURL == ManualValue {
		out = append(out, ServiceWeb+"/PUBLIC_URL")
	}
	if config.Admin && settings.AdminPublicURL == ManualValue {
		out = append(out, ServiceAdmin+"/ADMIN_PUBLIC_URL")
	}
	if settings.FromEmail == ManualValue {
		for _, service := range config.PublicServices() {
			out = append(out, service+"/FROM_EMAIL")
		}
	}
	return out
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
	// VolumeSnapshots are Railway's own snapshots of every Postgres volume
	// (Pro plan), independent of enabled: daily (Railway keeps them 6 days),
	// weekly (27 days), monthly (89 days). Setup sets the schedule through
	// Railway's API, without a redeploy. Absent: left as it is (the
	// dashboard's); []: none. per_environment.<env>.volume_snapshots
	// overrides it.
	VolumeSnapshots []string `yaml:"volume_snapshots"`
}

// SnapshotKinds are the Railway volume snapshot schedules.
var SnapshotKinds = []string{"daily", "weekly", "monthly"}

// VolumeSnapshots is env's Railway volume snapshot schedule as the API
// takes it (DAILY, WEEKLY, MONTHLY; sorted, unique), and whether
// platform.yaml manages it at all.
func (config Config) VolumeSnapshots(env string) (kinds []string, managed bool) {
	want := config.Backups.VolumeSnapshots
	if override := config.PerEnvironment[env].VolumeSnapshots; override != nil {
		want = override
	}
	if want == nil {
		return nil, false
	}
	kinds = []string{}
	for _, kind := range want {
		kinds = append(kinds, strings.ToUpper(kind))
	}
	slices.Sort(kinds)
	return slices.Compact(kinds), true
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
func (config Config) BucketRegion() string {
	if config.Backups.Region != "" {
		return config.Backups.Region
	}
	for prefix, raw := range map[string]string{"us-east": "iad", "europe": "ams", "asia": "sin"} {
		if strings.HasPrefix(config.Region, prefix) {
			return raw
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

// Defaults for what platform.yaml leaves out.
const (
	defaultKeepDaily     = 7   // a week of nightly backups
	defaultKeepWeekly    = 4   // and about a month of weeklies
	defaultWebMemoryMB   = 512 // per web replica
	defaultAdminCPU      = 0.5 // vCPU per admin replica: the admin app is light
	defaultAdminMemoryMB = 256 // per admin replica
)

var (
	defaults = Config{
		Version:      1,
		Branch:       "main",
		Environments: Environments{"production"},
		Secrets:      Secrets{Provider: ProviderDoppler, Project: DefaultDopplerProject},
		Email:        Email{From: "noreply@example.com"},
		Backups:      Backups{Enabled: true, Schedule: "0 3 * * *", KeepDaily: defaultKeepDaily, KeepWeekly: defaultKeepWeekly, Admin: true, Storage: StorageRailway},
		Resources: Resources{
			Web:   Size{CPU: 1, MemoryMB: defaultWebMemoryMB, Replicas: 1},
			Admin: Size{CPU: defaultAdminCPU, MemoryMB: defaultAdminMemoryMB, Replicas: 1},
		},
	}
	// Railway allows 1 to 32 characters.
	projectName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9 ._-]{0,31}$`)
	repoName    = regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`)
	httpsOrigin = regexp.MustCompile(`^https://[A-Za-z0-9.-]+(:[0-9]+)?$`)
	varPattern  = regexp.MustCompile(`^([A-Za-z0-9 ._-]+/)?[A-Za-z_*][A-Za-z0-9_*]*$`)
)

// Load reads and validates the file at path.
func Load(path string) (Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Config{}, err
	}
	return Parse(data)
}

// Parse decodes platform.yaml, fills defaults and validates it. Unknown keys
// are errors, so a typo never silently falls back to a default. Every
// problem is reported at once.
func Parse(data []byte) (Config, error) {
	cfg := defaults
	cfg.Resources = Resources{} // zero: filled per field below
	dec := yaml.NewDecoder(bytes.NewReader(data))
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

func (size Size) or(fallback Size) Size {
	if size.CPU == 0 {
		size.CPU = fallback.CPU
	}
	if size.MemoryMB == 0 {
		size.MemoryMB = fallback.MemoryMB
	}
	if size.Replicas == 0 {
		size.Replicas = fallback.Replicas
	}
	return size
}

// Validate reports every problem in cfg.
func (config Config) Validate() error {
	var errs []error
	add := func(format string, values ...any) { errs = append(errs, fmt.Errorf(format, values...)) }
	if config.Version != 1 {
		add("version must be 1, got %d", config.Version)
	}
	if config.Project != Auto && !projectName.MatchString(config.Project) {
		add("project must be a Railway project name (letters, digits, space . _ -; up to 32), got %q", config.Project)
	}
	if config.Repo != "" && config.Repo != Auto && !repoName.MatchString(config.Repo) {
		add("repo must be auto, the GitHub owner/name Railway builds from, or empty to always upload, got %q", config.Repo)
	}
	if strings.TrimSpace(config.Branch) == "" {
		add("branch must not be empty")
	}
	if !slices.ContainsFunc(Profiles, func(paths []string) bool { return slices.Equal(paths, config.Environments) }) {
		add("environments must be one of production | staging,production | staging,qa,production, got %q", strings.Join(config.Environments, ","))
	}
	for env, branch := range config.Branches {
		if !slices.Contains(config.Environments, env) {
			add("branches.%s: %q is not in environments", env, env)
		}
		if strings.TrimSpace(branch) == "" {
			add("branches.%s must not be empty", env)
		}
	}
	if config.Has("production") && len(config.Environments) > 1 {
		for _, env := range config.Environments {
			if env != "production" && config.BranchFor(env) == config.BranchFor("production") {
				add("branches.%s: %q is production's branch too, so every push would deploy both; give %s its own branch (default %q)", env, config.BranchFor(env), env, DefaultPreviewBranch)
			}
		}
	}
	if config.Previews && !config.Has("staging") {
		add("previews need a staging environment to copy (environments: staging,production): a preview must never start from production")
	}
	switch config.Secrets.Provider {
	case ProviderDoppler, ProviderEnv:
	default:
		add("secrets.provider must be %s or %s, got %q", ProviderDoppler, ProviderEnv, config.Secrets.Provider)
	}
	if !strings.Contains(config.Email.From, "@") {
		add("email.from must be an email address, got %q", config.Email.From)
	}
	for env, settings := range config.PerEnvironment {
		if !slices.Contains(config.Environments, env) {
			add("per_environment.%s: %q is not in environments", env, env)
		}
		for key, value := range map[string]string{"public_url": settings.PublicURL, "admin_public_url": settings.AdminPublicURL} {
			if value != "" && value != RailwayDomain && value != ManualValue && !httpsOrigin.MatchString(value) {
				add("per_environment.%s.%s must be %q, an https origin (https://host, no path) or %q, got %q", env, key, RailwayDomain, ManualValue, value)
			}
		}
		if from := settings.FromEmail; from != "" && from != ManualValue && !strings.Contains(from, "@") {
			add("per_environment.%s.from_email must be an email address or %q, got %q", env, ManualValue, from)
		}
		switch settings.Secrets {
		case "", SecretsSwitched, SecretsStaged:
		default:
			add("per_environment.%s.secrets must be %q or %q, got %q", env, SecretsSwitched, SecretsStaged, settings.Secrets)
		}
	}
	if config.AdminBootstrapEmail != "" && !strings.Contains(config.AdminBootstrapEmail, "@") {
		add("admin_bootstrap_email must be an email address, got %q", config.AdminBootstrapEmail)
	}
	for name, size := range map[string]Size{"web": config.Resources.Web, "admin": config.Resources.Admin} {
		if size.CPU < 0.1 || size.CPU > 32 {
			add("resources.%s.cpu must be between 0.1 and 32 vCPU, got %g", name, size.CPU)
		}
		if size.MemoryMB < 64 || size.MemoryMB > 32*1024 {
			add("resources.%s.memory_mb must be between 64 and 32768, got %d", name, size.MemoryMB)
		}
		if size.Replicas < 1 || size.Replicas > 50 {
			add("resources.%s.replicas must be between 1 and 50, got %d", name, size.Replicas)
		}
	}
	if config.Backups.Enabled {
		if !validCron(config.Backups.Schedule) {
			add("backups.schedule must be a 5-field cron expression in UTC (minute 0-59, hour 0-23, day 1-31, month 1-12, weekday 0-7; e.g. \"0 3 * * *\"), got %q", config.Backups.Schedule)
		}
		if config.Backups.KeepDaily < 1 || config.Backups.KeepDaily > 366 {
			add("backups.keep_daily must be between 1 and 366, got %d", config.Backups.KeepDaily)
		}
		if config.Backups.Storage != StorageRailway && config.Backups.Storage != StorageExternal {
			add("backups.storage must be %s or %s, got %q", StorageRailway, StorageExternal, config.Backups.Storage)
		}
		if config.Backups.Region != "" && !slices.Contains(BucketRegions, config.Backups.Region) {
			add("backups.region must be one of %v (Railway bucket regions), got %q", BucketRegions, config.Backups.Region)
		}
		if config.Backups.KeepWeekly < 0 || config.Backups.KeepWeekly > 520 {
			add("backups.keep_weekly must be between 0 and 520, got %d", config.Backups.KeepWeekly)
		}
	}
	snapshotLists := map[string][]string{"backups.volume_snapshots": config.Backups.VolumeSnapshots}
	for env, settings := range config.PerEnvironment {
		snapshotLists["per_environment."+env+".volume_snapshots"] = settings.VolumeSnapshots
	}
	for key, kinds := range snapshotLists {
		for _, kind := range kinds {
			if !slices.Contains(SnapshotKinds, kind) {
				add("%s: %q must be one of %v", key, kind, SnapshotKinds)
			}
		}
	}
	for _, path := range config.Unmanaged {
		if !varPattern.MatchString(path) {
			add("unmanaged_variables: %q must be NAME, NAME_* or service/NAME", path)
		}
	}
	return errors.Join(errs...)
}

// validCron checks what the backup job's parser accepts (packages/backup
// ParseSchedule; deploy/ is its own module): 5 fields of *, numbers, ranges,
// lists and steps within each field's bounds.
func validCron(expr string) bool {
	const cronFields = 5 // minute hour day-of-month month day-of-week
	fields := strings.Fields(expr)
	if len(fields) != cronFields {
		return false
	}
	bounds := [cronFields][2]int{{0, 59}, {0, 23}, {1, 31}, {1, 12}, {0, 7}}
	num := func(text string, lo, hi int) (int, bool) {
		count, err := strconv.Atoi(text)
		return count, err == nil && count >= lo && count <= hi
	}
	for index, field := range fields {
		lo, hi := bounds[index][0], bounds[index][1]
		for part := range strings.SplitSeq(field, ",") {
			rng, step, hasStep := strings.Cut(part, "/")
			if hasStep {
				if count, err := strconv.Atoi(step); err != nil || count < 1 {
					return false
				}
			}
			if rng == "*" {
				continue
			}
			left, right, isRange := strings.Cut(rng, "-")
			value, ok := num(left, lo, hi)
			if !ok {
				return false
			}
			if isRange {
				if row, ok := num(right, lo, hi); !ok || row < value {
					return false
				}
			}
		}
	}
	return true
}

// Upload reports whether services have no GitHub source: code is shipped
// with `railway up` (make railway-up) instead of Railway building on push.
func (config Config) Upload() bool { return config.Repo == "" || config.ForceUpload }

// DefaultPreviewBranch is the branch staging (and qa) deploy when
// production is not the only environment and branches does not name one:
// merges to dev reach staging, and production only takes what reaches
// branch (main).
const DefaultPreviewBranch = "dev"

// BranchFor is the git branch env deploys: branches.<env>, else branch for
// production (and for every environment of a production-only project), else
// DefaultPreviewBranch.
func (config Config) BranchFor(env string) string {
	if branch := config.Branches[env]; branch != "" {
		return branch
	}
	if env == "production" || len(config.Environments) == 1 {
		return config.Branch
	}
	return DefaultPreviewBranch
}

// Has reports whether env is one of the configured environments.
func (config Config) Has(env string) bool { return slices.Contains(config.Environments, env) }
