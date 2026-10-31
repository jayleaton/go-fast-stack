package verify_test

import (
	"slices"
	"strings"
	"testing"

	"github.com/jayleaton/go-fast-stack/scripts/cilocal/verify"
)

// Commit shas as GitHub reports them: 40 lowercase hex characters.
var (
	headSHA  = strings.Repeat("a", 40)
	otherSHA = strings.Repeat("b", 40)
)

func success(description string) verify.Status {
	return verify.Status{Context: verify.StatusContext, State: "success", Description: description, CreatedAt: "2026-10-31T10:00:00Z"}
}

// passingReport is an attestation of headSHA.
func passingReport() verify.Report {
	return verify.Report{
		Head:       headSHA,
		Tree:       strings.Repeat("d", 40),
		Author:     "JJ Eaton",
		Hostname:   "laptop",
		FinishedAt: "2026-10-31T10:00:00Z",
		Result:     verify.Passed,
		Ran:        "make lint test TEST_PKGS=./scripts/cilocal/verify",
		Failed:     "",
	}
}

// verified is a pull request whose head headSHA was attested and whose body
// and status hold for it.
func verified() verify.Verification {
	report := passingReport()
	section := verify.RenderSection(report)
	status := success(verify.StatusDescription(report, section))
	return verify.Verification{
		Body:     verify.WithSection("## Problem\n", section),
		Head:     headSHA,
		Status:   &status,
		Statuses: []verify.Status{status},
	}
}

func containsText(list []string, text string) bool {
	return slices.ContainsFunc(list, func(entry string) bool { return strings.Contains(entry, text) })
}

func requireNone(test *testing.T, problems []string) {
	test.Helper()
	if len(problems) != 0 {
		test.Fatalf("want no problems, got %q", problems)
	}
}

func requireMention(test *testing.T, problems []string, text string) {
	test.Helper()
	if !containsText(problems, text) {
		test.Fatalf("want a problem mentioning %q, got %q", text, problems)
	}
}
