package verify

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// The merge decision (make pr-merge, docs/CI.md "Merging on local
// verification"): each listed pull request merges now or is reported with
// its next step. Nothing waits: a draft, unattested, failing, pending or
// conflicting PR is left alone so it never holds up the others. A head
// behind its base is refused: GitHub would squash it into a tree (the base
// plus the PR) that nobody built, which is how two green PRs broke main.

// Main is the branch pull requests land on (scripts/branches.sh).
const Main = "main"

// The checks the main ruleset requires (ci.yml).
const (
	// Gate is the check of the make ci-local attestation.
	Gate = "Local verification"
	// AlwaysRuns runs on every pull request, docs included.
	AlwaysRuns = "Secret scan"
)

// Check is one entry of a pull request's statusCheckRollup (gh pr view
// --json statusCheckRollup): a check run of a workflow job, or a commit
// status (`local-ci`).
type Check struct {
	Typename     string `json:"__typename"` // CheckRun or StatusContext
	Name         string `json:"name"`
	Context      string `json:"context"` // a StatusContext's name
	State        string `json:"state"`   // a StatusContext's result
	WorkflowName string `json:"workflowName"`
	Status       string `json:"status"`
	Conclusion   string `json:"conclusion"`
	DetailsURL   string `json:"detailsUrl"`
	StartedAt    string `json:"startedAt"`
	// NotStarted is a job GitHub failed without starting it (Actions
	// blocked by billing or a spending limit): it never ran, so it counts
	// as no run, not as a failure. Set from the job's annotations
	// (NotStartedMessage).
	NotStarted bool `json:"-"`
}

// notStarted is how GitHub's annotation on such a job begins: "The job was
// not started because recent account payments have failed or your spending
// limit needs to be increased."
const notStarted = "The job was not started"

// NotStartedMessage is whether an annotation of a failed job says GitHub
// never started it.
func NotStartedMessage(message string) bool { return strings.HasPrefix(message, notStarted) }

var (
	jobID = regexp.MustCompile(`/job/(\d+)`)
	runID = regexp.MustCompile(`/actions/runs/(\d+)/`)
)

func number(pattern *regexp.Regexp, text string) int {
	match := pattern.FindStringSubmatch(text)
	if match == nil {
		return 0
	}
	value, _ := strconv.Atoi(match[1])
	return value
}

// JobID is the check run id of a workflow job's check (its details URL ends
// in /job/<id>), or 0.
func (check Check) JobID() int { return number(jobID, check.DetailsURL) }

func (check Check) run() int { return number(runID, check.DetailsURL) }

// CheckName is what the check is called: the job's name or the status's context.
func (check Check) CheckName() string {
	switch {
	case check.Name != "":
		return check.Name
	case check.Context != "":
		return check.Context
	}
	return "unnamed check"
}

// result is the check's outcome as one word: a status's state, a finished
// job's conclusion, or PENDING.
func (check Check) result() string {
	switch {
	case check.Typename == "StatusContext":
		return check.State
	case check.Status == "COMPLETED":
		return check.Conclusion
	}
	return "PENDING"
}

// CIWorkflow is the name of ci.yml, whose jobs check a pull request.
const CIWorkflow = "CI"

// LatestChecks keeps what counts of a head's rollup: it holds every run on
// the head, so a run cancelled by a newer edit sits next to the run that
// counts. Only the newest run of each workflow is current, and within it
// only the newest attempt of each job (a re-run adds a copy).
func LatestChecks(rollup []Check) []Check {
	newest := map[string]int{}
	for _, check := range rollup {
		if check.WorkflowName != "" {
			newest[check.WorkflowName] = max(newest[check.WorkflowName], check.run())
		}
	}
	latest := []Check{}
	index := map[string]int{}
	for _, check := range rollup {
		if check.WorkflowName != "" && check.run() != newest[check.WorkflowName] {
			continue
		}
		key := check.WorkflowName + "\x00" + check.CheckName()
		seen, found := index[key]
		switch {
		case !found:
			index[key] = len(latest)
			latest = append(latest, check)
		case check.StartedAt >= latest[seen].StartedAt:
			latest[seen] = check
		}
	}
	return latest
}

var (
	passedResults = map[string]bool{"SUCCESS": true, "NEUTRAL": true, "SKIPPED": true, "success": true}
	failedResults = map[string]bool{
		"FAILURE": true, "TIMED_OUT": true, "CANCELLED": true, "ACTION_REQUIRED": true,
		"STARTUP_FAILURE": true, "ERROR": true, "failure": true, "error": true,
	}
)

// Checks is a head's rollup sorted: failed and pending check names, whether
// any workflow job ran at all, and whether the green is hollow (the gate
// passed but the secret scan was skipped).
type Checks struct {
	Failed  []string
	Pending []string
	Ran     bool
	Hollow  bool
}

// ClassifyChecks sorts the newest result of each check (LatestChecks). A
// run with a job GitHub never started (Actions blocked) never ran at all:
// its other jobs are skipped for want of their needs (ci.yml's checking job
// waits on the claim), so none of its checks count. Without the gate, a head
// with nothing failed is pending.
func ClassifyChecks(rollup []Check) Checks {
	var sorted Checks
	gate, alwaysRuns := "", ""
	latest := LatestChecks(rollup)
	blocked := map[int]bool{}
	for _, check := range latest {
		if check.NotStarted {
			blocked[check.run()] = true
		}
	}
	for _, check := range latest {
		if check.Typename != "StatusContext" && blocked[check.run()] {
			continue
		}
		name, result := check.CheckName(), check.result()
		if check.Typename != "StatusContext" {
			sorted.Ran = true
		}
		switch name {
		case Gate:
			gate = result
		case AlwaysRuns:
			alwaysRuns = result
		}
		switch {
		case failedResults[result]:
			sorted.Failed = append(sorted.Failed, name)
		case !passedResults[result]:
			sorted.Pending = append(sorted.Pending, name)
		}
	}
	if sorted.Ran && gate == "" && len(sorted.Failed) == 0 {
		sorted.Pending = append(sorted.Pending, Gate)
	}
	sorted.Hollow = gate == "SUCCESS" && alwaysRuns == "SKIPPED"
	return sorted
}

// Pull is what the merge script reads of a pull request.
type Pull struct {
	Number    int
	State     string // OPEN, CLOSED, MERGED
	Draft     bool
	Base      string
	Mergeable string // MERGEABLE, CONFLICTING, UNKNOWN
	// Behind is how many commits of the base branch the head lacks
	// (GitHub's compare): 0 when the head contains the base, so the tree
	// that was verified is exactly the tree the merge makes.
	Behind int
	Checks []Check
	Verification
}

// Merge options.
type MergeOptions struct {
	AnyBase        bool // merge a PR stacked on another branch
	RequireActions bool // refuse when no GitHub CI job ran on the head
	// Now is when the decision is made (ActionsStartGrace); zero skips the
	// grace.
	Now time.Time
}

// ActionsStartGrace is how long after make ci-local posts local-ci a head
// with no GitHub CI job still counts as not started yet rather than as
// Actions not running. make ci-local edits the PR body right after the
// status, and GitHub creates the run that edit starts a few seconds later
// (one PR: local-ci at 10:55:31, its CI run at 10:55:37): in between, the
// rollup holds local-ci alone. Read as "no job ran", that gap made make
// pr-merge merge on local-ci alone, and made make land report that Actions
// never ran, for a head whose checks then ran and passed.
const ActionsStartGrace = 2 * time.Minute

// attestedAt is when the head's local-ci status was posted, or zero.
func attestedAt(rollup []Check) time.Time {
	for _, check := range rollup {
		if check.Typename == "StatusContext" && check.Context == StatusContext {
			if at, err := time.Parse(time.RFC3339, check.StartedAt); err == nil {
				return at
			}
		}
	}
	return time.Time{}
}

// startingYet is whether a head with no GitHub CI job was attested within
// ActionsStartGrace of `now`: its PR run may not exist yet.
func startingYet(rollup []Check, now time.Time) bool {
	at := attestedAt(rollup)
	return !now.IsZero() && !at.IsZero() && now.Sub(at) < ActionsStartGrace
}

// Actions of a verdict.
const (
	ActionDone  = "done"
	ActionMerge = "merge"
	ActionSkip  = "skip"
)

// Verdict is what to do with one pull request now: done, merge, or skip
// with the reason and the next step. Never wait.
type Verdict struct {
	Action string
	Reason string
	Next   string
	Note   string // how GitHub's own checks counted, for a merge
}

func skip(reason, next string) Verdict {
	return Verdict{Action: ActionSkip, Reason: reason, Next: next, Note: ""}
}

// Decide is the merge decision for one pull request. It merges on local
// verification of the exact head (LocalVerificationProblems) and on GitHub's
// checks where they ran. When no CI job ran on the head (a repository whose
// Actions are off, or blocked so GitHub fails every job without starting
// it), `local-ci` alone decides, with a note, unless RequireActions.
func Decide(pull Pull, options MergeOptions) Verdict {
	switch {
	case pull.State == "MERGED":
		return Verdict{Action: ActionDone, Reason: "already merged", Next: "", Note: ""}
	case pull.State != "OPEN":
		return skip("is "+strings.ToLower(pull.State), "nothing; never reopen, open a new PR")
	case pull.Draft:
		return skip("is a draft", fmt.Sprintf("gh pr ready %d", pull.Number))
	case pull.Base != Main && !options.AnyBase:
		return skip("is stacked on "+pull.Base,
			fmt.Sprintf("merge its parent first (make pr-merge retargets it to %s), or gh pr edit %d --base %s", Main, pull.Number, Main))
	case pull.Mergeable == "CONFLICTING":
		return skip("conflicts with "+pull.Base, fmt.Sprintf("git rebase origin/%s, push, %s", pull.Base, NextStep))
	case pull.Behind > 0:
		// Merging it would squash a tree nobody built: the base's newer
		// commits plus this PR. Two green PRs broke main this way (one
		// removed what the other still called).
		return skip(fmt.Sprintf("is %d commit(s) behind %s: the merged tree (%s plus this PR) was never built or tested", pull.Behind, pull.Base, pull.Base),
			fmt.Sprintf("rebase onto origin/%s and re-attest: git rebase origin/%s, push, %s", pull.Base, pull.Base, NextStep))
	}
	if problems := LocalVerificationProblems(pull.Verification); len(problems) > 0 {
		return skip("is not verified locally: "+strings.Join(problems, "; "), NextStep)
	}
	checks := ClassifyChecks(pull.Checks)
	note := "GitHub's checks are green"
	switch {
	case !checks.Ran && startingYet(pull.Checks, options.Now):
		return skip("has no GitHub CI job on the head yet: GitHub starts the PR run a few seconds after make ci-local edits the body",
			"run this again in a few seconds; nothing else waits on it")
	case !checks.Ran && options.RequireActions:
		return skip("has no GitHub CI job that ran on the head (Actions off, or blocked so GitHub never started them)",
			"edit the PR body once to start CI, or drop --require-actions where Actions cannot run")
	case !checks.Ran:
		note = "no GitHub CI job ran on the head (Actions off, or blocked so GitHub never started them): merging on local-ci alone"
	case len(checks.Failed) > 0:
		return skip("has failing checks: "+strings.Join(checks.Failed, ", "),
			"fix and push, or for a lost runner gh run rerun <id> --failed once")
	case checks.Hollow:
		return skip("is green only because CI skipped every job, the secret scan included", "push a new commit and attest it to start a full PR run")
	case len(checks.Pending) > 0:
		return skip("has checks still running: "+strings.Join(checks.Pending, ", "), "run this again when they finish; nothing else waits on it")
	}
	if pull.Mergeable != "MERGEABLE" {
		return skip("mergeability is still being computed", "run this again in a minute")
	}
	return Verdict{Action: ActionMerge, Reason: "attested on " + Short(pull.Head), Next: "", Note: note}
}
