package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os/exec"
	"slices"
	"strconv"
	"strings"

	"github.com/jayleaton/go-fast-stack/scripts/cilocal/verify"
)

// landRebaseCommand is make land's rebase plan for a pull request
// (verify.PlanStackRebase): run in its branch's checkout after a fetch, it
// prints
//
//	base <branch to rebase onto>
//	upstream <the merged parent's head, or ->
//	reason <why>
//
// and, without --dry-run, first retargets the PR to its new base when its
// parent merged. A merged parent's head that is not in the branch's history
// (the parent was pushed again after the branch was made) is an error: the
// rebase is then a person's job.
//
//	cilocal land-rebase [--dry-run] [--head <commit>] <pr>
func landRebaseCommand(ctx context.Context, args []string) error {
	flags := flag.NewFlagSet("land-rebase", flag.ContinueOnError)
	dryRun := flags.Bool("dry-run", false, "say the plan, retarget nothing")
	head := flags.String("head", "HEAD", "the branch's commit (tests)")
	if err := flags.Parse(args); err != nil {
		return usageError{message: err.Error()}
	}
	if flags.NArg() != 1 {
		return usageError{message: "usage: cilocal land-rebase [--dry-run] [--head <commit>] <pr>"}
	}
	number, err := strconv.Atoi(flags.Arg(0))
	if err != nil {
		return usageError{message: "not a pull request number: " + flags.Arg(0)}
	}
	hub, err := newGitHub(ctx)
	if err != nil {
		return err
	}
	pull, err := hub.pull(ctx, number)
	if err != nil {
		return err
	}
	parent, err := hub.stackParent(ctx, pull.BaseRefName)
	if err != nil {
		return err
	}
	if parent == nil {
		// Retargeted when its parent merged: the parent is the merged PR
		// whose head branch this PR was based on before.
		if parent, err = hub.formerParent(ctx, number, pull.BaseRefName); err != nil {
			return err
		}
	}
	var merged *verify.StackParent
	if parent == nil {
		if merged, err = hub.mergedAncestor(ctx, number, pull.BaseRefName, *head); err != nil {
			return err
		}
	}
	plan, err := verify.PlanStackRebase(pull.BaseRefName, parent, merged)
	if err != nil {
		return usageError{message: fmt.Sprintf("#%d: %v", number, err)}
	}
	if plan.Upstream != "" && parent != nil && !isAncestor(ctx, plan.Upstream, *head) {
		// The parent was pushed again after this branch was made (make land
		// rebases it before merging): find where this branch's own commits
		// start (verify.ForkPoint), or none when it holds none of the
		// parent's commits any more (rebased past them by hand).
		fork, err := hub.forkPoint(ctx, parent.Number, plan.Base, *head)
		if err != nil {
			return err
		}
		plan.Upstream = fork
		if fork != "" {
			plan.Reason += fmt.Sprintf("; the parent was pushed again after this branch was made: its own commits start after %s, one of #%d's commits", verify.Short(fork), parent.Number)
		} else {
			plan.Reason += fmt.Sprintf("; this branch holds none of #%d's commits (it was rebased past them): a plain rebase", parent.Number)
		}
	}
	if plan.Retarget && !*dryRun {
		if _, err := output(ctx, "", "gh", "pr", "edit", strconv.Itoa(number), "-R", hub.repo, "--base", plan.Base); err != nil {
			return err
		}
	}
	upstream := plan.Upstream
	if upstream == "" {
		upstream = "-"
	}
	fmt.Printf("base %s\nupstream %s\nreason %s\n", plan.Base, upstream, plan.Reason)
	return nil
}

func isAncestor(ctx context.Context, commit, head string) bool {
	_, err := git(ctx, "", "merge-base", "--is-ancestor", commit, head)
	return err == nil
}

// forkPoint is verify.ForkPoint for the branch at `head` (its commits not
// on origin/<base>) and pull request `parent`: every head it ever had
// (parentHeads), and the commits of each of them not on origin/<base>.
func (hub github) forkPoint(ctx context.Context, parent int, base, head string) (string, error) {
	heads, err := hub.parentHeads(ctx, parent)
	if err != nil {
		return "", err
	}
	var parentPatches []verify.Patched
	seen := map[string]bool{}
	for _, parentHead := range heads {
		// GitHub serves a force-pushed head by its id while it keeps it;
		// one it no longer has adds nothing.
		if _, err := git(ctx, "", "fetch", "-q", "origin", parentHead); err != nil {
			continue
		}
		commits, err := git(ctx, "", "rev-list", "--max-count="+strconv.Itoa(maxStackCommits), parentHead, "^origin/"+base)
		if err != nil {
			return "", err
		}
		for _, commit := range strings.Fields(commits) {
			if seen[commit] {
				continue
			}
			seen[commit] = true
			id, err := patchID(ctx, commit)
			if err != nil {
				return "", err
			}
			parentPatches = append(parentPatches, verify.Patched{Commit: commit, PatchID: id})
		}
	}
	own, err := git(ctx, "", "rev-list", "--max-count="+strconv.Itoa(maxStackCommits), "origin/"+base+".."+head)
	if err != nil {
		return "", err
	}
	var childPatches []verify.Patched
	for _, commit := range strings.Fields(own) {
		id, err := patchID(ctx, commit)
		if err != nil {
			return "", err
		}
		childPatches = append(childPatches, verify.Patched{Commit: commit, PatchID: id})
	}
	return verify.ForkPoint(childPatches, heads, parentPatches), nil
}

// pullTimeline is the parts of a pull request's timeline make land reads:
// its head before and after each force push, and its earlier bases.
type pullTimeline struct {
	HeadRefOid string
	Pushes     []struct{ Before, After string }
	Bases      []string // previous base branches, oldest first
}

func (hub github) timeline(ctx context.Context, number int) (pullTimeline, error) {
	owner, name, _ := strings.Cut(hub.repo, "/")
	const query = `query($owner:String!,$name:String!,$number:Int!){repository(owner:$owner,name:$name){pullRequest(number:$number){
headRefOid timelineItems(first:100,itemTypes:[HEAD_REF_FORCE_PUSHED_EVENT,BASE_REF_CHANGED_EVENT]){nodes{__typename
... on HeadRefForcePushedEvent{beforeCommit{oid} afterCommit{oid}} ... on BaseRefChangedEvent{previousRefName}}}}}}`
	out, err := output(ctx, "", "gh", "api", "graphql", "-f", "query="+query, "-f", "owner="+owner, "-f", "name="+name, "-F", "number="+strconv.Itoa(number))
	if err != nil {
		return pullTimeline{}, err
	}
	var data struct {
		Data struct {
			Repository struct {
				PullRequest struct {
					HeadRefOid    string `json:"headRefOid"`
					TimelineItems struct {
						Nodes []struct {
							Typename        string                `json:"__typename"`
							BeforeCommit    *struct{ Oid string } `json:"beforeCommit"`
							AfterCommit     *struct{ Oid string } `json:"afterCommit"`
							PreviousRefName string                `json:"previousRefName"`
						} `json:"nodes"`
					} `json:"timelineItems"`
				} `json:"pullRequest"`
			} `json:"repository"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(out), &data); err != nil {
		return pullTimeline{}, fmt.Errorf("gh api graphql (timeline of #%d): %w", number, err)
	}
	pull := data.Data.Repository.PullRequest
	timeline := pullTimeline{HeadRefOid: pull.HeadRefOid}
	for _, node := range pull.TimelineItems.Nodes {
		switch node.Typename {
		case "HeadRefForcePushedEvent":
			push := struct{ Before, After string }{}
			if node.BeforeCommit != nil {
				push.Before = node.BeforeCommit.Oid
			}
			if node.AfterCommit != nil {
				push.After = node.AfterCommit.Oid
			}
			timeline.Pushes = append(timeline.Pushes, push)
		case "BaseRefChangedEvent":
			timeline.Bases = append(timeline.Bases, node.PreviousRefName)
		}
	}
	return timeline, nil
}

// parentHeads is every head pull request `number` ever had that GitHub
// recorded: before and after each force push, and the final one.
func (hub github) parentHeads(ctx context.Context, number int) ([]string, error) {
	timeline, err := hub.timeline(ctx, number)
	if err != nil {
		return nil, err
	}
	heads := []string{}
	for _, push := range timeline.Pushes {
		for _, commit := range []string{push.Before, push.After} {
			if commit != "" && !slices.Contains(heads, commit) {
				heads = append(heads, commit)
			}
		}
	}
	if !slices.Contains(heads, timeline.HeadRefOid) {
		heads = append(heads, timeline.HeadRefOid)
	}
	return heads, nil
}

// formerParent is the merged pull request, into `base`, whose head branch
// pull request `number` was based on before it was retargeted to `base`
// (newest retarget first), or nil.
func (hub github) formerParent(ctx context.Context, number int, base string) (*verify.StackParent, error) {
	timeline, err := hub.timeline(ctx, number)
	if err != nil {
		return nil, err
	}
	for index := len(timeline.Bases) - 1; index >= 0; index-- {
		parent, err := hub.stackParent(ctx, timeline.Bases[index])
		if err != nil {
			return nil, err
		}
		if parent != nil && parent.State == "MERGED" && parent.Base == base {
			return parent, nil
		}
	}
	return nil, nil
}

// patchID is `git patch-id --stable` of one commit ("" for an empty one).
func patchID(ctx context.Context, commit string) (string, error) {
	command := exec.CommandContext(ctx, "sh", "-c", `git show --format= "$1" | git patch-id --stable`, "patch-id", commit)
	out, err := command.Output()
	if err != nil {
		return "", fmt.Errorf("git patch-id of %s: %w", verify.Short(commit), err)
	}
	id, _, _ := strings.Cut(strings.TrimSpace(string(out)), " ")
	return id, nil
}

// stackParent is the pull request whose head branch is `base`, newest first
// (nil when there is none, as for main).
func (hub github) stackParent(ctx context.Context, base string) (*verify.StackParent, error) {
	var pulls []struct {
		Number      int    `json:"number"`
		State       string `json:"state"`
		HeadRefOid  string `json:"headRefOid"`
		BaseRefName string `json:"baseRefName"`
		Cross       bool   `json:"isCrossRepository"`
	}
	out, err := output(ctx, "", "gh", "pr", "list", "-R", hub.repo, "--head", base, "--state", "all",
		"--json", "number,state,headRefOid,baseRefName,isCrossRepository")
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal([]byte(out), &pulls); err != nil {
		return nil, fmt.Errorf("gh pr list --head %s: %w", base, err)
	}
	for _, pull := range pulls {
		if !pull.Cross {
			return &verify.StackParent{Number: pull.Number, State: pull.State, Head: pull.HeadRefOid, Base: pull.BaseRefName}, nil
		}
	}
	return nil, nil
}

// maxStackCommits bounds the search for a merged parent among a branch's own
// commits (one API call each); a stack deeper than this is rebased by hand.
const maxStackCommits = 30

// mergedAncestor is the newest pull request merged into `base`, other than
// `self`, whose head commit is one of the branch's own commits (those of
// `head` not on origin/<base>), or nil.
func (hub github) mergedAncestor(ctx context.Context, self int, base, head string) (*verify.StackParent, error) {
	commits, err := git(ctx, "", "rev-list", "--max-count="+strconv.Itoa(maxStackCommits), "origin/"+base+".."+head)
	if err != nil {
		return nil, err
	}
	for _, commit := range strings.Fields(commits) {
		var pulls []struct {
			Number   int     `json:"number"`
			MergedAt *string `json:"merged_at"`
			Head     struct {
				Sha string `json:"sha"`
			} `json:"head"`
			Base struct {
				Ref string `json:"ref"`
			} `json:"base"`
		}
		if err := hub.api(ctx, "commits/"+commit+"/pulls", &pulls); err != nil {
			return nil, err
		}
		for _, pull := range pulls {
			if pull.Number != self && pull.MergedAt != nil && pull.Head.Sha == commit && pull.Base.Ref == base {
				return &verify.StackParent{Number: pull.Number, State: "MERGED", Head: commit, Base: base}, nil
			}
		}
	}
	return nil, nil
}
