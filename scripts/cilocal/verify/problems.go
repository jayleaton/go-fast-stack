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
	sectionHead = regexp.MustCompile("(?m)^Head `([0-9a-f]{40})`")
	ranLine     = regexp.MustCompile(`(?m)^\*\*Ran:\*\* \S`)
	bodyDigest  = regexp.MustCompile(`body ([0-9a-f]{12})`)
)

// headOf is the head commit a section names, or "".
func headOf(section string) string {
	if match := sectionHead.FindStringSubmatch(section); match != nil {
		return match[1]
	}
	return ""
}

// FailedBefore is the first failed `local-ci` among a commit's statuses, or
// nil. A head whose author reported a failure is never attested again: a
// failure is resolved by a new commit (the fix, or a rebase on a fix merged
// off main), never by attesting until green.
func FailedBefore(statuses []Status) *Status {
	for index := range statuses {
		status := statuses[index]
		if status.Context == StatusContext && (status.State == "failure" || status.State == "error") {
			return &status
		}
	}
	return nil
}

// FailedHeadRefusal is why make ci-local refuses to attest `head`, or ""
// when no `local-ci` failed there.
func FailedHeadRefusal(head string, statuses []Status) string {
	failed := FailedBefore(statuses)
	if failed == nil {
		return ""
	}
	return fmt.Sprintf("%s already failed on %s (%s). A failed head is resolved by a new commit: fix it in this PR if the failure is in what it changed, otherwise fix it forward in its own PR off main with a regression test, rebase on it and attest again.",
		StatusContext, Short(head), failed.CreatedAt)
}

// ClaimsHead is whether make ci-local has claimed `head`: a `local-ci`
// status was posted on it (`statuses`, every status ever posted there), or
// the body's section names it. Until then nothing is attested, so ci.yml
// names its checking job "Awaiting make ci-local" and the required check
// `Local verification` stays "Expected" rather than red; once claimed, every
// problem fails it (docs/CI.md, "What runs").
func ClaimsHead(body, head string, statuses []Status) bool {
	for _, status := range statuses {
		if status.Context == StatusContext {
			return true
		}
	}
	section, _ := SectionOf(body)
	return headOf(section) == head
}

// BodyProblems is why the PR body's section does not attest `head`:
// missing, for another head, or no statement of what ran. Empty when it holds.
func BodyProblems(body, head string) []string {
	section, found := SectionOf(body)
	if !found {
		return []string{"the PR body has no Local verification section"}
	}
	problems := []string{}
	switch named := headOf(section); {
	case named == "":
		problems = append(problems, "the Local verification section names no head")
	case named != head:
		problems = append(problems, fmt.Sprintf("the Local verification section is for %s, not the PR head %s", Short(named), Short(head)))
	}
	if !ranLine.MatchString(section) {
		problems = append(problems, "the Local verification section does not say what ran")
	}
	return problems
}

// Verification is what GitHub holds for a pull request's head: its body,
// the `local-ci` status in the combined status (nil when there is none),
// every status ever posted on the head, and the fork the head lives in.
type Verification struct {
	Body     string
	Head     string
	Status   *Status
	Statuses []Status
	// Fork is the repository of a head outside this one (owner/name, or
	// ForkGone), "" for a branch of this repository.
	Fork string
}

// ForkGone stands for the fork of a pull request whose fork was deleted.
const ForkGone = "a deleted fork"

// ForkOf is the fork a head lives in: "" when `headRepo` (owner/name) is
// `repo` itself, ForkGone when GitHub names no repository.
func ForkOf(repo, headRepo string) string {
	switch headRepo {
	case repo:
		return ""
	case "":
		return ForkGone
	}
	return headRepo
}

// Claimed is whether the Local verification job checks this head under its
// required name (ClaimsHead). A fork's head always is, so it fails red
// rather than waiting as "Expected".
func Claimed(verification Verification) bool {
	return verification.Fork != "" || ClaimsHead(verification.Body, verification.Head, verification.Statuses)
}

// LocalVerificationProblems is every reason a head is not attested; empty
// when it is: the head is a branch of this repository, the section names the
// head and says what ran, `local-ci` is success there with the section's
// digest, and no `local-ci` ever failed on it. The Local verification job,
// the merge script and the staging gate all decide with it. A head behind
// main is not a problem here.
//
// A fork's head is never attested: make ci-local attests only a pushed
// branch of this repository, and a fork's author controls its body, so a
// section and status copied from another pull request must not pass.
func LocalVerificationProblems(verification Verification) []string {
	head := verification.Head
	problems := BodyProblems(verification.Body, head)
	if verification.Fork != "" {
		problems = append(problems, fmt.Sprintf("the head is in %s, not this repository: make ci-local attests only branches pushed here (push the commits to a branch of this repository and open the PR from it)", verification.Fork))
	}
	if failed := FailedBefore(verification.Statuses); failed != nil {
		problems = append(problems, fmt.Sprintf("%s failed on %s at %s: a failed head is resolved by a new commit (fix it here, or fix it forward off main and rebase), never by attesting again",
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
	return problems
}
