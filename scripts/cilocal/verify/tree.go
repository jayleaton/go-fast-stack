package verify

import (
	"fmt"
	"path/filepath"
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
// nil. A path that would leave the scan's directory (absolute, or with a
// `..` element) is a problem, not a file to skip: git never checks one out,
// so only a crafted tree holds one, and the scan fails closed on it.
func ScanTargets(entries []TreeEntry, wanted []string) ([]TreeEntry, string) {
	named := map[string]bool{}
	for _, file := range wanted {
		named[file] = true
	}
	targets := []TreeEntry{}
	for _, entry := range entries {
		if !filepath.IsLocal(filepath.FromSlash(entry.Path)) {
			return nil, fmt.Sprintf("the head's tree has the path %q, which leaves the scan's directory", entry.Path)
		}
		if strings.HasPrefix(entry.Mode, regularMode) && (wanted == nil || named[entry.Path]) {
			targets = append(targets, entry)
		}
	}
	return targets, ""
}
