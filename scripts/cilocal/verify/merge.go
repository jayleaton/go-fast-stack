package verify

import (
	"fmt"
	"regexp"
	"strconv"
)

// Check is one entry of a pull request's statusCheckRollup (gh pr view
// --json statusCheckRollup): a check run of a workflow job.
type Check struct {
	Name         string `json:"name"`
	WorkflowName string `json:"workflowName"`
	Status       string `json:"status"`
	Conclusion   string `json:"conclusion"`
	DetailsURL   string `json:"detailsUrl"`
}

// CIWorkflow is the name of ci.yml, whose jobs check a pull request.
const CIWorkflow = "CI"

var runID = regexp.MustCompile(`/actions/runs/(\d+)/`)

func (check Check) run() int {
	match := runID.FindStringSubmatch(check.DetailsURL)
	if match == nil {
		return 0
	}
	run, _ := strconv.Atoi(match[1])
	return run
}

// LatestCIChecks keeps the checks of the newest CI run on the head: a run
// cancelled by a newer edit sits next to the run that counts.
func LatestCIChecks(rollup []Check) []Check {
	newest := 0
	for _, check := range rollup {
		if check.WorkflowName == CIWorkflow {
			newest = max(newest, check.run())
		}
	}
	latest := []Check{}
	for _, check := range rollup {
		if check.WorkflowName == CIWorkflow && check.run() == newest {
			latest = append(latest, check)
		}
	}
	return latest
}

// Pull is what the merge script reads of a pull request.
type Pull struct {
	State     string // OPEN, CLOSED, MERGED
	Draft     bool
	Mergeable string // MERGEABLE, CONFLICTING, UNKNOWN
	Checks    []Check
	Verification
}

// MergeProblems is why the merge script refuses a pull request, and a note
// on how GitHub's own checks counted. It merges on local verification of the
// exact head (Verification.Problems). GitHub's CI jobs count when they ran:
// the newest run's jobs must all have passed. When none ran on the head (a
// repository whose Actions are off or blocked), `local-ci` alone decides,
// unless `requireActions`.
func MergeProblems(pull Pull, requireActions bool) ([]string, string) {
	problems := []string{}
	switch {
	case pull.State != "OPEN":
		return []string{"the pull request is " + pull.State}, ""
	case pull.Draft:
		problems = append(problems, "the pull request is a draft: mark it ready only to merge")
	}
	problems = append(problems, pull.Problems()...)
	switch pull.Mergeable {
	case "CONFLICTING":
		problems = append(problems, "it conflicts with its base: rebase, push and run make ci-local again (unchanged steps are reused)")
	case "MERGEABLE":
	default:
		problems = append(problems, "GitHub has not decided whether it merges cleanly yet: try again in a minute")
	}
	checks := LatestCIChecks(pull.Checks)
	if len(checks) == 0 {
		if requireActions {
			problems = append(problems, "no GitHub CI run on the head (it starts when make ci-local fills the body)")
			return problems, ""
		}
		return problems, "no GitHub CI run on the head (Actions off or blocked?): merging on local-ci alone"
	}
	for _, check := range checks {
		switch {
		case check.Status != "COMPLETED":
			problems = append(problems, fmt.Sprintf("`%s` is still %s", check.Name, check.Status))
		case check.Conclusion != "SUCCESS":
			problems = append(problems, fmt.Sprintf("`%s` is %s: %s", check.Name, check.Conclusion, check.DetailsURL))
		}
	}
	return problems, fmt.Sprintf("%d GitHub CI checks on the head", len(checks))
}
