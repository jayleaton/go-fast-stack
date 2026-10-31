package verify_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jayleaton/go-fast-stack/scripts/cilocal/verify"
)

// The PR template holds the section make ci-local fills, between its markers,
// with a placeholder that claims no head: a new PR's Local verification job
// then runs as "Awaiting make ci-local" (ClaimsHead) and its body problems
// say how to attest.
func TestTemplateHoldsAnUnattestedSection(test *testing.T) {
	test.Parallel()
	template, err := os.ReadFile(filepath.Join("..", "..", "..", ".github", "pull_request_template.md"))
	if err != nil {
		test.Fatal(err)
	}
	body := string(template)
	section, found := verify.SectionOf(body)
	if !found || strings.Count(body, verify.SectionStart) != 1 || strings.Count(body, verify.SectionEnd) != 1 {
		test.Fatal("the PR template has no single Local verification section between its markers")
	}
	if !strings.Contains(section, `make ci-local RAN=`) {
		test.Errorf("the template's placeholder %q does not say how to attest", section)
	}
	if verify.ClaimsHead(body, headSHA, nil) {
		test.Error("the template's section claims a head")
	}
	requireMention(test, verify.BodyProblems(body, headSHA), "names no head")
}
