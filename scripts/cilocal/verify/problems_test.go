package verify_test

import (
	"strings"
	"testing"

	"github.com/jayleaton/go-fast-stack/scripts/cilocal/verify"
)

func TestBodyProblems(test *testing.T) {
	test.Parallel()
	section := verify.RenderSection(passingReport())
	noHead := strings.Replace(section, "Head `", "Head: `", 1)
	noRan := strings.Replace(section, "**Ran:** ", "Ran: ", 1)
	emptyRan := strings.Replace(section, "**Ran:** "+passingReport().Ran, "**Ran:** ", 1)
	for _, testCase := range []struct {
		name string
		body string
		head string
		want string
	}{
		{"no section", "just a body", headSHA, "has no Local verification section"},
		{"wrong head", verify.WithSection("", section), otherSHA, "is for aaaaaaaaaaaa, not the PR head bbbbbbbbbbbb"},
		{"no head", verify.WithSection("", noHead), headSHA, "names no head"},
		{"no statement", verify.WithSection("", noRan), headSHA, "does not say what ran"},
		{"empty statement", verify.WithSection("", emptyRan), headSHA, "does not say what ran"},
		{"the template's placeholder", verify.WithSection("", "Not attested yet."), headSHA, "names no head"},
	} {
		test.Run(testCase.name, func(test *testing.T) {
			test.Parallel()
			requireMention(test, verify.BodyProblems(testCase.body, testCase.head), testCase.want)
		})
	}
}

func TestFailedHeadRefusal(test *testing.T) {
	test.Parallel()
	for _, testCase := range []struct {
		name     string
		statuses []verify.Status
		refuses  bool
	}{
		{"no statuses", nil, false},
		{"success", []verify.Status{success("ok")}, false},
		{"pending", []verify.Status{{Context: verify.StatusContext, State: "pending"}}, false},
		{"other context failed", []verify.Status{{Context: "ci/other", State: "failure"}, {Context: "ci/other", State: "error"}}, false},
		{"failure", []verify.Status{success("ok"), {Context: verify.StatusContext, State: "failure", CreatedAt: "2026-10-30T09:00:00Z"}}, true},
		{"error", []verify.Status{{Context: verify.StatusContext, State: "error", CreatedAt: "2026-10-30T09:00:00Z"}}, true},
	} {
		test.Run(testCase.name, func(test *testing.T) {
			test.Parallel()
			failed := verify.FailedBefore(testCase.statuses)
			refusal := verify.FailedHeadRefusal(headSHA, testCase.statuses)
			if (failed != nil) != testCase.refuses || (refusal != "") != testCase.refuses {
				test.Fatalf("FailedBefore = %+v, refusal = %q; want refuses %v", failed, refusal, testCase.refuses)
			}
			if testCase.refuses && (!strings.Contains(refusal, "aaaaaaaaaaaa") || !strings.Contains(refusal, failed.CreatedAt) || !strings.Contains(refusal, "attest again")) {
				test.Fatalf("refusal = %q", refusal)
			}
		})
	}
}

func TestClaimsHead(test *testing.T) {
	test.Parallel()
	attested := verified()
	template := verify.WithSection("", "Not attested yet: run what the change needs, then make ci-local.")
	for _, testCase := range []struct {
		name     string
		body     string
		statuses []verify.Status
		want     bool
	}{
		{"the template, no status", template, nil, false},
		{"a section for an older head", verify.WithSection("", strings.ReplaceAll(verify.RenderSection(passingReport()), headSHA, otherSHA)), nil, false},
		{"another context only", template, []verify.Status{{Context: "ci/other", State: "success"}}, false},
		{"a local-ci status on the head", template, []verify.Status{{Context: verify.StatusContext, State: "failure"}}, true},
		{"the section names the head", attested.Body, nil, true},
		{"no section", "", nil, false},
	} {
		test.Run(testCase.name, func(test *testing.T) {
			test.Parallel()
			if got := verify.ClaimsHead(testCase.body, headSHA, testCase.statuses); got != testCase.want {
				test.Fatalf("ClaimsHead = %v, want %v", got, testCase.want)
			}
		})
	}
}

func TestLocalVerificationProblems(test *testing.T) {
	test.Parallel()
	requireNone(test, verify.LocalVerificationProblems(verified()))
	edit := func(change func(*verify.Verification)) verify.Verification {
		verification := verified()
		change(&verification)
		return verification
	}
	for _, testCase := range []struct {
		name         string
		verification verify.Verification
		want         string
	}{
		{"section edited after", edit(func(verification *verify.Verification) {
			verification.Body = strings.Replace(verification.Body, "make lint", "make lint (edited)", 1)
		}), "is not the one local-ci attested"},
		{"status without body digest", edit(func(verification *verify.Verification) {
			status := success("passed")
			verification.Status = &status
		}), "is not the one local-ci attested"},
		{"no status", edit(func(verification *verify.Verification) { verification.Status = nil }), "no local-ci status on aaaaaaaaaaaa"},
		{"status failed", edit(func(verification *verify.Verification) {
			verification.Status = &verify.Status{Context: verify.StatusContext, State: "failure"}
		}), "local-ci is failure on aaaaaaaaaaaa"},
		{"failure in history", edit(func(verification *verify.Verification) {
			verification.Statuses = append(verification.Statuses, verify.Status{Context: verify.StatusContext, State: "failure", CreatedAt: "earlier"})
		}), "local-ci failed on aaaaaaaaaaaa at earlier"},
		{"other head", edit(func(verification *verify.Verification) { verification.Head = otherSHA }), "not the PR head bbbbbbbbbbbb"},
		{"no section", edit(func(verification *verify.Verification) { verification.Body = "## Problem" }), "has no Local verification section"},
		// Randy's review of #134: a fork's author controls its body, so a
		// section and status copied from an attested head must not pass.
		{"fork", edit(func(verification *verify.Verification) { verification.Fork = "someone/go-fast-stack" }), "the head is in someone/go-fast-stack, not this repository"},
		{"deleted fork", edit(func(verification *verify.Verification) { verification.Fork = verify.ForkGone }), "the head is in a deleted fork"},
	} {
		test.Run(testCase.name, func(test *testing.T) {
			test.Parallel()
			requireMention(test, verify.LocalVerificationProblems(testCase.verification), testCase.want)
		})
	}
}

func TestForkOf(test *testing.T) {
	test.Parallel()
	for _, testCase := range []struct{ headRepo, want string }{
		{"acme/app", ""},
		{"someone/app", "someone/app"},
		{"", verify.ForkGone},
	} {
		if got := verify.ForkOf("acme/app", testCase.headRepo); got != testCase.want {
			test.Errorf("ForkOf(acme/app, %q) = %q, want %q", testCase.headRepo, got, testCase.want)
		}
	}
}

// A fork's head is checked under the required name, so it fails red instead
// of waiting as "Expected"; any other head is claimed as ClaimsHead says.
func TestClaimed(test *testing.T) {
	test.Parallel()
	fork := verify.Verification{Body: "", Head: headSHA, Status: nil, Statuses: nil, Fork: "someone/app"}
	if !verify.Claimed(fork) {
		test.Error("a fork's unattested head is not claimed")
	}
	if problems := verify.LocalVerificationProblems(fork); !containsText(problems, "not this repository") {
		test.Errorf("a fork's head passes: %q", problems)
	}
	if verify.Claimed(verify.Verification{Body: "", Head: headSHA, Status: nil, Statuses: nil, Fork: ""}) {
		test.Error("an unattested branch head is claimed")
	}
	if !verify.Claimed(verified()) {
		test.Error("an attested head is not claimed")
	}
}
