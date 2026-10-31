package verify_test

import (
	"strings"
	"testing"

	"github.com/jayleaton/go-fast-stack/scripts/cilocal/verify"
)

func TestHeadRefusal(test *testing.T) {
	test.Parallel()
	branch := verify.Head{SHA: headSHA, Branch: "feat/x", Pushed: headSHA, Base: "main"}
	mainCommit := verify.Head{SHA: headSHA, MainCommit: true, OnBase: true, Base: "main"}
	for _, testCase := range []struct {
		name string
		head func(verify.Head) verify.Head
		from verify.Head
		want string // "" when it may attest
	}{
		{"clean pushed branch", func(head verify.Head) verify.Head { return head }, branch, ""},
		{"dirty", func(head verify.Head) verify.Head { head.Dirty = " M main.go"; return head }, branch, "uncommitted changes; commit and push them first:\n M main.go"},
		{"dirty main commit", func(head verify.Head) verify.Head { head.Dirty = "?? new"; return head }, mainCommit, "uncommitted changes"},
		{"detached", func(head verify.Head) verify.Head { head.Branch = ""; return head }, branch, "HEAD is detached"},
		{"not pushed", func(head verify.Head) verify.Head { head.Pushed = ""; return head }, branch, "origin/feat/x is missing, not HEAD aaaaaaaaaaaa; push first"},
		{"pushed elsewhere", func(head verify.Head) verify.Head { head.Pushed = otherSHA; return head }, branch, "origin/feat/x is bbbbbbbbbbbb, not HEAD aaaaaaaaaaaa"},
		{"main commit on base", func(head verify.Head) verify.Head { return head }, mainCommit, ""},
		{"main commit matching --commit", func(head verify.Head) verify.Head { head.Commit = headSHA; return head }, mainCommit, ""},
		{"main commit not on base", func(head verify.Head) verify.Head { head.OnBase = false; return head }, mainCommit, "aaaaaaaaaaaa is not on origin/main"},
		{"--commit mismatch", func(head verify.Head) verify.Head { head.Commit = otherSHA; return head }, mainCommit, "HEAD is aaaaaaaaaaaa, not bbbbbbbbbbbb"},
	} {
		test.Run(testCase.name, func(test *testing.T) {
			test.Parallel()
			got := testCase.head(testCase.from).Refusal()
			if testCase.want == "" {
				if got != "" {
					test.Fatalf("Refusal = %q, want none", got)
				}
				return
			}
			if !strings.Contains(got, testCase.want) {
				test.Fatalf("Refusal = %q, want it to mention %q", got, testCase.want)
			}
		})
	}
}

func TestAttestation(test *testing.T) {
	test.Parallel()
	const sha = "deadbeef"
	unverified := verified()
	unverified.Status = nil
	for _, testCase := range []struct {
		name     string
		status   string
		pulls    []verify.PullAttestation
		verdict  string
		mentions []string
	}{
		{"status on the commit", "success", nil, "local-ci on deadbeef itself", nil},
		{"attested merged pull", "", []verify.PullAttestation{{Number: 7, Verification: verified()}}, "local-ci on #7's merged head " + headSHA, nil},
		{"second pull attested", "", []verify.PullAttestation{{Number: 6, Verification: unverified}, {Number: 7, Verification: verified()}}, "#7's merged head", nil},
		{"direct push", "", nil, "", []string{"deadbeef has no passed local-ci status and no merged pull request (a direct push)"}},
		{"failed direct push", "failure", nil, "", []string{"deadbeef has local-ci failure", "a direct push"}},
		{"unattested merged pull", "pending", []verify.PullAttestation{{Number: 6, Verification: unverified}}, "",
			[]string{"deadbeef has local-ci pending", "#6's merged head " + headSHA + " is not attested: no local-ci status"}},
	} {
		test.Run(testCase.name, func(test *testing.T) {
			test.Parallel()
			verdict, reasons := verify.Attestation(sha, testCase.status, testCase.pulls)
			if testCase.verdict != "" {
				if !strings.Contains(verdict, testCase.verdict) || reasons != nil {
					test.Fatalf("Attestation = %q, %q; want attested %q", verdict, reasons, testCase.verdict)
				}
				return
			}
			if verdict != "" {
				test.Fatalf("Attestation attested %q", verdict)
			}
			for _, mention := range testCase.mentions {
				requireMention(test, reasons, mention)
			}
			if len(reasons) != len(testCase.mentions) {
				test.Fatalf("reasons = %q, want %d", reasons, len(testCase.mentions))
			}
		})
	}
}

func TestTitledPull(test *testing.T) {
	test.Parallel()
	for _, testCase := range []struct {
		message string
		want    int
	}{
		{"fix(config): refuse an unset FROM_EMAIL (#103)", 103},
		{"fix(config): refuse (#103)  \n\nbody (#5)", 103},
		{"feat: no number", 0},
		{"feat: (#12) not at the end", 0},
		{"feat: x\n\nmerged (#12)", 0},
		{"feat: overflow (#99999999999999999999999)", 0},
		{"", 0},
	} {
		test.Run(testCase.message, func(test *testing.T) {
			test.Parallel()
			if got := verify.TitledPull(testCase.message); got != testCase.want {
				test.Fatalf("TitledPull(%q) = %d, want %d", testCase.message, got, testCase.want)
			}
		})
	}
}

func TestAttestNextStep(test *testing.T) {
	test.Parallel()
	got := verify.AttestNextStep(headSHA)
	for _, want := range []string{"git worktree add --detach <dir> " + headSHA, "make ci-local COMMIT=" + headSHA + ` RAN="<what you ran>"`, "dispatch Deploy staging"} {
		if !strings.Contains(got, want) {
			test.Fatalf("AttestNextStep = %q, want it to mention %q", got, want)
		}
	}
}

// Randy's review of #134 (TestRejectedDirtyTreeReportCanBePublished): a run
// refused for worktree changes still published its report as success. The
// checkout is read again just before posting, and a report is posted only
// for exactly that clean, pushed head and tree.
func TestPublishRefusal(test *testing.T) {
	test.Parallel()
	report := passingReport()
	tree := report.Tree
	clean := verify.Head{SHA: headSHA, Branch: "feat/x", Pushed: headSHA, Base: "main"}
	for _, testCase := range []struct {
		name string
		head func(verify.Head) verify.Head
		tree string
		want string // "" when it may post
	}{
		{"clean and unchanged", func(head verify.Head) verify.Head { return head }, tree, ""},
		{"a file changed since the report", func(head verify.Head) verify.Head { head.Dirty = " M README.md"; return head }, tree, "uncommitted changes"},
		{"HEAD moved", func(head verify.Head) verify.Head { head.SHA, head.Pushed = otherSHA, otherSHA; return head }, tree, "the report is for aaaaaaaaaaaa, but HEAD is now bbbbbbbbbbbb"},
		{"not pushed any more", func(head verify.Head) verify.Head { head.Pushed = otherSHA; return head }, tree, "push first"},
		{"another tree", func(head verify.Head) verify.Head { return head }, strings.Repeat("f", 40), "the report is for tree dddddddddddd, but HEAD's tree is ffffffffffff"},
	} {
		test.Run(testCase.name, func(test *testing.T) {
			test.Parallel()
			got := verify.PublishRefusal(report, testCase.head(clean), testCase.tree)
			if testCase.want == "" {
				if got != "" {
					test.Fatalf("PublishRefusal = %q, want none", got)
				}
				return
			}
			if !strings.Contains(got, testCase.want) {
				test.Fatalf("PublishRefusal = %q, want it to mention %q", got, testCase.want)
			}
		})
	}
}

func TestLastAttested(test *testing.T) {
	test.Parallel()
	attestedSet := map[string]bool{"m2": true, "m4": true}
	isAttested := func(sha string) bool { return attestedSet[sha] }
	for _, testCase := range []struct {
		name      string
		ancestors []string
		want      string
	}{
		{"nearest attested ancestor", []string{"m1", "m2", "m3", "m4"}, "m2"},
		{"none attested", []string{"m1", "m3"}, ""},
		{"a root commit", nil, ""},
	} {
		test.Run(testCase.name, func(test *testing.T) {
			test.Parallel()
			if got := verify.LastAttested(testCase.ancestors, isAttested); got != testCase.want {
				test.Fatalf("LastAttested = %q, want %q", got, testCase.want)
			}
		})
	}
}

func TestRanFullCheck(test *testing.T) {
	test.Parallel()
	for _, testCase := range []struct {
		ran  string
		want bool
	}{
		{"make ci", true},
		{"make lint; make ci", true},
		{"go build ./... && make ci", true},
		{"davit box run -- make ci", true},
		{"make -j4 ci", true},
		{"make ci-local", false},
		{"make lint test", false},
		{"ci", false},
		{"", false},
	} {
		if got := verify.RanFullCheck(testCase.ran); got != testCase.want {
			test.Errorf("RanFullCheck(%q) = %v, want %v", testCase.ran, got, testCase.want)
		}
	}
}

// Randy's review of #134 (TestNoAttestedAncestorHasNoBootstrapBase): a new
// project has no attested main commit, and the first attestation was
// refused before anything ran. It is allowed, and it must cover the whole
// tree: with no attested main commit before it, the statement names make ci.
func TestFirstAttestationProblem(test *testing.T) {
	test.Parallel()
	if got := verify.FirstAttestationProblem(headSHA, "", "make ci"); got != "" {
		test.Fatalf("a first attestation that ran make ci is refused: %q", got)
	}
	if got := verify.FirstAttestationProblem(headSHA, otherSHA, "make lint"); got != "" {
		test.Fatalf("an attestation since an attested main commit is refused: %q", got)
	}
	got := verify.FirstAttestationProblem(headSHA, "", "make lint test")
	for _, want := range []string{"no attested main commit among the 50 before aaaaaaaaaaaa", "run make ci on it", `make ci-local COMMIT=aaaaaaaaaaaa RAN="make ci"`} {
		if !strings.Contains(got, want) {
			test.Errorf("FirstAttestationProblem = %q, want it to mention %q", got, want)
		}
	}
}
