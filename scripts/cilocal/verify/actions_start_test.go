package verify_test

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/jayleaton/go-fast-stack/scripts/cilocal/verify"
)

// pullRollup is a real PR's statusCheckRollup (gh pr view <n> --json
// statusCheckRollup), as make pr-merge read it at the merge: CI's three jobs
// done by 10:56:39, merged at 10:56:51.
const pullRollup = `[
 {"__typename":"CheckRun","name":"Secret scan","workflowName":"CI","status":"COMPLETED","conclusion":"SUCCESS","startedAt":"2026-11-01T10:55:40Z","completedAt":"2026-11-01T10:56:24Z","detailsUrl":"https://github.com/jayleaton/go-fast-stack/actions/runs/41299593204/job/124827591767"},
 {"__typename":"CheckRun","name":"Local verification claim","workflowName":"CI","status":"COMPLETED","conclusion":"SUCCESS","startedAt":"2026-11-01T10:55:40Z","completedAt":"2026-11-01T10:56:07Z","detailsUrl":"https://github.com/jayleaton/go-fast-stack/actions/runs/41299593204/job/124827591562"},
 {"__typename":"CheckRun","name":"Local verification","workflowName":"CI","status":"COMPLETED","conclusion":"SUCCESS","startedAt":"2026-11-01T10:56:09Z","completedAt":"2026-11-01T10:56:39Z","detailsUrl":"https://github.com/jayleaton/go-fast-stack/actions/runs/41299593204/job/124827689020"},
 {"__typename":"StatusContext","context":"local-ci","state":"SUCCESS","startedAt":"2026-11-01T10:55:31Z","targetUrl":null}
]`

func realRollup(test *testing.T) []verify.Check {
	test.Helper()
	var checks []verify.Check
	if err := json.Unmarshal([]byte(pullRollup), &checks); err != nil {
		test.Fatal(err)
	}
	return checks
}

func at(test *testing.T, clock string) time.Time {
	test.Helper()
	when, err := time.Parse(time.RFC3339, "2026-11-01T"+clock+"Z")
	if err != nil {
		test.Fatal(err)
	}
	return when
}

func realPull(checks []verify.Check) verify.Pull {
	return verify.Pull{Number: 59, State: "OPEN", Base: verify.Main, Mergeable: "MERGEABLE", Checks: checks, Verification: verified()}
}

func TestARealRollupIsGreenWhenItMerged(test *testing.T) {
	test.Parallel()
	checks := verify.ClassifyChecks(realRollup(test))
	if !checks.Ran || len(checks.Failed) > 0 || len(checks.Pending) > 0 {
		test.Fatalf("ClassifyChecks = %+v, want ran and green", checks)
	}
	for _, require := range []bool{false, true} {
		verdict := verify.Decide(realPull(realRollup(test)), verify.MergeOptions{RequireActions: require, Now: at(test, "10:56:51")})
		if verdict.Action != verify.ActionMerge || verdict.Note != "GitHub's checks are green" {
			test.Fatalf("RequireActions=%v: verdict = %+v, want a merge on green checks", require, verdict)
		}
	}
}

// Between make ci-local's local-ci (10:55:31) and GitHub creating the run
// its body edit starts (10:55:37), the rollup holds local-ci alone.
func TestAHeadAttestedSecondsAgoWaitsForItsRunToStart(test *testing.T) {
	test.Parallel()
	statusOnly := realRollup(test)[3:]
	for _, require := range []bool{false, true} {
		verdict := verify.Decide(realPull(statusOnly), verify.MergeOptions{RequireActions: require, Now: at(test, "10:55:33")})
		if verdict.Action != verify.ActionSkip || verdict.Reason != "has no GitHub CI job on the head yet: GitHub starts the PR run a few seconds after make ci-local edits the body" {
			test.Fatalf("RequireActions=%v, 2 s after local-ci: verdict = %+v, want a skip until the run starts", require, verdict)
		}
	}
}

// Past the grace, no run means Actions is not running here: local-ci
// alone, or a refusal with --require-actions, as before.
func TestAHeadWithNoRunPastTheGraceIsActionsOff(test *testing.T) {
	test.Parallel()
	statusOnly := realRollup(test)[3:]
	later := at(test, "10:55:31").Add(verify.ActionsStartGrace + time.Second)
	if verdict := verify.Decide(realPull(statusOnly), verify.MergeOptions{Now: later}); verdict.Action != verify.ActionMerge ||
		verdict.Note != "no GitHub CI job ran on the head (Actions off, or blocked so GitHub never started them): merging on local-ci alone" {
		test.Fatalf("past the grace: verdict = %+v, want a merge on local-ci alone", verdict)
	}
	if verdict := verify.Decide(realPull(statusOnly), verify.MergeOptions{RequireActions: true, Now: later}); verdict.Action != verify.ActionSkip {
		test.Fatalf("past the grace with --require-actions: verdict = %+v, want a skip", verdict)
	}
}
