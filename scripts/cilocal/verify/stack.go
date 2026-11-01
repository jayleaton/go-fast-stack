package verify

import "fmt"

// A stacked pull request's parent is squash-merged: its commits reach the
// base as one new commit, while the child's branch still holds the parent's
// own commits. A plain `git rebase origin/<base>` replays those again on top
// of the squash (conflicts, or the same change twice); only the child's own
// commits, the ones after the parent's head, belong on the base.

// StackParent is the pull request a branch was stacked on.
type StackParent struct {
	Number int
	State  string // OPEN, MERGED or CLOSED
	Head   string // its head commit (when it merged, for a merged one)
	Base   string // the branch it merged into
}

// StackRebase is how make land rebases a pull request before it verifies it.
type StackRebase struct {
	// Base is the branch to rebase onto, and the PR's base from now on.
	Base string
	// Upstream is the merged parent's head for `git rebase --onto origin/<Base>
	// <Upstream>`; "" for a plain `git rebase origin/<Base>`.
	Upstream string
	// Retarget is whether the PR's base on GitHub moves to Base first.
	Retarget bool
	// Reason says why, for the landing's log.
	Reason string
}

// OntoCommand is the rebase that moves a branch stacked on a squash-merged
// parent whose head was `parentHead` onto `base`, replaying only the
// branch's own commits. make pr-merge prints it for each PR it retargets and
// make land runs it (PlanStackRebase).
func OntoCommand(base, parentHead, branch string) string {
	return fmt.Sprintf("git rebase --onto origin/%s %s %s", base, parentHead, branch)
}

// PlanStackRebase decides the rebase of a pull request whose base is `base`.
// `parent` is the pull request whose head branch is `base`, or, once the PR
// was retargeted to the parent's base (make pr-merge does that as it merges
// the parent, GitHub when it deletes the parent's branch), the merged pull
// request whose head branch was the PR's base before (nil when neither: a
// branch made on a trunk such as main); `mergedAncestor` is the newest
// merged pull request into `base` whose head is one of the branch's own
// commits (nil when there is none), which finds a parent that was merged and
// the child already retargeted (make pr-merge does that, and GitHub does
// when it deletes the parent's branch).
func PlanStackRebase(base string, parent, mergedAncestor *StackParent) (StackRebase, error) {
	if parent != nil {
		switch parent.State {
		case "MERGED":
			retarget := ""
			if parent.Base != base {
				retarget = ", and retarget to " + parent.Base
			}
			return StackRebase{Base: parent.Base, Upstream: parent.Head, Retarget: parent.Base != base,
				Reason: fmt.Sprintf("stacked on #%d, squash-merged into %s: rebase only this branch's own commits (after %s) onto it%s",
					parent.Number, parent.Base, Short(parent.Head), retarget)}, nil
		case "OPEN":
			return StackRebase{}, fmt.Errorf("stacked on #%d, still open: land it first (make land PR=%d), then this one", parent.Number, parent.Number)
		default:
			return StackRebase{}, fmt.Errorf("stacked on #%d, which is %s without merging: point this PR at its own base (gh pr edit --base main) and rebase it by hand", parent.Number, parent.State)
		}
	}
	if mergedAncestor != nil {
		return StackRebase{Base: base, Upstream: mergedAncestor.Head, Retarget: false,
			Reason: fmt.Sprintf("holds the commits of #%d, squash-merged into %s: rebase only this branch's own commits (after %s) onto it",
				mergedAncestor.Number, base, Short(mergedAncestor.Head))}, nil
	}
	return StackRebase{Base: base, Upstream: "", Retarget: false, Reason: "not stacked on a merged pull request: a plain rebase onto " + base}, nil
}

// Patched is a commit and its `git patch-id --stable`.
type Patched struct {
	Commit, PatchID string
}

// ForkPoint is where a child branch's own commits start when the merged
// parent's final head is not in its history. make land rebases a parent
// before it merges it (and an author may push it again), so a child made
// earlier holds an older copy of the parent's commits. `child` is the
// child's commits not on the base, newest first; `heads` every head the
// parent pull request ever had (its force pushes' before and after, and
// its final head); `parent` the parent's commits with their patch-ids.
//
// The newest child commit that was once the parent's head is exact. Else,
// the newest child commit with the patch of one of the parent's commits: a
// clean rebase keeps each commit's patch. "" when the child holds none of
// the parent's commits (it was already rebased past them): a plain rebase.
func ForkPoint(child []Patched, heads []string, parent []Patched) string {
	wasHead := map[string]bool{}
	for _, head := range heads {
		wasHead[head] = true
	}
	for _, commit := range child {
		if wasHead[commit.Commit] {
			return commit.Commit
		}
	}
	ids := map[string]bool{}
	for _, commit := range parent {
		if commit.PatchID != "" {
			ids[commit.PatchID] = true
		}
	}
	for _, commit := range child {
		if ids[commit.PatchID] {
			return commit.Commit
		}
	}
	return ""
}
