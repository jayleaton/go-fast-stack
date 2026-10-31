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
	oldSHA   = strings.Repeat("c", 40)
)

func exitCode(code int) *int { return &code }

func success(description string) verify.Status {
	return verify.Status{Context: verify.StatusContext, State: "success", Description: description, CreatedAt: "2026-10-31T10:00:00Z"}
}

// passingReport is a run on headSHA where every selected step passed.
func passingReport() verify.Report {
	return verify.Report{
		Head:         headSHA,
		Tree:         strings.Repeat("d", 40),
		MergeBase:    strings.Repeat("e", 40),
		Base:         "origin/main",
		Host:         "laptop",
		Versions:     map[string]string{"go": "go1.26.10", "docker": "28.0"},
		Areas:        []string{"go"},
		ChangedFiles: 2,
		Steps: []verify.StepResult{
			{Step: verify.Step{Item: "secrets", Job: verify.JobSecrets, Name: "gitleaks"}, Exit: exitCode(0), DurationMS: 900},
			{Step: verify.Step{Item: "build", Job: verify.JobGo, Name: "go build"}, InputHash: "1111111111111111", Exit: exitCode(0), DurationMS: 4000},
			{Step: verify.Step{Item: "tests", Job: verify.JobGo, Name: "make test"}, InputHash: "2222222222222222", Exit: exitCode(0), DurationMS: 62000},
		},
		FinishedAt: "2026-10-31T10:00:00Z",
		DurationMS: 67000,
		Result:     verify.Passed,
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
