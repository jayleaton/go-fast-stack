package verify_test

import (
	"slices"
	"strings"
	"testing"

	"github.com/jayleaton/go-fast-stack/scripts/cilocal/verify"
)

func TestClassifySelectsTheAreasAFileFeeds(test *testing.T) {
	test.Parallel()
	everythingButMobile := verify.Areas{Everything: true, Go: true, Migrations: true, Deploy: true, Workflows: true}
	for _, testCase := range []struct {
		file string
		want verify.Areas
	}{
		{"docs/CI.md", verify.Areas{}},
		{"README.md", verify.Areas{}},
		{"AGENTS.md", verify.Areas{}},
		{"apps/web/internal/api/widgets.go", verify.Areas{Go: true}},
		{"apps/web/README.md", verify.Areas{Go: true}},
		{"Makefile", everythingButMobile},
		{"mise.toml", everythingButMobile},
		{"scripts/cilocal/run.go", everythingButMobile},
		{"scripts/cilocal/verify/plan.go", everythingButMobile},
		{"apps/mobile-app/app/index.tsx", verify.Areas{Mobile: true}},
		{"apps/web/internal/config/routes.go", verify.Areas{Go: true, Mobile: true}},
		{"deploy/main.go", verify.Areas{Deploy: true}},
		{".env.example", verify.Areas{Go: true, Deploy: true}},
		{".golangci.yml", verify.Areas{Go: true, Deploy: true}},
		{"tools/go.mod", verify.Areas{Go: true, Deploy: true}},
		{"scripts/namelint/main.go", verify.Areas{Go: true, Deploy: true}},
		{".github/workflows/ci.yml", verify.Areas{Workflows: true}},
		{".github/actions/setup/action.yml", verify.Areas{Workflows: true}},
		{".github/pull_request_template.md", verify.Areas{}},
		{".gitleaks.toml", verify.Areas{}},
		{"packages/database/migrations/00042_widgets.sql", verify.Areas{Go: true, Migrations: true}},
		{"migrations/00001_init.sql", verify.Areas{Go: true, Migrations: true}},
		{"scripts/check-migration-order.sh", verify.Areas{Go: true, Migrations: true}},
		{"packages/database/queries/widgets.sql", verify.Areas{Go: true}},
	} {
		test.Run(testCase.file, func(test *testing.T) {
			test.Parallel()
			if got := verify.Classify(testCase.file); got != testCase.want {
				test.Fatalf("Classify(%q) = %+v, want %+v", testCase.file, got, testCase.want)
			}
		})
	}
}

func TestSelectAreasCombinesTheChangedFiles(test *testing.T) {
	test.Parallel()
	everythingButMobile := verify.Areas{Everything: true, Go: true, Migrations: true, Deploy: true, Workflows: true}
	tooMany := make([]string, verify.TooManyFiles)
	for index := range tooMany {
		tooMany[index] = "docs/page.md"
	}
	for _, testCase := range []struct {
		name  string
		files []string
		want  verify.Areas
	}{
		{"empty diff runs everything", nil, everythingButMobile},
		{"too many files run everything", tooMany, everythingButMobile},
		{"one short of too many trusts the diff", tooMany[1:], verify.Areas{}},
		{"docs only", []string{"docs/CI.md", "README.md"}, verify.Areas{}},
		{"go and mobile", []string{"apps/web/x.go", "apps/mobile-app/app.ts"}, verify.Areas{Go: true, Mobile: true}},
		{"workflow and migration", []string{".github/workflows/ci.yml", "migrations/1.sql"}, verify.Areas{Go: true, Migrations: true, Workflows: true}},
		{"makefile with mobile", []string{"Makefile", "apps/mobile-app/app.ts"}, verify.Areas{Everything: true, Go: true, Migrations: true, Deploy: true, Workflows: true, Mobile: true}},
	} {
		test.Run(testCase.name, func(test *testing.T) {
			test.Parallel()
			if got := verify.SelectAreas(testCase.files); got != testCase.want {
				test.Fatalf("SelectAreas = %+v, want %+v", got, testCase.want)
			}
		})
	}
}

func TestAreasNamesListsTheSelectedAreasInOrder(test *testing.T) {
	test.Parallel()
	for _, testCase := range []struct {
		name  string
		areas verify.Areas
		want  []string
	}{
		{"none", verify.Areas{}, []string{}},
		{"go and mobile", verify.Areas{Go: true, Mobile: true}, []string{"go", "mobile"}},
		{"all", verify.Areas{Everything: true, Go: true, Migrations: true, Deploy: true, Mobile: true, Workflows: true}, []string{"everything", "go", "migrations", "deploy", "mobile", "workflows"}},
	} {
		test.Run(testCase.name, func(test *testing.T) {
			test.Parallel()
			if got := testCase.areas.Names(); !slices.Equal(got, testCase.want) {
				test.Fatalf("Names = %q, want %q", got, testCase.want)
			}
		})
	}
}

func TestParseAffectedReadsKeyValueLines(test *testing.T) {
	test.Parallel()
	got := verify.ParseAffected("full=false\npackages=github.com/x/a github.com/x/b\ndirs=./a ./b\nsqlc=true\ntempl=true\ngenerate=./a\nsmoke=true\nnoise line\nunknown=1\n")
	want := verify.Affected{
		Full: false, Packages: []string{"github.com/x/a", "github.com/x/b"}, Dirs: []string{"./a", "./b"},
		SQLC: true, Templ: true, Generate: []string{"./a"}, Smoke: true,
	}
	if got.Full != want.Full || got.SQLC != want.SQLC || got.Templ != want.Templ || got.Smoke != want.Smoke ||
		!slices.Equal(got.Packages, want.Packages) || !slices.Equal(got.Dirs, want.Dirs) || !slices.Equal(got.Generate, want.Generate) {
		test.Fatalf("ParseAffected = %+v, want %+v", got, want)
	}
	if full := verify.ParseAffected("full=true\npackages=./...\n"); !full.Full || !slices.Equal(full.Packages, []string{"./..."}) {
		test.Fatalf("full run = %+v", full)
	}
}

func stepIDs(steps []verify.Step) []string {
	ids := []string{}
	for _, step := range steps {
		ids = append(ids, step.ID())
	}
	return ids
}

func TestPlanRunsTheSelectedSteps(test *testing.T) {
	test.Parallel()
	const (
		gitleaks   = "secrets › gitleaks"
		order      = "migrations › migration order"
		build      = "go › go build"
		lint       = "go › make lint"
		generated  = "go › make check-generated"
		tests      = "go › make test"
		layering   = "go › make lint-layering"
		lintTests  = "go › make lint-tests"
		floors     = "go › coverage floors exist"
		smoke      = "go › make release-smoke"
		actionlint = "workflows › make actionlint"
		deploy     = "deploy › make deploy-check"
		mobile     = "mobile › mobile checks"
	)
	for _, testCase := range []struct {
		name     string
		areas    verify.Areas
		affected string
		want     []string
	}{
		{"docs only scans secrets", verify.SelectAreas([]string{"docs/CI.md"}), "", []string{gitleaks}},
		{"go without packages builds nothing", verify.Areas{Go: true}, "full=false\npackages=\ndirs=\n", []string{gitleaks}},
		{"go packages", verify.Areas{Go: true}, "packages=github.com/x/a\ndirs=./a\n", []string{gitleaks, build, lint, tests}},
		{"generators only", verify.Areas{Go: true}, "templ=true\n", []string{gitleaks, generated}},
		{"sqlc", verify.Areas{Go: true}, "sqlc=true\npackages=github.com/x/a\ndirs=./a", []string{gitleaks, build, lint, generated, tests}},
		{"generate list", verify.Areas{Go: true}, "generate=./a\n", []string{gitleaks, generated}},
		{"full run probes", verify.Areas{Go: true}, "full=true\npackages=./...\ndirs=./...\n", []string{gitleaks, build, lint, tests, layering, lintTests, floors}},
		{"smoke", verify.Areas{Go: true}, "smoke=true\n", []string{gitleaks, smoke}},
		{"affected ignored without go", verify.Areas{Deploy: true}, "full=true\npackages=./...\nsmoke=true\n", []string{gitleaks, deploy}},
		{"everything", verify.SelectAreas([]string{"Makefile", "apps/mobile-app/app.ts"}), "full=true\npackages=./...\ndirs=./...\nsqlc=true\nsmoke=true\n",
			[]string{gitleaks, order, build, lint, generated, tests, layering, lintTests, floors, smoke, actionlint, deploy, mobile}},
	} {
		test.Run(testCase.name, func(test *testing.T) {
			test.Parallel()
			steps := verify.Plan(testCase.areas, verify.ParseAffected(testCase.affected), "base-sha")
			if got := stepIDs(steps); !slices.Equal(got, testCase.want) {
				test.Fatalf("Plan = %q, want %q", got, testCase.want)
			}
			first := steps[0]
			if first.Item != "secrets" || !first.Local || !slices.Contains(first.Run, "base-sha") {
				test.Fatalf("first step = %+v, want the local secret scan against the base", first)
			}
		})
	}
}

func TestPlanScopesGoStepsToTheAffectedPackages(test *testing.T) {
	test.Parallel()
	steps := verify.Plan(verify.Areas{Go: true, Migrations: true}, verify.ParseAffected("packages=github.com/x/a github.com/x/b\ndirs=./a ./b"), "base")
	byName := map[string]verify.Step{}
	for _, step := range steps {
		byName[step.Name] = step
	}
	if got := byName["go build"].Run; !slices.Equal(got, []string{"go", "build", "github.com/x/a", "github.com/x/b"}) {
		test.Fatalf("go build runs %q", got)
	}
	if got := strings.Join(byName["make lint"].Run, " "); got != "make lint LINT_DIRS=./a ./b" {
		test.Fatalf("make lint runs %q", got)
	}
	if got := strings.Join(byName["make test"].Run, " "); got != "make test TEST_PKGS=github.com/x/a github.com/x/b" {
		test.Fatalf("make test runs %q", got)
	}
	if !byName["migration order"].Local || byName["go build"].Local {
		test.Fatal("the migration order runs locally and go build does not")
	}
}

func TestOnlyFiltersByItemOrJob(test *testing.T) {
	test.Parallel()
	steps := verify.Plan(verify.SelectAreas(nil), verify.ParseAffected("full=true\npackages=./...\ndirs=./...\nsmoke=true"), "base")
	for _, testCase := range []struct {
		name  string
		names []string
		want  []string
	}{
		{"none keeps all", nil, stepIDs(steps)},
		{"item", []string{"tests"}, []string{"go › make test", "go › make lint-tests", "go › coverage floors exist"}},
		{"job", []string{"deploy"}, []string{"deploy › make deploy-check"}},
		{"item and job", []string{"smoke", "workflows"}, []string{"go › make release-smoke", "workflows › make actionlint"}},
		{"lint item spans jobs", []string{"lint"}, []string{"go › make lint", "go › make lint-layering", "workflows › make actionlint"}},
		{"unknown", []string{"nothing"}, []string{}},
	} {
		test.Run(testCase.name, func(test *testing.T) {
			test.Parallel()
			if got := stepIDs(verify.Only(steps, testCase.names)); !slices.Equal(got, testCase.want) {
				test.Fatalf("Only(%q) = %q, want %q", testCase.names, got, testCase.want)
			}
		})
	}
}
