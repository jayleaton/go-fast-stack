package verify_test

import (
	"os"
	"path/filepath"
	"slices"
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
