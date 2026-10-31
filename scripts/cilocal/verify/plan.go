// Package verify holds the rules of local verification (docs/CI.md, "Local
// verification"): what `make ci-local` selects from a branch's changed files,
// the steps it runs, the input hashes that let a later head reuse a step, the
// PR body section it fills and the checks that refuse a pull request whose
// section or `local-ci` status does not hold for its exact head. It is pure:
// git, gh, docker and the steps themselves run in scripts/cilocal, which
// passes their results in. Its tests are a test boundary (AGENTS.md "Test
// boundaries").
package verify

import (
	"path"
	"regexp"
	"strings"
)

// TooManyFiles is where a diff stops being trusted to select: GitHub lists at
// most 3,000 files of a pull request, so a change this large runs everything.
const TooManyFiles = 3000

// Areas is what a change selects. Each step of the plan belongs to one.
type Areas struct {
	Everything bool // a file that decides how every check runs, an empty diff or too many files
	Go         bool // the root module: build, lint, generated code, tests, probes, release smoke
	Migrations bool // a goose migration: the migration order
	Deploy     bool // the deploy/ module and what make deploy-vars reads
	Mobile     bool // apps/mobile-app and the route table its test reads
	Workflows  bool // GitHub workflows and actions: actionlint
}

// Names lists the selected areas, for the report.
func (areas Areas) Names() []string {
	names := []string{}
	for _, area := range []struct {
		on   bool
		name string
	}{
		{areas.Everything, "everything"}, {areas.Go, "go"}, {areas.Migrations, "migrations"},
		{areas.Deploy, "deploy"}, {areas.Mobile, "mobile"}, {areas.Workflows, "workflows"},
	} {
		if area.on {
			names = append(names, area.name)
		}
	}
	return names
}

// everythingFiles decide how every step runs: this tool and the make targets
// and toolchain pins its steps call. A change to one selects every step
// except the mobile app's (which runs only when the app or its route table
// changed).
var everythingFiles = []string{"scripts/cilocal/", "Makefile", "mise.toml"}

// notGo are the paths the root module's checks never read (the old CI's
// paths filter): docs, root Markdown, the mobile app, the deploy/ module
// (its own checks), GitHub's configuration and the secret scan's.
var notGo = []string{"docs/", "apps/mobile-app/", "deploy/", ".github/", ".gitignore", ".dockerignore", ".gitleaks.toml"}

var migrationFile = regexp.MustCompile(`(^|/)migrations/[^/]*\.sql$`)

func hasPrefix(file string, prefixes []string) bool {
	for _, prefix := range prefixes {
		if file == prefix || (strings.HasSuffix(prefix, "/") && strings.HasPrefix(file, prefix)) {
			return true
		}
	}
	return false
}

// Classify is what a change to `file` alone selects.
func Classify(file string) Areas {
	if hasPrefix(file, everythingFiles) {
		return Areas{Everything: true, Go: true, Migrations: true, Deploy: true, Workflows: true}
	}
	rootMarkdown := !strings.Contains(file, "/") && path.Ext(file) == ".md"
	return Areas{
		Go:         !rootMarkdown && !hasPrefix(file, notGo),
		Migrations: migrationFile.MatchString(file) || hasPrefix(file, []string{"scripts/check-migration-order.sh", "scripts/branches.sh"}),
		// make deploy-check builds namelint and lints with the root config
		// and the tools pin; deploy-vars reads .env.example and the Makefile.
		Deploy:    hasPrefix(file, []string{"deploy/", ".env.example", ".golangci.yml", "tools/", "scripts/namelint/"}),
		Mobile:    hasPrefix(file, []string{"apps/mobile-app/", "apps/web/internal/config/routes.go"}),
		Workflows: hasPrefix(file, []string{".github/workflows/", ".github/actions/"}),
	}
}

// SelectAreas is what a branch's changed files select together. An empty
// diff or one of TooManyFiles or more selects everything.
func SelectAreas(files []string) Areas {
	if len(files) == 0 || len(files) >= TooManyFiles {
		return Classify("Makefile")
	}
	var areas Areas
	for _, file := range files {
		one := Classify(file)
		areas.Everything = areas.Everything || one.Everything
		areas.Go = areas.Go || one.Go
		areas.Migrations = areas.Migrations || one.Migrations
		areas.Deploy = areas.Deploy || one.Deploy
		areas.Mobile = areas.Mobile || one.Mobile
		areas.Workflows = areas.Workflows || one.Workflows
	}
	return areas
}

// Affected is scripts/affected.sh's output: what the root module's checks
// cover for the change.
type Affected struct {
	Full     bool
	Packages []string // import paths, or ./... on a full run
	Dirs     []string // the same as ./relative dirs
	SQLC     bool
	Templ    bool
	Generate []string
	Smoke    bool
}

// ParseAffected reads affected.sh's key=value lines.
func ParseAffected(output string) Affected {
	var affected Affected
	for line := range strings.SplitSeq(output, "\n") {
		key, value, found := strings.Cut(strings.TrimSpace(line), "=")
		if !found {
			continue
		}
		switch key {
		case "full":
			affected.Full = value == "true"
		case "packages":
			affected.Packages = strings.Fields(value)
		case "dirs":
			affected.Dirs = strings.Fields(value)
		case "sqlc":
			affected.SQLC = value == "true"
		case "templ":
			affected.Templ = value == "true"
		case "generate":
			affected.Generate = strings.Fields(value)
		case "smoke":
			affected.Smoke = value == "true"
		}
	}
	return affected
}

// Step is one command of the plan. Its item is the checklist line it fills;
// its job and name are its id in the report and the ledger.
type Step struct {
	Item  string            `json:"item"`
	Job   string            `json:"job"`
	Name  string            `json:"name"`
	Run   []string          `json:"run"`
	Env   map[string]string `json:"env,omitempty"`
	Local bool              `json:"local,omitempty"` // runs on this machine even when the others run in a Davit box
}

// ID is the step's name in the ledger.
func (step Step) ID() string { return step.Job + " › " + step.Name }

// Jobs, which are also the input classes of reuse (InputOf).
const (
	JobSecrets    = "secrets"
	JobGo         = "go"
	JobMigrations = "migrations"
	JobDeploy     = "deploy"
	JobMobile     = "mobile"
	JobWorkflows  = "workflows"
)

// Items is the PR body's checklist, in order (.github/pull_request_template.md
// lists the same). Each is ticked only by make ci-local: passed, or not
// selected when the change selects none of its steps.
var Items = []string{"secrets", "build", "lint", "generated", "tests", "migrations", "smoke", "deploy", "mobile"}

// mobileChecks are the mobile app's checks (its old CI job): install from the
// lockfile, type-check, lint, the Expo pin check and the tests.
const mobileChecks = "cd apps/mobile-app && pnpm install --frozen-lockfile && pnpm exec tsc --noEmit && pnpm exec expo lint && pnpm exec expo install --check && pnpm test"

// lintTests runs the test probes on make test's coverage profile, or runs the
// tests itself when that step was reused and left no profile.
const lintTests = "if [ -f .cache/coverage.out ]; then make lint-tests PROFILE=.cache/coverage.out; else make lint-tests; fi"

// Plan is every step a selection runs, in order: the secret scan always,
// then each selected area's steps. The steps use the repo's own make targets,
// scoped to the affected packages. `base` is the commit the change is taken
// against: the secret scan reads the files changed since it.
func Plan(areas Areas, affected Affected, base string) []Step {
	steps := []Step{{Item: "secrets", Job: JobSecrets, Name: "gitleaks", Run: []string{"go", "run", "./scripts/cilocal", "secret-scan", "--base", base}, Local: true}}
	if areas.Migrations {
		steps = append(steps, Step{Item: "migrations", Job: JobMigrations, Name: "migration order", Run: []string{"./scripts/check-migration-order.sh"}, Local: true})
	}
	if areas.Go {
		packages := strings.Join(affected.Packages, " ")
		if len(affected.Packages) > 0 {
			steps = append(steps,
				Step{Item: "build", Job: JobGo, Name: "go build", Run: append([]string{"go", "build"}, affected.Packages...)},
				Step{Item: "lint", Job: JobGo, Name: "make lint", Run: []string{"make", "lint", "LINT_DIRS=" + strings.Join(affected.Dirs, " ")}},
			)
		}
		if affected.SQLC || affected.Templ || len(affected.Generate) > 0 {
			steps = append(steps, Step{Item: "generated", Job: JobGo, Name: "make check-generated", Run: []string{"make", "check-generated"}})
		}
		if len(affected.Packages) > 0 {
			steps = append(steps, Step{Item: "tests", Job: JobGo, Name: "make test", Run: []string{"make", "test", "TEST_PKGS=" + packages}})
		}
		if affected.Full {
			// The probes prove the lint and test rules still bite; their
			// inputs always make a full run (affected.sh).
			steps = append(steps,
				Step{Item: "lint", Job: JobGo, Name: "make lint-layering", Run: []string{"make", "lint-layering"}},
				Step{Item: "tests", Job: JobGo, Name: "make lint-tests", Run: []string{"bash", "-c", lintTests}},
				Step{Item: "tests", Job: JobGo, Name: "coverage floors exist", Run: []string{"./scripts/check-coverage.sh", "--exist"}},
			)
		}
		if affected.Smoke {
			steps = append(steps, Step{Item: "smoke", Job: JobGo, Name: "make release-smoke", Run: []string{"make", "release-smoke"}})
		}
	}
	if areas.Workflows {
		steps = append(steps, Step{Item: "lint", Job: JobWorkflows, Name: "make actionlint", Run: []string{"make", "actionlint"}})
	}
	if areas.Deploy {
		steps = append(steps, Step{Item: "deploy", Job: JobDeploy, Name: "make deploy-check", Run: []string{"make", "deploy-check"}})
	}
	if areas.Mobile {
		steps = append(steps, Step{Item: "mobile", Job: JobMobile, Name: "mobile checks", Run: []string{"bash", "-c", mobileChecks}})
	}
	return steps
}

// Only keeps the steps whose item or job `names` lists (make ci-local
// ONLY=tests,deploy); every step when it lists none.
func Only(steps []Step, names []string) []Step {
	if len(names) == 0 {
		return steps
	}
	kept := []Step{}
	for _, step := range steps {
		for _, name := range names {
			if name == step.Item || name == step.Job {
				kept = append(kept, step)
				break
			}
		}
	}
	return kept
}
