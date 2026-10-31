package verify_test

import (
	"slices"
	"strings"
	"testing"

	"github.com/jayleaton/go-fast-stack/scripts/cilocal/verify"
)

func ciCheck(name string, run, status, conclusion string) verify.Check {
	return verify.Check{
		Name: name, WorkflowName: verify.CIWorkflow, Status: status, Conclusion: conclusion,
		DetailsURL: "https://github.com/jayleaton/go-fast-stack/actions/runs/" + run + "/job/1",
	}
}

func TestLatestCIChecksKeepsTheNewestRun(test *testing.T) {
	test.Parallel()
	rollup := []verify.Check{
		ciCheck("build", "100", "COMPLETED", "CANCELLED"),
		ciCheck("build", "205", "COMPLETED", "SUCCESS"),
		ciCheck("lint", "205", "IN_PROGRESS", ""),
		ciCheck("lint", "99", "COMPLETED", "FAILURE"),
		{Name: "deploy", WorkflowName: "Deploy", Status: "COMPLETED", DetailsURL: "https://github.com/o/r/actions/runs/999/job/1"},
		{Name: "no url", WorkflowName: verify.CIWorkflow, Status: "COMPLETED"},
	}
	names := []string{}
	for _, check := range verify.LatestCIChecks(rollup) {
		names = append(names, check.Name+"@"+strings.Split(strings.Split(check.DetailsURL, "/runs/")[1], "/")[0])
	}
	if !slices.Equal(names, []string{"build@205", "lint@205"}) {
		test.Fatalf("LatestCIChecks = %q", names)
	}
	if got := verify.LatestCIChecks(nil); len(got) != 0 {
		test.Fatalf("LatestCIChecks(nil) = %+v", got)
	}
}

func TestMergeProblems(test *testing.T) {
	test.Parallel()
	ready := verify.Pull{State: "OPEN", Mergeable: "MERGEABLE", Verification: verified()}
	passed := []verify.Check{ciCheck("build", "205", "COMPLETED", "SUCCESS"), ciCheck("lint", "205", "COMPLETED", "SUCCESS")}
	for _, testCase := range []struct {
		name           string
		pull           func(verify.Pull) verify.Pull
		requireActions bool
		want           []string // empty when it merges
		note           string
	}{
		{"no CI checks merges on local-ci", func(pull verify.Pull) verify.Pull { return pull }, false, nil, "merging on local-ci alone"},
		{"no CI checks but actions required", func(pull verify.Pull) verify.Pull { return pull }, true, []string{"no GitHub CI run on the head"}, ""},
		{"CI checks passed", func(pull verify.Pull) verify.Pull { pull.Checks = passed; return pull }, true, nil, "2 GitHub CI checks on the head"},
		{"older failed run ignored", func(pull verify.Pull) verify.Pull {
			pull.Checks = append(slices.Clone(passed), ciCheck("build", "100", "COMPLETED", "FAILURE"))
			return pull
		}, false, nil, "2 GitHub CI checks"},
		{"CI in progress", func(pull verify.Pull) verify.Pull {
			pull.Checks = []verify.Check{ciCheck("lint", "205", "IN_PROGRESS", "")}
			return pull
		}, false, []string{"`lint` is still IN_PROGRESS"}, "1 GitHub CI checks"},
		{"CI failed", func(pull verify.Pull) verify.Pull {
			pull.Checks = []verify.Check{ciCheck("test", "205", "COMPLETED", "FAILURE")}
			return pull
		}, false, []string{"`test` is FAILURE: https://github.com/jayleaton/go-fast-stack/actions/runs/205/job/1"}, "1 GitHub CI checks"},
		{"conflicting", func(pull verify.Pull) verify.Pull { pull.Mergeable = "CONFLICTING"; return pull }, false, []string{"it conflicts with its base"}, "local-ci alone"},
		{"unknown mergeable", func(pull verify.Pull) verify.Pull { pull.Mergeable = "UNKNOWN"; return pull }, false, []string{"GitHub has not decided"}, "local-ci alone"},
		{"draft", func(pull verify.Pull) verify.Pull { pull.Draft = true; return pull }, false, []string{"the pull request is a draft"}, "local-ci alone"},
		{"closed", func(pull verify.Pull) verify.Pull { pull.State = "CLOSED"; pull.Draft = true; return pull }, false, []string{"the pull request is CLOSED"}, ""},
		{"merged", func(pull verify.Pull) verify.Pull { pull.State = "MERGED"; return pull }, false, []string{"the pull request is MERGED"}, ""},
		{"unverified head", func(pull verify.Pull) verify.Pull { pull.Status = nil; return pull }, false, []string{"no local-ci status"}, "local-ci alone"},
	} {
		test.Run(testCase.name, func(test *testing.T) {
			test.Parallel()
			problems, note := verify.MergeProblems(testCase.pull(ready), testCase.requireActions)
			if len(testCase.want) == 0 {
				requireNone(test, problems)
			}
			for _, want := range testCase.want {
				requireMention(test, problems, want)
			}
			if len(testCase.want) > 0 && len(problems) != len(testCase.want) {
				test.Fatalf("problems = %q, want only %q", problems, testCase.want)
			}
			if (testCase.note == "" && note != "") || !strings.Contains(note, testCase.note) {
				test.Fatalf("note = %q, want %q", note, testCase.note)
			}
		})
	}
}
