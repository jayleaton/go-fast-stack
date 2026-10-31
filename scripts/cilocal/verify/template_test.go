package verify_test

import (
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"testing"

	"github.com/jayleaton/go-fast-stack/scripts/cilocal/verify"
)

// The PR template lists the checklist make ci-local fills; a new item in
// verify.Items that the template lacks would leave every new PR's section
// short until its first run, and the other way round an item the checks
// never tick.
func TestTemplateListsEveryItem(test *testing.T) {
	test.Parallel()
	template, err := os.ReadFile(filepath.Join("..", "..", "..", ".github", "pull_request_template.md"))
	if err != nil {
		test.Fatal(err)
	}
	section, found := verify.SectionOf(string(template))
	if !found {
		test.Fatal("the PR template has no Local verification section")
	}
	listed := []string{}
	for _, match := range regexp.MustCompile(`(?m)^- \[ \] \*\*([\w-]+)\*\*$`).FindAllStringSubmatch(section, -1) {
		listed = append(listed, match[1])
	}
	if !slices.Equal(listed, verify.Items) {
		test.Errorf("the template lists %v, verify.Items is %v", listed, verify.Items)
	}
}
