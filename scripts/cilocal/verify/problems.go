package verify

import (
	"fmt"
	"regexp"
)

// Status is a commit status as GitHub returns it (`commits/<sha>/statuses`
// lists every one ever posted, newest first).
type Status struct {
	Context     string `json:"context"`
	State       string `json:"state"`
	Description string `json:"description"`
	CreatedAt   string `json:"created_at"`
}

var (
	sectionHead   = regexp.MustCompile("(?m)^Head `([0-9a-f]{40})`")
	checklistLine = regexp.MustCompile(`(?m)^- \[([ xX])\] \*\*([\w-]+)\*\*`)
	bodyDigest    = regexp.MustCompile(`body ([0-9a-f]{12})`)
	ledgerDigest  = regexp.MustCompile(`ledger ([0-9a-f]{12})`)
)

// FailedBefore is the first failed `local-ci` among a commit's statuses, or
// nil. A head that failed is never attested again: a failure is resolved by
// a new commit (the fix, or a rebase on a fix merged off main), never by
// running until green.
func FailedBefore(statuses []Status) *Status {
	for index := range statuses {
		status := statuses[index]
		if status.Context == StatusContext && (status.State == "failure" || status.State == "error") {
			return &status
		}
	}
	return nil
}

// FailedHeadRefusal is why make ci-local refuses to run on or attest `head`
// again, or "" when no `local-ci` failed there.
func FailedHeadRefusal(head string, statuses []Status) string {
	failed := FailedBefore(statuses)
	if failed == nil {
		return ""
	}
	return fmt.Sprintf("%s already failed on %s (%s). A failed head is resolved by a new commit: fix it in this PR if the failure is in what it changed, otherwise fix it forward in its own PR off main with a regression test, rebase on it and run again. Never re-run a head until green.",
		StatusContext, Short(head), failed.CreatedAt)
}

// SourceProblem is why an earlier head's ledger may not lend a step, or "":
// its statuses must hold a successful `local-ci` carrying this ledger's
// digest, and no failed one. A failed head lends nothing.
func SourceProblem(ledger Ledger, statuses []Status) string {
	at := Short(ledger.Head)
	if FailedBefore(statuses) != nil {
		return fmt.Sprintf("%s failed on %s", StatusContext, at)
	}
	want := LedgerDigest(ledger)
	for _, status := range statuses {
		if status.Context != StatusContext || status.State != "success" {
			continue
		}
		if match := ledgerDigest.FindStringSubmatch(status.Description); match != nil && match[1] == want {
			return ""
		}
	}
	return fmt.Sprintf("no successful %s on %s attests the ledger the section carries for it", StatusContext, at)
}

// ReuseProblems is why the section's reused steps do not hold for `head`:
// each must name an earlier head whose ledger the section carries, that
// passed the step at the same input hash, and that `local-ci` attested there
// (`sources`: that head's statuses).
func ReuseProblems(section, head string, sources map[string][]Status) []string {
	own, lent := Ledgers(section)
	if own == nil {
		return nil
	}
	problems := []string{}
	if own.Head != head {
		problems = append(problems, fmt.Sprintf("the ledger is for %s, not %s", Short(own.Head), Short(head)))
	}
	for _, step := range own.Steps {
		if step.From == "" {
			continue
		}
		var why string
		source := findLedger(lent, step.From)
		switch {
		case source == nil:
			why = "the section carries no ledger for " + Short(step.From)
		case step.From == head:
			why = "a head cannot lend to itself"
		case !source.holds(step.ID, step.Hash):
			why = fmt.Sprintf("it did not pass on %s with input hash %s", Short(step.From), step.Hash)
		default:
			why = SourceProblem(*source, sources[step.From])
		}
		if why != "" {
			problems = append(problems, fmt.Sprintf("`%s` is reused from %s, but %s", step.ID, Short(step.From), why))
		}
	}
	return problems
}

func findLedger(ledgers []Ledger, head string) *Ledger {
	for index := range ledgers {
		if ledgers[index].Head == head {
			return &ledgers[index]
		}
	}
	return nil
}

// BodyProblems is why the PR body's section does not attest `head`: missing,
// for another head, or an item unticked or missing.
func BodyProblems(body, head string) []string {
	section, found := SectionOf(body)
	if !found {
		return []string{"the PR body has no Local verification section"}
	}
	problems := []string{}
	match := sectionHead.FindStringSubmatch(section)
	switch {
	case match == nil:
		problems = append(problems, "the Local verification section names no head")
	case match[1] != head:
		problems = append(problems, fmt.Sprintf("the Local verification section is for %s, not the PR head %s", Short(match[1]), Short(head)))
	}
	ticked := map[string]bool{}
	for _, line := range checklistLine.FindAllStringSubmatch(section, -1) {
		ticked[line[2]] = line[1] != " "
	}
	for _, item := range Items {
		done, listed := ticked[item]
		switch {
		case !listed:
			problems = append(problems, "the checklist has no "+item+" item")
		case !done:
			problems = append(problems, item+" is not ticked")
		}
	}
	return problems
}

// Verification is what GitHub holds for a pull request's head: its body,
// the `local-ci` status in the combined status (nil when there is none),
// every status ever posted on the head, and those of each earlier head its
// section reuses steps from.
type Verification struct {
	Body     string
	Head     string
	Status   *Status
	Statuses []Status
	Sources  map[string][]Status
}

// Problems is every reason a pull request is not verified on its exact
// head; empty when it is. The Local verification job, the merge script and
// the staging gate all decide with it.
func (verification Verification) Problems() []string {
	head := verification.Head
	problems := BodyProblems(verification.Body, head)
	if failed := FailedBefore(verification.Statuses); failed != nil {
		problems = append(problems, fmt.Sprintf("%s failed on %s at %s: a failed head is resolved by a new commit (fix it here, or fix it forward off main and rebase), never by running again",
			StatusContext, Short(head), failed.CreatedAt))
	}
	section, found := SectionOf(verification.Body)
	switch status := verification.Status; {
	case status == nil:
		problems = append(problems, fmt.Sprintf("no %s status on %s", StatusContext, Short(head)))
	case status.State != "success":
		problems = append(problems, fmt.Sprintf("%s is %s on %s", StatusContext, status.State, Short(head)))
	case found:
		match := bodyDigest.FindStringSubmatch(status.Description)
		if match == nil || match[1] != SectionDigest(section) {
			problems = append(problems, fmt.Sprintf("the Local verification section is not the one %s attested (edited by hand?)", StatusContext))
		}
	}
	if found {
		problems = append(problems, ReuseProblems(section, head, verification.Sources)...)
	}
	return problems
}
