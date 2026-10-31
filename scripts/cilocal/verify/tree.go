package verify

import (
	"fmt"
	pathpkg "path"
	"path/filepath"
	"slices"
	"strings"
)

// The Secret scan job reads a pull request's head as data: the files of its
// tree, from git's objects, never a checkout of it (docs/CI.md, "Secret
// scan"). The code that scans, and its .gitleaks.toml, are the base's.

// TreeEntry is one entry of `git ls-tree -r -z <commit>`.
type TreeEntry struct {
	Mode, Blob, Path string
}

// ParseTree reads `git ls-tree -r -z` output.
func ParseTree(output string) []TreeEntry {
	entries := []TreeEntry{}
	for line := range strings.SplitSeq(output, "\x00") {
		meta, file, found := strings.Cut(line, "\t")
		fields := strings.Fields(meta)
		const lsFields = 3 // mode, type, object
		if !found || len(fields) != lsFields {
			continue
		}
		entries = append(entries, TreeEntry{Mode: fields[0], Blob: fields[2], Path: file})
	}
	return entries
}

// regularMode is the mode prefix of a file with content (100644, 100755);
// symlinks (120000) and submodules (160000) have none of their own.
const regularMode = "100"

// ScanTargets is the files of a tree the secret scan copies out: the regular
// files among `entries` named in `wanted`, or all of them when `wanted` is
// nil, less templ's generated Go (GeneratedTempl). A path that would leave
// the scan's directory (absolute, or with a `..` element) is a problem, not
// a file to skip: git never checks one out, so only a crafted tree holds
// one, and the scan fails closed on it.
func ScanTargets(entries []TreeEntry, wanted []string) ([]TreeEntry, string) {
	named := map[string]bool{}
	for _, file := range wanted {
		named[file] = true
	}
	tree := map[string]bool{}
	for _, entry := range entries {
		tree[entry.Path] = true
	}
	has := func(path string) bool { return tree[path] }
	targets := []TreeEntry{}
	for _, entry := range entries {
		if !filepath.IsLocal(filepath.FromSlash(entry.Path)) {
			return nil, fmt.Sprintf("the head's tree has the path %q, which leaves the scan's directory", entry.Path)
		}
		if strings.HasPrefix(entry.Mode, regularMode) && (wanted == nil || named[entry.Path]) && !GeneratedTempl(entry.Path, has) {
			targets = append(targets, entry)
		}
	}
	return targets, ""
}

// templOutput is the suffix templ gives the Go it generates from x.templ.
const templOutput = "_templ.go"

// GeneratedTempl is whether the secret scan skips `path`: templ's generated
// Go (`x_templ.go`) for a `x.templ` the same tree holds (`has`). Its strings
// are the source's, which the scan reads, and its generated identifiers
// (`apiclient.X:` then `templ_7745c5c3_Err`) read to gitleaks as API keys. A
// `_templ.go` with no `.templ` beside it is scanned like any file, and
// make check-generated fails one that is not what templ generates.
func GeneratedTempl(path string, has func(string) bool) bool {
	source, found := strings.CutSuffix(path, templOutput)
	return found && has(source+".templ")
}

// ScannerPolicy are the files gitleaks reads as policy rather than content:
// its config, and the ignore list it loads from the root of the directory it
// scans whatever --gitleaks-ignore-path says (gitleaks 8.30.1). The scan
// takes them from the trusted side only, written outside the scanned
// directory, never from the change it scans.
var ScannerPolicy = []string{".gitleaks.toml", ".gitleaksignore"}

// scannedSuffix marks a policy-named file copied in as content.
const scannedSuffix = ".scanned"

// ScanCopyPath is where the scan copies a file of the change: a file named
// like scanner policy, at any depth, gets scannedSuffix, so gitleaks scans
// it as content and never loads it as policy.
func ScanCopyPath(path string) string {
	if slices.Contains(ScannerPolicy, pathpkg.Base(path)) {
		return path + scannedSuffix
	}
	return path
}

// PolicyChanges is the scanner policy files (at the tree's root, where
// gitleaks looks) that differ between the trusted tree and the scanned one:
// added, removed or changed.
func PolicyChanges(trusted, scanned []TreeEntry) []string {
	blobs := func(entries []TreeEntry) map[string]string {
		found := map[string]string{}
		for _, entry := range entries {
			if slices.Contains(ScannerPolicy, entry.Path) {
				found[entry.Path] = entry.Blob
			}
		}
		return found
	}
	before, after := blobs(trusted), blobs(scanned)
	changed := []string{}
	for _, name := range ScannerPolicy {
		if before[name] != after[name] {
			changed = append(changed, name)
		}
	}
	return changed
}

// PolicyProblem is why a scan of a change that edits scanner policy fails
// even when it finds nothing: the scan ran with the trusted policy, so the
// new one is unchecked until someone reviews it.
func PolicyProblem(changed []string) string {
	if len(changed) == 0 {
		return ""
	}
	return fmt.Sprintf("the change edits the secret scan's policy (%s), which the scan never takes from the change it scans: it ran with the trusted base's. A maintainer reviews the policy change and merges it on its own (docs/CI.md, \"Secret scan\")",
		strings.Join(changed, ", "))
}

// GitleaksArgs is the gitleaks command line for a scan of the current
// directory: the trusted `config` and `ignore` file (outside the scanned
// directory), and inline `gitleaks:allow` comments ignored, since they are
// content of the change too.
func GitleaksArgs(config, ignore string) []string {
	return []string{"dir", ".", "--config", config, "--gitleaks-ignore-path", ignore, "--ignore-gitleaks-allow",
		"--redact", "--no-banner", "--no-color", "--exit-code", "1", "--verbose"}
}
