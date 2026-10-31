package verify_test

import (
	"slices"
	"strings"
	"testing"

	"github.com/jayleaton/go-fast-stack/scripts/cilocal/verify"
)

func ciCheck(name, run, status, conclusion string) verify.Check {
	return verify.Check{
		Typename: "CheckRun", Name: name, WorkflowName: verify.CIWorkflow, Status: status, Conclusion: conclusion,
		DetailsURL: "https://github.com/jayleaton/go-fast-stack/actions/runs/" + run + "/job/1", StartedAt: "2026-10-31T10:00:00Z",
	}
}

func notStarted(name string) verify.Check {
	check := ciCheck(name, "205", "COMPLETED", "FAILURE")
	check.NotStarted = true
	return check
}

// green is a PR run where every job passed.
func green() []verify.Check {
	return []verify.Check{
		ciCheck(verify.AlwaysRuns, "205", "COMPLETED", "SUCCESS"),
		ciCheck("Local verification claim", "205", "COMPLETED", "SUCCESS"),
		ciCheck(verify.Gate, "205", "COMPLETED", "SUCCESS"),
		{Typename: "StatusContext", Context: verify.StatusContext, State: "SUCCESS"},
	}
}

func TestNotStartedReadsGitHubsAnnotationAndTheJobID(test *testing.T) {
	test.Parallel()
	if !verify.NotStartedMessage("The job was not started because recent account payments have failed or your spending limit needs to be increased. Please check the 'Billing & plans' section in your settings") {
		test.Fatal("the billing annotation is not recognised")
	}
	if verify.NotStartedMessage("Process completed with exit code 1.") {
		test.Fatal("a job's own failure counts as not started")
	}
	if got := ciCheck("x", "205", "COMPLETED", "FAILURE").JobID(); got != 1 {
		test.Fatalf("JobID = %d, want 1", got)
	}
	if got := (verify.Check{DetailsURL: "https://github.com/o/r/pull/1#local-verification"}).JobID(); got != 0 {
		test.Fatalf("JobID without a job = %d, want 0", got)
	}
	if got := (verify.Check{}).CheckName(); got != "unnamed check" {
		test.Fatalf("CheckName of an empty check = %q", got)
	}
}

func TestLatestChecksKeepsTheNewestRunAndAttempt(test *testing.T) {
	test.Parallel()
	rerun := ciCheck("lint", "205", "COMPLETED", "SUCCESS")
	rerun.StartedAt = "2026-10-31T11:00:00Z"
	rollup := []verify.Check{
		ciCheck("build", "100", "COMPLETED", "CANCELLED"),
		ciCheck("build", "205", "COMPLETED", "SUCCESS"),
		ciCheck("lint", "205", "COMPLETED", "FAILURE"),
		rerun,
		ciCheck("lint", "99", "COMPLETED", "FAILURE"),
		{Typename: "CheckRun", Name: "deploy", WorkflowName: "Deploy", Status: "COMPLETED", DetailsURL: "https://github.com/o/r/actions/runs/999/job/1"},
		{Typename: "StatusContext", Context: verify.StatusContext, State: "SUCCESS"},
	}
	names := []string{}
	for _, check := range verify.LatestChecks(rollup) {
		names = append(names, check.CheckName()+":"+check.Conclusion+check.State)
	}
	if !slices.Equal(names, []string{"build:SUCCESS", "lint:SUCCESS", "deploy:", "local-ci:SUCCESS"}) {
		test.Fatalf("LatestChecks = %q", names)
	}
	if got := verify.LatestChecks(nil); len(got) != 0 {
		test.Fatalf("LatestChecks(nil) = %+v", got)
	}
}

func TestDecide(test *testing.T) {
	test.Parallel()
	ready := verify.Pull{Number: 12, State: "OPEN", Base: verify.Main, Mergeable: "MERGEABLE", Checks: green(), Verification: verified()}
	unattested := verified()
	unattested.Status = nil
	for _, testCase := range []struct {
		name    string
		pull    func(verify.Pull) verify.Pull
		options verify.MergeOptions
		action  string
		reason  string // or the note, for a merge
		next    string
	}{
		{"green", func(pull verify.Pull) verify.Pull { return pull }, verify.MergeOptions{}, verify.ActionMerge, "GitHub's checks are green", ""},
		{"merged", func(pull verify.Pull) verify.Pull { pull.State = "MERGED"; return pull }, verify.MergeOptions{}, verify.ActionDone, "already merged", ""},
		{"closed", func(pull verify.Pull) verify.Pull { pull.State = "CLOSED"; return pull }, verify.MergeOptions{}, verify.ActionSkip, "is closed", "never reopen"},
		{"draft", func(pull verify.Pull) verify.Pull { pull.Draft = true; return pull }, verify.MergeOptions{}, verify.ActionSkip, "is a draft", "gh pr ready 12"},
		{"stacked", func(pull verify.Pull) verify.Pull { pull.Base = "feat/a"; return pull }, verify.MergeOptions{}, verify.ActionSkip, "is stacked on feat/a", "gh pr edit 12 --base main"},
		{"stacked, any base", func(pull verify.Pull) verify.Pull { pull.Base = "feat/a"; return pull }, verify.MergeOptions{AnyBase: true}, verify.ActionMerge, "green", ""},
		{"conflicting", func(pull verify.Pull) verify.Pull { pull.Mergeable = "CONFLICTING"; return pull }, verify.MergeOptions{}, verify.ActionSkip, "conflicts with main", "git rebase origin/main"},
		{"behind its base: the merged tree was never built", func(pull verify.Pull) verify.Pull { pull.Behind = 1; return pull }, verify.MergeOptions{}, verify.ActionSkip, "1 commit(s) behind main", "rebase onto origin/main and re-attest"},
		{"unattested", func(pull verify.Pull) verify.Pull { pull.Verification = unattested; return pull }, verify.MergeOptions{}, verify.ActionSkip, "is not verified locally: no local-ci status", "make ci-local RAN="},
		{"failed check", func(pull verify.Pull) verify.Pull {
			pull.Checks = append(green(), ciCheck(verify.AlwaysRuns, "206", "COMPLETED", "FAILURE"))
			return pull
		}, verify.MergeOptions{}, verify.ActionSkip, "has failing checks: Secret scan", "gh run rerun"},
		{"hollow", func(pull verify.Pull) verify.Pull {
			pull.Checks = []verify.Check{ciCheck(verify.AlwaysRuns, "205", "COMPLETED", "SKIPPED"), ciCheck(verify.Gate, "205", "COMPLETED", "SUCCESS")}
			return pull
		}, verify.MergeOptions{}, verify.ActionSkip, "skipped every job", "push a new commit"},
		{"running", func(pull verify.Pull) verify.Pull {
			pull.Checks = []verify.Check{ciCheck(verify.AlwaysRuns, "205", "IN_PROGRESS", ""), ciCheck(verify.Gate, "205", "COMPLETED", "SUCCESS")}
			return pull
		}, verify.MergeOptions{}, verify.ActionSkip, "still running: Secret scan", "run this again"},
		{"awaiting: no gate yet", func(pull verify.Pull) verify.Pull {
			pull.Checks = []verify.Check{ciCheck(verify.AlwaysRuns, "205", "COMPLETED", "SUCCESS"), ciCheck("Awaiting make ci-local", "205", "COMPLETED", "SUCCESS")}
			return pull
		}, verify.MergeOptions{}, verify.ActionSkip, "still running: Local verification", ""},
		{"unknown mergeable", func(pull verify.Pull) verify.Pull { pull.Mergeable = "UNKNOWN"; return pull }, verify.MergeOptions{}, verify.ActionSkip, "still being computed", "in a minute"},
		{"no CI run merges on local-ci", func(pull verify.Pull) verify.Pull {
			pull.Checks = []verify.Check{{Typename: "StatusContext", Context: verify.StatusContext, State: "SUCCESS"}}
			return pull
		}, verify.MergeOptions{}, verify.ActionMerge, "merging on local-ci alone", ""},
		{"no CI run, actions required", func(pull verify.Pull) verify.Pull { pull.Checks = nil; return pull }, verify.MergeOptions{RequireActions: true}, verify.ActionSkip, "no GitHub CI job that ran", "--require-actions"},
		// Billing-blocked Actions fail every job in seconds without starting
		// it. Those jobs never ran, so local-ci decides.
		{"jobs GitHub never started merge on local-ci", func(pull verify.Pull) verify.Pull {
			pull.Checks = []verify.Check{notStarted(verify.AlwaysRuns), notStarted("Local verification claim")}
			return pull
		}, verify.MergeOptions{}, verify.ActionMerge, "merging on local-ci alone", ""},
		{"jobs never started, actions required", func(pull verify.Pull) verify.Pull {
			pull.Checks = []verify.Check{notStarted(verify.AlwaysRuns)}
			return pull
		}, verify.MergeOptions{RequireActions: true}, verify.ActionSkip, "no GitHub CI job that ran", ""},
		// sparkrack #17: the claim and secret scan never started, so the
		// checking job was skipped under its unevaluated name; the gate is
		// missing because the whole run never ran, not because it is pending.
		{"a blocked run's skipped checking job does not hold the merge", func(pull verify.Pull) verify.Pull {
			pull.Checks = []verify.Check{
				notStarted(verify.AlwaysRuns), notStarted("Local verification claim"),
				ciCheck("${{ needs.claim.outputs.claimed == 'true' && 'Local verification' || 'Awaiting make ci-local' }}", "205", "COMPLETED", "SKIPPED"),
				{Typename: "StatusContext", Context: verify.StatusContext, State: "SUCCESS"},
			}
			return pull
		}, verify.MergeOptions{}, verify.ActionMerge, "merging on local-ci alone", ""},
		{"a newer run that ran and failed refuses after a blocked one", func(pull verify.Pull) verify.Pull {
			pull.Checks = []verify.Check{notStarted(verify.AlwaysRuns), ciCheck(verify.Gate, "206", "COMPLETED", "FAILURE")}
			return pull
		}, verify.MergeOptions{}, verify.ActionSkip, "has failing checks: Local verification", ""},
	} {
		test.Run(testCase.name, func(test *testing.T) {
			test.Parallel()
			verdict := verify.Decide(testCase.pull(ready), testCase.options)
			said := verdict.Reason
			if verdict.Action == verify.ActionMerge {
				said = verdict.Note
			}
			if verdict.Action != testCase.action || !strings.Contains(said, testCase.reason) || !strings.Contains(verdict.Next, testCase.next) {
				test.Fatalf("Decide = %+v, want %s %q next %q", verdict, testCase.action, testCase.reason, testCase.next)
			}
		})
	}
}
