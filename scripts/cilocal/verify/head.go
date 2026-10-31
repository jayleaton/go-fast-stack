package verify

import (
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

// Head is the checkout make ci-local was started in.
type Head struct {
	SHA        string
	Branch     string // empty when detached
	Dirty      string // `git status --porcelain`, empty when clean
	Commit     string // the resolved COMMIT sha, or empty
	MainCommit bool   // attesting a commit of the base branch (COMMIT, or on it)
	Pushed     string // the remote branch's head, or empty when it has none
	OnBase     bool   // SHA is reachable from origin/<base>
	Base       string // the branch it lands on: main (scripts/branches.sh)
}

// Refusal is why make ci-local may not attest the head, or "": only a clean
// checkout whose HEAD is exactly the pushed branch head (or, with COMMIT, a
// commit of the base branch checked out detached) is attested.
func (head Head) Refusal() string {
	sha := Short(head.SHA)
	if head.Dirty != "" {
		return "the worktree has uncommitted changes; commit and push them first:\n" + head.Dirty
	}
	if head.MainCommit {
		if head.Commit != "" && head.Commit != head.SHA {
			return fmt.Sprintf("HEAD is %s, not %s; check it out in a clean worktree (git worktree add --detach <dir> %s)", sha, Short(head.Commit), Short(head.Commit))
		}
		if !head.OnBase {
			return fmt.Sprintf("%s is not on origin/%s; COMMIT attests only a commit of the base branch", sha, head.Base)
		}
		return ""
	}
	if head.Branch == "" {
		return "HEAD is detached; check out the PR branch, or pass COMMIT=<sha> for a commit of the base branch"
	}
	if head.Pushed != head.SHA {
		pushed := "missing"
		if head.Pushed != "" {
			pushed = Short(head.Pushed)
		}
		return fmt.Sprintf("origin/%s is %s, not HEAD %s; push first", head.Branch, pushed, sha)
	}
	return ""
}

// PublishRefusal is why make ci-local may not post `report`, or "": it reads
// the checkout again just before it posts (`head`, `tree`), and the report
// must be for exactly that clean, pushed head and its tree. One rule for
// every way a report is posted, so none can publish for a worktree that
// changed or a HEAD that moved after the report was made.
func PublishRefusal(report Report, head Head, tree string) string {
	if refusal := head.Refusal(); refusal != "" {
		return refusal
	}
	if report.Head != head.SHA {
		return fmt.Sprintf("the report is for %s, but HEAD is now %s", Short(report.Head), Short(head.SHA))
	}
	if report.Tree != tree {
		return fmt.Sprintf("the report is for tree %s, but HEAD's tree is %s", Short(report.Tree), Short(tree))
	}
	return ""
}

// AttestedAncestorLimit is how many first-parent ancestors of a main commit
// make ci-local reads for the last attested one.
const AttestedAncestorLimit = 50

// LastAttested is the nearest of a main commit's first-parent `ancestors`
// (nearest first) the staging gate accepts, or "" when none is: a new
// project's first attestation, or a longer run of unattested commits.
func LastAttested(ancestors []string, attested func(string) bool) string {
	for _, ancestor := range ancestors {
		if attested(ancestor) {
			return ancestor
		}
	}
	return ""
}

// FullCheck is the check over the whole tree a first attestation runs.
const FullCheck = "make ci"

// RanFullCheck is whether a statement names FullCheck: a command (parts
// joined with `;` or `&&`) that runs make with the target ci, directly or
// through a wrapper (`davit box run -- make ci`).
func RanFullCheck(ran string) bool {
	for _, part := range strings.FieldsFunc(strings.ReplaceAll(ran, "&&", ";"), func(char rune) bool { return char == ';' }) {
		fields := strings.Fields(part)
		for index, field := range fields {
			if field == "make" && slices.Contains(fields[index+1:], "ci") {
				return true
			}
		}
	}
	return false
}

// FirstAttestationProblem is why a main commit's statement `ran` may not
// attest it, or "": with no attested main commit before it (`last` empty)
// nothing narrows what changed, so it must have run the whole check.
func FirstAttestationProblem(sha, last, ran string) string {
	if last != "" || RanFullCheck(ran) {
		return ""
	}
	return fmt.Sprintf("no attested main commit among the %d before %s (a new project's first attestation, or a longer unattested run), so its statement covers the whole tree: run %s on it, then make ci-local COMMIT=%s RAN=%q",
		AttestedAncestorLimit, Short(sha), FullCheck, Short(sha), FullCheck)
}

// PullAttestation is a merged pull request whose merge commit is the commit
// being judged, with what GitHub holds for its merged head.
type PullAttestation struct {
	Number int
	Verification
}

// Attestation is the staging gate's verdict on a commit of a deployed
// branch: attested when `local-ci` passed on the commit itself (`status`,
// its state there, or ""), or when it is the merge of a pull request whose
// merged head holds a valid local verification. A direct push, or the merge
// of an unattested head, is not.
func Attestation(sha, status string, pulls []PullAttestation) (string, []string) {
	if status == "success" {
		return fmt.Sprintf("%s on %s itself", StatusContext, sha), nil
	}
	reasons := []string{}
	if status != "" {
		reasons = append(reasons, fmt.Sprintf("%s has %s %s", sha, StatusContext, status))
	}
	for _, pull := range pulls {
		problems := LocalVerificationProblems(pull.Verification)
		if len(problems) == 0 {
			return fmt.Sprintf("%s on #%d's merged head %s", StatusContext, pull.Number, pull.Head), nil
		}
		reasons = append(reasons, fmt.Sprintf("#%d's merged head %s is not attested: %s", pull.Number, pull.Head, strings.Join(problems, "; ")))
	}
	if len(pulls) == 0 {
		reasons = append(reasons, fmt.Sprintf("%s has no passed %s status and no merged pull request (a direct push)", sha, StatusContext))
	}
	return "", reasons
}

// AttestNextStep is how to attest an unattested main commit, so the staging
// gate passes it.
func AttestNextStep(sha string) string {
	return fmt.Sprintf("in a clean worktree of it (git worktree add --detach <dir> %s), run what the commits since the last attested main commit need (%s when there is none, as in a new project), then make ci-local COMMIT=%s RAN=\"<what you ran>\" there: it posts local-ci on %s. Then dispatch Deploy staging (Staging catch-up also dispatches it within the hour)",
		sha, FullCheck, sha, Short(sha))
}

var squashTitle = regexp.MustCompile(`\(#(\d+)\)$`)

// TitledPull is the pull request a squash commit's title names (`... (#12)`),
// or 0. GitHub's commit-to-PR index can trail a fresh merge.
func TitledPull(message string) int {
	title, _, _ := strings.Cut(message, "\n")
	match := squashTitle.FindStringSubmatch(strings.TrimSpace(title))
	if match == nil {
		return 0
	}
	number, err := strconv.Atoi(match[1])
	if err != nil {
		return 0
	}
	return number
}
