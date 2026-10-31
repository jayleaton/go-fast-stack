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
		{"main commit not on base", func(head verify.Head) verify.Head { head.OnBase = false; head.Base = "dev"; return head }, mainCommit, "aaaaaaaaaaaa is not on origin/dev"},
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

func TestSelectionBase(test *testing.T) {
	test.Parallel()
	attestedSet := map[string]bool{"m2": true, "m4": true}
	isAttested := func(sha string) bool { return attestedSet[sha] }
	for _, testCase := range []struct {
		name       string
		mainCommit bool
		ancestors  []string
		want       string
	}{
		{"branch uses merge-base", false, []string{"m2"}, "merge-base"},
		{"nearest attested ancestor", true, []string{"m1", "m2", "m3", "m4"}, "m2"},
		{"none attested", true, []string{"m1", "m3"}, ""},
		{"no ancestors", true, nil, ""},
	} {
		test.Run(testCase.name, func(test *testing.T) {
			test.Parallel()
			if got := verify.SelectionBase(testCase.mainCommit, "merge-base", testCase.ancestors, isAttested); got != testCase.want {
				test.Fatalf("SelectionBase = %q, want %q", got, testCase.want)
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
