package verify

import (
	"fmt"
	"path"
	"sort"
	"strconv"
	"strings"
)

// make land renumbers a pull request's new migrations after its rebase,
// inside the CI lock, so a PR that waited while another migration merged
// still lands above main's highest (MigrationProblems, the order check).

// MigrationChange is one line of `git diff --name-status --no-renames
// origin/<base> HEAD`: A added, M modified, D deleted.
type MigrationChange struct {
	Status, File string
}

// Rename moves a migration to a new number, and every exact mention of its
// stem (the file name without .sql, as docs and tests name it) with it.
type Rename struct {
	From, To string
}

// Stem is a migration file's name without its directory and ".sql".
func Stem(file string) string { return strings.TrimSuffix(path.Base(file), ".sql") }

// MigrationRenumbers is how make land renumbers the migrations a pull
// request adds (`changes`, against the base whose tree lists `baseFiles`):
// in each set where any added migration is numbered at or below the base's
// highest, every added one moves, in their order, to highest+1, +2, ... at
// the set's width. A set whose added migrations are all above the highest
// keeps its numbers. Anything ambiguous is a problem and nothing moves: a
// change to or the deletion of a migration the base has (renumbering cannot
// make that safe: a deployed database already applied it), or two added
// migrations with one number in a set (their order is unknown).
func MigrationRenumbers(baseFiles []string, changes []MigrationChange) ([]Rename, []string) {
	highest, width := map[string]int{}, map[string]int{}
	for _, file := range baseFiles {
		if !IsMigration(file) {
			continue
		}
		prefix := versionPrefix(file)
		version, _ := strconv.Atoi(prefix)
		dir := path.Dir(file)
		highest[dir], width[dir] = max(highest[dir], version), len(prefix)
	}
	problems := []string{}
	added := map[string][]string{}
	for _, change := range changes {
		if !IsMigration(change.File) {
			continue
		}
		switch change.Status {
		case "A":
			added[path.Dir(change.File)] = append(added[path.Dir(change.File)], change.File)
		case "M":
			problems = append(problems, fmt.Sprintf("%s: the PR changes a migration the base already has; a deployed database applied it as it was: undo the change and add a new migration instead", change.File))
		case "D":
			problems = append(problems, fmt.Sprintf("%s: the PR deletes a migration the base already has; a deployed database applied it: restore it", change.File))
		default:
			problems = append(problems, fmt.Sprintf("%s: unexpected change %q to a migration: renumber it by hand", change.File, change.Status))
		}
	}
	dirs := make([]string, 0, len(added))
	for dir := range added {
		dirs = append(dirs, dir)
	}
	sort.Strings(dirs)
	renames := []Rename{}
	for _, dir := range dirs {
		files := added[dir]
		sort.Slice(files, func(left, right int) bool {
			leftVersion, _ := strconv.Atoi(versionPrefix(files[left]))
			rightVersion, _ := strconv.Atoi(versionPrefix(files[right]))
			return leftVersion < rightVersion || (leftVersion == rightVersion && files[left] < files[right])
		})
		stale := false
		seen := map[int]string{}
		for _, file := range files {
			version, _ := strconv.Atoi(versionPrefix(file))
			if other, dup := seen[version]; dup {
				problems = append(problems, fmt.Sprintf("%s and %s: the PR adds two migrations numbered %d, so their order is unknown: renumber them by hand", other, file, version))
			}
			seen[version] = file
			stale = stale || version <= highest[dir]
		}
		if !stale {
			continue
		}
		digits := width[dir]
		if digits == 0 {
			digits = len(versionPrefix(files[0]))
		}
		for index, file := range files {
			_, name, _ := strings.Cut(path.Base(file), "_")
			to := path.Join(dir, fmt.Sprintf("%0*d_%s", digits, highest[dir]+1+index, name))
			if to != file {
				renames = append(renames, Rename{From: file, To: to})
			}
		}
	}
	if len(problems) > 0 {
		return nil, problems
	}
	return renames, nil
}
