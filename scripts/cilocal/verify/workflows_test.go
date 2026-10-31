package verify_test

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/jayleaton/go-fast-stack/scripts/cilocal/verify"
)

func TestPushesToMain(test *testing.T) {
	test.Parallel()
	for _, testCase := range []struct {
		name, text string
		want       bool
	}{
		{"push to main", "name: Deploy\non:\n  push:\n    branches: [main] # the deployed branch\n  workflow_dispatch:\n", true},
		{"dash list", "on:\n  push:\n    branches:\n      - 'release'\n      - \"main\"\n", true},
		{"glob", "on:\n  push:\n    branches: ['**']\n", true},
		{"every branch", "on:\n  push:\n  pull_request:\n", true},
		{"inline push", "on: push\n", true},
		{"inline list", "'on': [pull_request, push]\n", true},
		{"other branches", "on:\n  push:\n    branches: ['bench/**']\n", false},
		{"tags only", "on:\n  push:\n    tags: ['v*']\n", false},
		{"pull requests only", "# push: [main]\n'on':\n  pull_request:\n    types: [edited]\njobs:\n  push:\n    runs-on: x\n", false},
		{"workflow_run", "on:\n  workflow_run:\n    workflows: [Deploy staging]\n", false},
		{"no triggers", "name: x\n", false},
		{"empty on", "on:\nname: x\n", false},
	} {
		test.Run(testCase.name, func(test *testing.T) {
			test.Parallel()
			if got := verify.PushesToMain(testCase.text); got != testCase.want {
				test.Fatalf("PushesToMain(%q) = %v, want %v", testCase.text, got, testCase.want)
			}
		})
	}
}

func TestReportGaps(test *testing.T) {
	test.Parallel()
	report := "name: Post-merge report\non:\n  workflow_run:\n    workflows:\n      - Deploy staging\n    types: [completed]\n"
	if got := verify.ReportedWorkflows(report); !slices.Equal(got, []string{"Deploy staging"}) {
		test.Fatalf("ReportedWorkflows = %q", got)
	}
	if got := verify.ReportedWorkflows("on:\n  workflow_run:\n    workflows: [A, 'B']\n"); !slices.Equal(got, []string{"A", "B"}) {
		test.Fatalf("ReportedWorkflows of an inline list = %q", got)
	}
	if got := verify.ReportedWorkflows("on: push\n"); got != nil {
		test.Fatalf("ReportedWorkflows without workflow_run = %q", got)
	}
	if got := verify.ReportedWorkflows("on:\n  workflow_run:\n"); got != nil {
		test.Fatalf("ReportedWorkflows of an empty workflow_run = %q", got)
	}
	if got := verify.WorkflowName("on: push\n"); got != "" {
		test.Fatalf("WorkflowName without a name = %q", got)
	}
	workflows := map[string]string{
		"deploy-staging.yml": "name: Deploy staging\non:\n  push:\n    branches: [main]\n",
		"box.yml":            "name: 'box-ami'\non:\n  push:\n    branches: [main]\n",
		"ci.yml":             "name: CI\non:\n  pull_request:\n",
	}
	if got := verify.ReportGaps(workflows, report); !slices.Equal(got, []string{"box-ami"}) {
		test.Fatalf("ReportGaps = %q, want [box-ami]", got)
	}
}

// Every workflow of this repository that runs on a push to main is in the
// post-merge report's list, so a failure after merge is reported on the PR.
func TestThePostMergeReportListsEveryPushToMainWorkflow(test *testing.T) {
	test.Parallel()
	dir := filepath.Join("..", "..", "..", ".github", "workflows")
	files, err := filepath.Glob(filepath.Join(dir, "*.yml"))
	if err != nil || len(files) == 0 {
		test.Fatalf("no workflows in %s: %v", dir, err)
	}
	workflows := map[string]string{}
	for _, file := range files {
		text, err := os.ReadFile(file)
		if err != nil {
			test.Fatal(err)
		}
		workflows[filepath.Base(file)] = string(text)
	}
	report, found := workflows["post-merge-report.yml"]
	if !found {
		test.Fatal("no .github/workflows/post-merge-report.yml")
	}
	if !verify.PushesToMain(workflows["deploy-staging.yml"]) {
		test.Fatal("deploy-staging.yml does not read as a push to main")
	}
	if gaps := verify.ReportGaps(workflows, report); len(gaps) > 0 {
		test.Fatalf("post-merge-report.yml's workflow_run.workflows misses %q, which run on a push to main", gaps)
	}
}

// ci.yml as #134 had it at bf0b5d3 (Randy's review): pull_request runs the
// pull request's own workflow and code, so a fork adding
// `func init() { os.Exit(0) }` to scripts/cilocal passed both checks, and
// the secret scan used the fork's .gitleaks.toml.
const untrustedCI = `name: CI
on:
  pull_request:
    types: [edited]
jobs:
  secrets:
    name: Secret scan
    runs-on: ${{ fromJSON(vars.CI_RUNNER || '"ubuntu-latest"') }}
    steps:
      - uses: actions/checkout@v7
        with:
          ref: ${{ github.event.pull_request.head.sha }}
      - name: No secrets in the changed files
        env:
          PR: ${{ github.event.pull_request.number }}
        run: go run ./scripts/cilocal secret-scan --pr "$PR"
  local:
    name: Local verification
    steps:
      - uses: actions/checkout@v7
        with:
          repository: ${{ github.event.pull_request.head.repo.full_name }}
      - name: make ci-local passed on this head
        run: |
          echo "${{ github.event.pull_request.title }}"
          go run ./scripts/cilocal check
`

func TestTrustProblems(test *testing.T) {
	test.Parallel()
	problems := verify.TrustProblems(untrustedCI)
	for _, want := range []string{
		"it runs on [pull_request], not pull_request_target alone",
		"a checkout names a ref or repository",
		"puts an expression in its script",
		"does not read the head as data (--head)",
		"is not told the head's repository (HEAD_REPO)",
	} {
		requireMention(test, problems, want)
	}
	if got := len(problems); got != 6 { // both checkouts are named
		test.Errorf("TrustProblems = %d problems, want 6: %q", got, problems)
	}
	if got := verify.TrustProblems("on: [pull_request_target, push]\n"); len(got) != 1 {
		test.Errorf("TrustProblems of an inline trigger list = %q", got)
	}
	requireNone(test, verify.TrustProblems("on: pull_request_target\n"))
	requireMention(test, verify.TrustProblems("name: no triggers\n"), "it runs on [], not pull_request_target alone")
}

// The required checks run only the base's code: a branch or fork that
// rewrites scripts/cilocal, .gitleaks.toml or ci.yml itself changes nothing
// the Secret scan and Local verification jobs do, and a fork's head fails.
func TestCIRunsOnlyTrustedCode(test *testing.T) {
	test.Parallel()
	text, err := os.ReadFile(filepath.Join("..", "..", "..", ".github", "workflows", "ci.yml"))
	if err != nil {
		test.Fatal(err)
	}
	requireNone(test, verify.TrustProblems(string(text)))
	for _, want := range []string{"cilocal secret-scan", "cilocal check --claim", "cilocal check\n"} {
		if !strings.Contains(string(text), want) {
			test.Errorf("ci.yml no longer runs %q", want)
		}
	}
}
