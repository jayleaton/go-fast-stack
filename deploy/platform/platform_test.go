package platform_test

import (
	"encoding/json"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/jayleaton/ai-patterns-go/deploy/platform"
)

const minimal = "version: 1\nproject: acme\nrepo: acme/app\n"

func parse(t *testing.T, yaml string) platform.Config {
	t.Helper()
	cfg, err := platform.Parse([]byte(yaml))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	return cfg
}

// graph builds env's graph and round-trips it through JSON, as the CLI sees it.
func graph(t *testing.T, cfg platform.Config, env string) map[string]any {
	t.Helper()
	p, err := cfg.Build(env)
	if err != nil {
		t.Fatalf("Build(%s): %v", env, err)
	}
	b, err := json.Marshal(p.Graph())
	if err != nil {
		t.Fatal(err)
	}
	var g map[string]any
	if err := json.Unmarshal(b, &g); err != nil {
		t.Fatal(err)
	}
	return g
}

func resource(t *testing.T, g map[string]any, address string) map[string]any {
	t.Helper()
	for _, r := range g["resources"].([]any) {
		if r := r.(map[string]any); r["address"] == address {
			return r
		}
	}
	return nil
}

func addresses(g map[string]any) []string {
	var out []string
	for _, r := range g["resources"].([]any) {
		out = append(out, r.(map[string]any)["address"].(string))
	}
	slices.Sort(out)
	return out
}

func TestCheckedInPlatformYAMLIsValid(t *testing.T) {
	t.Parallel()
	cfg, err := platform.Load("../platform.yaml")
	if err != nil {
		t.Fatal(err)
	}
	// The onboarding defaults JJ chose: cheapest working setup.
	if cfg.Admin || cfg.Previews || !slices.Equal(cfg.Environments, platform.Environments{"production"}) || cfg.Secrets.Provider != platform.ProviderDoppler {
		t.Fatalf("defaults changed: admin=%v previews=%v environments=%v provider=%s", cfg.Admin, cfg.Previews, cfg.Environments, cfg.Secrets.Provider)
	}
}

func TestParseDefaults(t *testing.T) {
	t.Parallel()
	cfg := parse(t, minimal)
	if !slices.Equal(cfg.Environments, platform.Environments{"production"}) || cfg.Branch != "main" || cfg.Secrets.Provider != platform.ProviderDoppler {
		t.Fatalf("defaults: %+v", cfg)
	}
	if cfg.Resources.Web != (platform.Size{CPU: 1, MemoryMB: 512, Replicas: 1}) || cfg.Resources.Admin != (platform.Size{CPU: 0.5, MemoryMB: 256, Replicas: 1}) {
		t.Fatalf("resources: %+v", cfg.Resources)
	}
	// A partial size keeps the other defaults.
	cfg = parse(t, minimal+"resources:\n  web: { memory_mb: 256 }\n")
	if cfg.Resources.Web != (platform.Size{CPU: 1, MemoryMB: 256, Replicas: 1}) {
		t.Fatalf("partial size: %+v", cfg.Resources.Web)
	}
}

func TestParseEnvironmentProfiles(t *testing.T) {
	t.Parallel()
	for _, in := range []string{"staging,production", "[staging, production]", "staging, production"} {
		cfg := parse(t, minimal+"environments: "+in+"\n")
		if !slices.Equal(cfg.Environments, platform.Environments{"staging", "production"}) {
			t.Errorf("%q: %v", in, cfg.Environments)
		}
	}
	for _, in := range []string{"staging", "production,staging", "qa,production", "dev,staging,production", "staging,qa,production,preview"} {
		if _, err := platform.Parse([]byte(minimal + "environments: " + in + "\n")); err == nil {
			t.Errorf("%q: accepted", in)
		}
	}
}

func TestParseReportsEveryProblem(t *testing.T) {
	t.Parallel()
	_, err := platform.Parse([]byte(`version: 2
project: ""
repo: not-a-repo
environments: qa
secrets: { provider: vault }
email: { from: nobody }
resources: { web: { cpu: 64, memory_mb: 16, replicas: 100 } }
branches: { staging: dev }
unmanaged_variables: ["a/b/c"]
`))
	if err == nil {
		t.Fatal("accepted")
	}
	for _, want := range []string{"version", "project", "repo", "environments", "secrets.provider", "email.from", "resources.web.cpu", "resources.web.memory_mb", "resources.web.replicas", "branches.staging", "unmanaged_variables"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("missing %q in:\n%v", want, err)
		}
	}
}

func TestPreviewsNeedStaging(t *testing.T) {
	t.Parallel()
	if _, err := platform.Parse([]byte(minimal + "previews: true\n")); err == nil {
		t.Fatal("previews copying production accepted")
	}
	if cfg := parse(t, minimal+"previews: true\nenvironments: staging,production\n"); !cfg.Previews {
		t.Fatal("previews dropped")
	}
}

func TestParseRejectsUnknownKeys(t *testing.T) {
	t.Parallel()
	if _, err := platform.Parse([]byte(minimal + "admn: true\n")); err == nil {
		t.Fatal("typo accepted")
	}
}

func TestBuildWebOnly(t *testing.T) {
	t.Parallel()
	g := graph(t, parse(t, minimal), "production")
	if got := addresses(g); !slices.Equal(got, []string{"database.postgres", "service.web"}) {
		t.Fatalf("resources: %v", got)
	}
	web := resource(t, g, "service.web")
	if src := web["source"].(map[string]any); src["repo"] != "acme/app" || src["branch"] != "main" {
		t.Fatalf("source: %v", src)
	}
	build := web["build"].(map[string]any)
	if build["builder"] != "DOCKERFILE" || build["dockerfilePath"] != "Dockerfile" || !slices.Contains(build["watchPatterns"].([]any), any("packages/**")) {
		t.Fatalf("build: %v", build)
	}
	deploy := web["deploy"].(map[string]any)
	if deploy["preDeployCommand"].([]any)[0] != "/server migrate" || deploy["healthcheckPath"] != "/healthz" || deploy["numReplicas"] != float64(1) {
		t.Fatalf("deploy: %v", deploy)
	}
	if _, ok := deploy["restartPolicyType"]; ok {
		t.Fatal("restartPolicyType ON_FAILURE is Railway's default; declaring it drifts forever")
	}
	limits := deploy["limitOverride"].(map[string]any)["containers"].(map[string]any)
	if limits["cpu"] != float64(1) || limits["memoryBytes"] != float64(512<<20) {
		t.Fatalf("limits: %v", limits)
	}
	vars := web["variables"].(map[string]any)
	if ref := vars["DATABASE_URL"].(map[string]any); ref["type"] != "reference" || ref["resource"] != "database.postgres" {
		t.Fatalf("DATABASE_URL: %v", ref)
	}
	for _, secret := range []string{"AUTH_SECRET", "RESEND_API_KEY", "DOPPLER_TOKEN", "SECRETS_PROVIDER"} {
		if _, ok := vars[secret]; ok {
			t.Errorf("%s is in the graph; setup owns it", secret)
		}
	}
	ignore := g["variables"].(map[string]any)["ignore"].([]any)
	// Rotation variables set by hand must never be planned for deletion.
	for _, v := range []string{"web/AUTH_SECRET_PREVIOUS", "web/AUTH_SECRET_REVOKED"} {
		if !slices.Contains(ignore, any(v)) {
			t.Errorf("%s not ignored", v)
		}
	}
	if !slices.Contains(ignore, any("web/AUTH_SECRET")) || !slices.Contains(ignore, any("web/DOPPLER_TOKEN")) || slices.ContainsFunc(ignore, func(v any) bool { return strings.HasPrefix(v.(string), "admin/") }) {
		t.Fatalf("ignore: %v", ignore)
	}
}

func TestBuildAdmin(t *testing.T) {
	t.Parallel()
	cfg := parse(t, minimal+"admin: true\nenvironments: staging,qa,production\nadmin_bootstrap_email: ops@acme.dev\n")
	for _, env := range cfg.Environments {
		g := graph(t, cfg, env)
		if got := addresses(g); !slices.Equal(got, []string{"database.admin-postgres", "database.postgres", "service.admin", "service.web"}) {
			t.Fatalf("%s resources: %v", env, got)
		}
		admin := resource(t, g, "service.admin")
		vars := admin["variables"].(map[string]any)
		if vars["ADMIN_DATABASE_URL"].(map[string]any)["resource"] != "database.admin-postgres" || vars["DATABASE_URL"].(map[string]any)["resource"] != "database.postgres" {
			t.Fatalf("admin databases: %v", vars)
		}
		if vars["ADMIN_BOOTSTRAP_EMAIL"].(map[string]any)["value"] != "ops@acme.dev" {
			t.Fatalf("bootstrap email: %v", vars["ADMIN_BOOTSTRAP_EMAIL"])
		}
		if admin["deploy"].(map[string]any)["preDeployCommand"].([]any)[0] != "/admin migrate" || admin["build"].(map[string]any)["dockerfilePath"] != "apps/admin/Dockerfile" {
			t.Fatalf("admin build/deploy: %v %v", admin["build"], admin["deploy"])
		}
	}
	if !slices.Equal(cfg.PublicServices(), []string{"web", "admin"}) {
		t.Fatalf("public: %v", cfg.PublicServices())
	}
}

func TestBuildUnknownEnvironment(t *testing.T) {
	t.Parallel()
	if _, err := parse(t, minimal).Build("staging"); err == nil {
		t.Fatal("built an environment not in platform.yaml")
	}
}

func TestBuildPerEnvironmentBranch(t *testing.T) {
	t.Parallel()
	cfg := parse(t, minimal+"environments: staging,production\nbranches: { production: release }\n")
	if b := resource(t, graph(t, cfg, "production"), "service.web")["source"].(map[string]any)["branch"]; b != "release" {
		t.Fatalf("production branch %v", b)
	}
	if b := resource(t, graph(t, cfg, "staging"), "service.web")["source"].(map[string]any)["branch"]; b != "main" {
		t.Fatalf("staging branch %v", b)
	}
}

func TestBuildUploadModeHasNoSource(t *testing.T) {
	t.Parallel()
	cfg := parse(t, "version: 1\nproject: acme\nrepo: \"\"\n")
	if !cfg.Upload() {
		t.Fatal("empty repo is upload mode")
	}
	web := resource(t, graph(t, cfg, "production"), "service.web")
	if src, ok := web["source"]; ok {
		t.Fatalf("source: %v", src)
	}
	if w, ok := web["build"].(map[string]any)["watchPatterns"]; ok {
		t.Fatalf("watch paths skip railway up deploys: %v", w)
	}
}

func TestBuildRegionPinsEveryService(t *testing.T) {
	t.Parallel()
	g := graph(t, parse(t, minimal+"region: europe-west4\n"), "production")
	web := resource(t, g, "service.web")["deploy"].(map[string]any)
	if web["multiRegionConfig"].(map[string]any)["europe-west4"].(map[string]any)["numReplicas"] != float64(1) {
		t.Fatalf("web region: %v", web)
	}
	db := resource(t, g, "database.postgres")["deploy"].(map[string]any)
	if _, ok := db["multiRegionConfig"].(map[string]any)["europe-west4"]; !ok {
		t.Fatalf("postgres region: %v", db)
	}
}

func TestSecretVarsFollowProvider(t *testing.T) {
	t.Parallel()
	has := func(cfg platform.Config, service, name string) bool {
		return slices.ContainsFunc(cfg.SecretVars(), func(s platform.Secret) bool { return s.Service == service && s.Name == name })
	}
	// DOPPLER_TOKEN stays ignored with provider env: switching provider must
	// never plan deleting a real token.
	cfg := parse(t, minimal+"admin: true\nsecrets: { provider: env }\n")
	if !has(cfg, "web", "DOPPLER_TOKEN") || !has(cfg, "admin", "DOPPLER_TOKEN") || !has(cfg, "admin", "ADMIN_AUTH_SECRET") || !has(cfg, "web", "SECRETS_PROVIDER") {
		t.Fatalf("env provider: %+v", cfg.SecretVars())
	}
	cfg = parse(t, minimal)
	if !has(cfg, "web", "DOPPLER_TOKEN") || has(cfg, "admin", "ADMIN_AUTH_SECRET") {
		t.Fatalf("doppler, no admin: %+v", cfg.SecretVars())
	}
}

func TestResolveAuto(t *testing.T) {
	cfg := parse(t, "version: 1\nproject: auto\nrepo: auto\n")
	for _, tc := range []struct{ origin, dir, project, repo string }{
		{"https://github.com/acme/shop.git", "/src/x", "shop", "acme/shop"},
		{"git@github.com:acme/shop.git", "/src/x", "shop", "acme/shop"},
		{"ssh://git@github.com/acme/shop", "", "shop", "acme/shop"},
		{"https://token@github.com/acme/shop/", "", "shop", "acme/shop"},
		{"https://gitlab.com/acme/shop.git", "/src/my-app", "my-app", ""}, // not GitHub: upload mode
		{"", "/src/my-app", "my-app", ""},
	} {
		got, err := cfg.Resolve(tc.origin, tc.dir)
		if err != nil || got.Project != tc.project || got.Repo != tc.repo {
			t.Errorf("Resolve(%q, %q) = %q, %q, %v; want %q, %q", tc.origin, tc.dir, got.Project, got.Repo, err, tc.project, tc.repo)
		}
	}
	long, err := cfg.Resolve("https://github.com/acme/a-very-long-repository-name-for-railway.git", "")
	if err != nil || long.Project != "a-very-long-repository-name-for" {
		t.Errorf("long repo name: %q %v (Railway allows 32 characters)", long.Project, err)
	}
	if _, err := cfg.Resolve("", ""); err == nil {
		t.Error("project: auto resolved without a checkout")
	}
	// Explicit values win over the checkout.
	got, err := parse(t, minimal).Resolve("https://github.com/other/thing.git", "/x")
	if err != nil || got.Project != "acme" || got.Repo != "acme/app" {
		t.Errorf("explicit values changed: %q %q %v", got.Project, got.Repo, err)
	}
}

func TestBuildRefusesUnresolvedAuto(t *testing.T) {
	if _, err := parse(t, "version: 1\nproject: auto\nrepo: auto\n").Build("production"); err == nil {
		t.Fatal("built a graph with repo: auto")
	}
}

func TestForceUploadDropsSource(t *testing.T) {
	cfg := parse(t, minimal)
	cfg.ForceUpload = true
	if _, ok := resource(t, graph(t, cfg, "production"), "service.web")["source"]; ok {
		t.Fatal("ForceUpload kept the GitHub source")
	}
}

func TestEditKeepsCommentsAndValidates(t *testing.T) {
	src := []byte("# top\nversion: 1\nproject: auto # name\nrepo: auto\nenvironments: [production]\nresources:\n  web: { cpu: 1 }\n")
	out, changed, err := platform.Edit(src, []platform.Key{{"project", "shop"}, {"environments", "staging,production"}, {"admin", "true"}, {"region", ""}})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(changed, []string{"project", "environments", "admin", "region"}) {
		t.Fatalf("changed: %v", changed)
	}
	s := string(out)
	for _, want := range []string{"# top", "project: shop # name", "environments: staging,production", "admin: true", `region: ""`, "  web: { cpu: 1 }"} {
		if !strings.Contains(s, want) {
			t.Errorf("missing %q in:\n%s", want, s)
		}
	}
	cfg := parse(t, s)
	if cfg.Project != "shop" || !cfg.Admin || !slices.Equal(cfg.Environments, platform.Environments{"staging", "production"}) {
		t.Fatalf("parsed: %+v", cfg)
	}
	if again, changed, err := platform.Edit(out, []platform.Key{{"project", "shop"}}); err != nil || len(changed) != 0 || string(again) != s {
		t.Fatalf("no-op edit changed the file: %v %v", changed, err)
	}
	if _, _, err := platform.Edit(src, []platform.Key{{"environments", "qa"}}); err == nil {
		t.Fatal("invalid edit accepted")
	}
	// Only the edited values change: every other byte stays.
	full, err := os.ReadFile("../platform.yaml")
	if err != nil {
		t.Fatal(err)
	}
	edited, _, err := platform.Edit(full, []platform.Key{{"project", "shop"}})
	if err != nil || strings.Replace(string(full), "project: auto", "project: shop", 1) != string(edited) {
		t.Fatalf("Edit touched more than the value (err %v)", err)
	}
}
