package verify

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// Head is the checkout make ci-local was started in.
type Head struct {
	SHA        string
	Branch     string // empty when detached
	Dirty      string // `git status --porcelain`, empty when clean
	Commit     string // the resolved --commit sha, or empty
	MainCommit bool   // attesting a commit of the base branch (--commit, or on it)
	Pushed     string // the remote branch's head, or empty when it has none
	OnBase     bool   // SHA is reachable from origin/<base>
	Base       string // the branch it lands on: main, or dev where it exists
}

// Refusal is why make ci-local may not attest the head, or "": only a clean
// checkout whose HEAD is exactly the pushed branch head (or, with --commit,
// a commit of the base branch) is attested. The steps then run in a fresh
// worktree of that commit.
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
			return fmt.Sprintf("%s is not on origin/%s; --commit attests only a commit of the base branch", sha, head.Base)
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

// AttestedAncestorLimit is how far back a base-branch commit looks for an
// attested ancestor to select against before refusing.
const AttestedAncestorLimit = 50

// SelectionBase is the commit a run selects against: a PR branch's
// merge-base with origin/<base>, or for a base-branch commit the nearest of
// its first-parent `ancestors` (nearest first) that the staging gate
// accepts, so every unattested commit since then is in the diff. Empty when
// none is.
func SelectionBase(mainCommit bool, mergeBase string, ancestors []string, attested func(string) bool) string {
	if !mainCommit {
		return mergeBase
	}
	for _, ancestor := range ancestors {
		if attested(ancestor) {
			return ancestor
		}
	}
	return ""
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
		problems := pull.Problems()
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
