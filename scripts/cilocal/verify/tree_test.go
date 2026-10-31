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

// Randy's re-review of #134 (2f1cca7): with the base's .gitleaks.toml, a
// synthetic credential failed the scan, and a child commit adding only
// .gitleaksignore with that finding's fingerprint passed it, because
// gitleaks loads .gitleaksignore from the root of the directory it scans.
// Both heads must fail: the first on the finding, the second on the finding
// (its .gitleaksignore is copied in as content, never as policy, and the
// trusted ignore file sits outside the scanned directory) and on the policy
// change it makes.
func TestHeadPolicyIsNeverScannerPolicy(test *testing.T) {
	test.Parallel()
	base := []verify.TreeEntry{{Mode: "100644", Blob: "cfg", Path: ".gitleaks.toml"}, {Mode: "100644", Blob: "go", Path: "main.go"}}
	credential := append(slices.Clone(base), verify.TreeEntry{Mode: "100644", Blob: "leak", Path: "credential.txt"})
	ignored := append(slices.Clone(credential), verify.TreeEntry{Mode: "100644", Blob: "fingerprint", Path: ".gitleaksignore"})

	requireNoPolicyChange := func(head []verify.TreeEntry) {
		test.Helper()
		if changed := verify.PolicyChanges(base, head); len(changed) != 0 || verify.PolicyProblem(changed) != "" {
			test.Fatalf("PolicyChanges = %q, want none", changed)
		}
	}
	requireNoPolicyChange(credential)

	changed := verify.PolicyChanges(base, ignored)
	if !slices.Equal(changed, []string{".gitleaksignore"}) {
		test.Fatalf("PolicyChanges = %q, want [.gitleaksignore]", changed)
	}
	if problem := verify.PolicyProblem(changed); !strings.Contains(problem, "edits the secret scan's policy (.gitleaksignore)") {
		test.Fatalf("PolicyProblem = %q", problem)
	}
	targets, problem := verify.ScanTargets(ignored, nil)
	if problem != "" {
		test.Fatal(problem)
	}
	for _, target := range targets {
		if copied := verify.ScanCopyPath(target.Path); slices.Contains(verify.ScannerPolicy, copied) {
			test.Errorf("%s is copied into the scan as %s, where gitleaks loads it as policy", target.Path, copied)
		}
	}

	edited := slices.Clone(base)
	edited[0].Blob = "allow everything"
	removed := base[1:]
	for name, head := range map[string][]verify.TreeEntry{"edited config": edited, "removed config": removed} {
		if got := verify.PolicyChanges(base, head); !slices.Equal(got, []string{".gitleaks.toml"}) {
			test.Errorf("%s: PolicyChanges = %q, want [.gitleaks.toml]", name, got)
		}
	}
	nested := append(slices.Clone(base), verify.TreeEntry{Mode: "100644", Blob: "x", Path: "docs/.gitleaksignore"})
	requireNoPolicyChange(nested) // gitleaks reads only the root's
}

func TestScanCopyPath(test *testing.T) {
	test.Parallel()
	for path, want := range map[string]string{
		".gitleaksignore":      ".gitleaksignore.scanned",
		".gitleaks.toml":       ".gitleaks.toml.scanned",
		"sub/.gitleaksignore":  "sub/.gitleaksignore.scanned",
		"main.go":              "main.go",
		"docs/gitleaks.toml":   "docs/gitleaks.toml",
		".gitleaksignore.orig": ".gitleaksignore.orig",
	} {
		if got := verify.ScanCopyPath(path); got != want {
			test.Errorf("ScanCopyPath(%q) = %q, want %q", path, got, want)
		}
	}
}

// gitleaks gets the trusted config and ignore file by path and ignores
// inline gitleaks:allow comments, which a change could add to its own lines.
func TestGitleaksArgs(test *testing.T) {
	test.Parallel()
	args := strings.Join(verify.GitleaksArgs("/trusted/.gitleaks.toml", "/trusted/.gitleaksignore"), " ")
	for _, want := range []string{"dir . ", "--config /trusted/.gitleaks.toml", "--gitleaks-ignore-path /trusted/.gitleaksignore", "--ignore-gitleaks-allow", "--redact", "--exit-code 1"} {
		if !strings.Contains(args, want) {
			test.Errorf("GitleaksArgs = %q, want %q", args, want)
		}
	}
}
