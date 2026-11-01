package verify_test

import (
	"strings"
	"testing"

	"github.com/jayleaton/go-fast-stack/scripts/cilocal/verify"
)

const (
	parentHead = "1111111111111111111111111111111111111111"
	olderHead  = "2222222222222222222222222222222222222222"
)

func TestOntoCommandReplaysOnlyTheBranchsOwnCommits(test *testing.T) {
	test.Parallel()
	if got := verify.OntoCommand("main", parentHead, "feat/child"); got != "git rebase --onto origin/main "+parentHead+" feat/child" {
		test.Fatalf("OntoCommand = %q", got)
	}
}

func TestPlanStackRebase(test *testing.T) {
	test.Parallel()
	for name, tc := range map[string]struct {
		base           string
		parent, merged *verify.StackParent
		want           verify.StackRebase
		wantErr        string
	}{
		"a branch on main with no merged parent rebases plainly": {
			base: "main",
			want: verify.StackRebase{Base: "main"},
		},
		"stacked on a squash-merged parent: onto its base, after its head, retargeted": {
			base:   "feat/parent",
			parent: &verify.StackParent{Number: 7, State: "MERGED", Head: parentHead, Base: "main"},
			want:   verify.StackRebase{Base: "main", Upstream: parentHead, Retarget: true},
		},
		"the parent's own base, not main, when it merged into one": {
			base:   "feat/parent",
			parent: &verify.StackParent{Number: 7, State: "MERGED", Head: parentHead, Base: "feat/integration"},
			want:   verify.StackRebase{Base: "feat/integration", Upstream: parentHead, Retarget: true},
		},
		"already retargeted after the parent merged: found by its head among the branch's commits": {
			base:   "main",
			merged: &verify.StackParent{Number: 7, State: "MERGED", Head: parentHead, Base: "main"},
			want:   verify.StackRebase{Base: "main", Upstream: parentHead},
		},
		"the parent branch's PR wins over an older merged ancestor": {
			base:   "feat/parent",
			parent: &verify.StackParent{Number: 7, State: "MERGED", Head: parentHead, Base: "main"},
			merged: &verify.StackParent{Number: 5, State: "MERGED", Head: olderHead, Base: "feat/parent"},
			want:   verify.StackRebase{Base: "main", Upstream: parentHead, Retarget: true},
		},
		"stacked on an open parent: land that first": {
			base:    "feat/parent",
			parent:  &verify.StackParent{Number: 7, State: "OPEN", Head: parentHead, Base: "main"},
			wantErr: "land it first (make land PR=7)",
		},
		"stacked on a parent closed without merging: by hand": {
			base:    "feat/parent",
			parent:  &verify.StackParent{Number: 7, State: "CLOSED", Head: parentHead, Base: "main"},
			wantErr: "CLOSED without merging",
		},
	} {
		test.Run(name, func(test *testing.T) {
			test.Parallel()
			got, err := verify.PlanStackRebase(tc.base, tc.parent, tc.merged)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					test.Fatalf("err = %v, want one containing %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				test.Fatal(err)
			}
			if got.Reason == "" {
				test.Error("no reason given")
			}
			got.Reason = ""
			if got != tc.want {
				test.Fatalf("plan = %+v, want %+v", got, tc.want)
			}
		})
	}
}

// A real case (a child PR stacked on its parent): make land rebased the parent before
// merging it (heads 4017f6c, f7daad1, then 2591fd4, squashed as 00fa21d),
// and make pr-merge retargeted the child to main, while it was still built on
// f7daad1. A plain rebase replayed the parent's two commits onto the squash and
// conflicted.
var (
	stackChild = []verify.Patched{ // the child's old head 4afe38e, down to main
		{Commit: "4afe38e9216fff4de3556ed66ff57bea8535c038", PatchID: "own-first"},
		{Commit: "caa2a19", PatchID: "own-second"},
		{Commit: "f7daad197a4529999490a950b9bfc3387222e76b", PatchID: "5bd6e9d73a"},
		{Commit: "9d74480", PatchID: "f6a926015e"},
	}
	parentHeads = []string{
		"4017f6c843dff114c632f8b4fa9fde7bcd0f4329", "f7daad197a4529999490a950b9bfc3387222e76b",
		"2591fd4da841b97b6c2535899d9efa71e6680699",
	}
	parentSquashed = []verify.Patched{{Commit: "a6dc2d8", PatchID: "f6a926015e"}, {Commit: "2591fd4da841b97b6c2535899d9efa71e6680699", PatchID: "5bd6e9d73a"}}
)

func TestForkPointFindsTheParentsOldHeadInTheChild(test *testing.T) {
	test.Parallel()
	if got := verify.ForkPoint(stackChild, parentHeads, parentSquashed); got != "f7daad197a4529999490a950b9bfc3387222e76b" {
		test.Fatalf("ForkPoint = %q, want the parent's old head f7daad1", got)
	}
}

func TestForkPointFallsBackToPatchIDs(test *testing.T) {
	test.Parallel()
	// No head history (or the child was made from a commit that was never
	// pushed as the head): the patches still match.
	if got := verify.ForkPoint(stackChild, nil, parentSquashed); got != "f7daad197a4529999490a950b9bfc3387222e76b" {
		test.Fatalf("ForkPoint = %q, want f7daad1 by patch-id", got)
	}
}

func TestForkPointIsNoneOnceTheChildHoldsNoneOfTheParent(test *testing.T) {
	test.Parallel()
	// The child was then rebased by hand (728d3e2): only its own two commits.
	rebased := []verify.Patched{{Commit: "728d3e2", PatchID: "own-first"}, {Commit: "b1", PatchID: "own-second"}}
	if got := verify.ForkPoint(rebased, parentHeads, parentSquashed); got != "" {
		test.Fatalf("ForkPoint = %q, want none (a plain rebase)", got)
	}
	if got := verify.ForkPoint([]verify.Patched{{Commit: "c1", PatchID: ""}}, nil, []verify.Patched{{Commit: "p1", PatchID: ""}}); got != "" {
		test.Fatalf("ForkPoint matched an empty patch-id: %q", got)
	}
}

func TestPlanStackRebaseForARetargetedChildOfAMergedParent(test *testing.T) {
	test.Parallel()
	// The child after the retarget: base main, its former base's PR merged.
	got, err := verify.PlanStackRebase("main", &verify.StackParent{Number: 59, State: "MERGED", Head: parentHeads[2], Base: "main"}, nil)
	if err != nil {
		test.Fatal(err)
	}
	if got.Base != "main" || got.Upstream != parentHeads[2] || got.Retarget {
		test.Fatalf("plan = %+v, want onto main past the parent's head, no retarget (already on main)", got)
	}
}
