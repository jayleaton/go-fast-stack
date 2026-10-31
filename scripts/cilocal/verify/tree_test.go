package verify_test

import (
	"slices"
	"strings"
	"testing"

	"github.com/jayleaton/go-fast-stack/scripts/cilocal/verify"
)

func TestParseTree(test *testing.T) {
	test.Parallel()
	listing := "100644 blob " + headSHA + "\tREADME.md\x00" +
		"100755 blob " + otherSHA + "\tscripts/a b.sh\x00" +
		"160000 commit " + strings.Repeat("c", 40) + "\tvendor/sub\x00" +
		"not a tree line\x00"
	want := []verify.TreeEntry{
		{Mode: "100644", Blob: headSHA, Path: "README.md"},
		{Mode: "100755", Blob: otherSHA, Path: "scripts/a b.sh"},
		{Mode: "160000", Blob: strings.Repeat("c", 40), Path: "vendor/sub"},
	}
	if got := verify.ParseTree(listing); !slices.Equal(got, want) {
		test.Fatalf("ParseTree = %+v, want %+v", got, want)
	}
}

// The Secret scan job reads a pull request's head as data: the files GitHub
// lists, or every one, of the head's tree. A fork rewriting .gitleaks.toml
// or scripts/cilocal changes only files that are scanned, never the scanner.
func TestScanTargets(test *testing.T) {
	test.Parallel()
	entries := []verify.TreeEntry{
		{Mode: "100644", Blob: "b1", Path: ".gitleaks.toml"},
		{Mode: "100644", Blob: "b2", Path: "scripts/cilocal/bypass.go"},
		{Mode: "100755", Blob: "b3", Path: "scripts/run.sh"},
		{Mode: "120000", Blob: "b4", Path: "link"},
		{Mode: "160000", Blob: "b5", Path: "vendor/sub"},
		{Mode: "100644", Blob: "b6", Path: "docs/old.md"},
	}
	paths := func(targets []verify.TreeEntry) []string {
		names := []string{}
		for _, target := range targets {
			names = append(names, target.Path)
		}
		return names
	}
	every, problem := verify.ScanTargets(entries, nil)
	if want := []string{".gitleaks.toml", "scripts/cilocal/bypass.go", "scripts/run.sh", "docs/old.md"}; problem != "" || !slices.Equal(paths(every), want) {
		test.Fatalf("ScanTargets(every) = %q, %q; want %q", paths(every), problem, want)
	}
	listed, problem := verify.ScanTargets(entries, []string{".gitleaks.toml", "scripts/cilocal/bypass.go", "link", "deleted.go"})
	if want := []string{".gitleaks.toml", "scripts/cilocal/bypass.go"}; problem != "" || !slices.Equal(paths(listed), want) {
		test.Fatalf("ScanTargets(listed) = %q, %q; want %q", paths(listed), problem, want)
	}
	for _, path := range []string{"../outside", "/etc/passwd", "a/../../b"} {
		crafted := append(slices.Clone(entries), verify.TreeEntry{Mode: "100644", Blob: "b7", Path: path})
		targets, problem := verify.ScanTargets(crafted, nil)
		if targets != nil || !strings.Contains(problem, "leaves the scan's directory") {
			test.Errorf("ScanTargets with %q = %q, %q; want it to fail closed", path, paths(targets), problem)
		}
	}
}
