package verify

import (
	"fmt"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// The migration order (AGENTS.md "Migrations"; scripts/check-migration-order.sh
// runs `cilocal migration-order`): a goose migration a checkout adds must be
// numbered above the highest version every deployed base already has in the
// same set. Goose refuses out-of-order migrations, so once a deployed
// database has applied 00007, a later-merged 00005 stops `migrate` (and
// every deploy) there. A set is a directory named migrations holding
// NNNNN_*.sql files (one goose version table each).

var migrationVersion = regexp.MustCompile(`(^|/)migrations/[0-9]+_[^/]*\.sql$`)

// IsMigration is whether `file` is a numbered goose migration.
func IsMigration(file string) bool { return migrationVersion.MatchString(file) }

func versionPrefix(file string) string {
	prefix, _, _ := strings.Cut(path.Base(file), "_")
	return prefix
}

type migrationSet struct {
	highest, width, nextFree int
}

// MigrationProblems is one line per misnumbered migration, naming the number
// to renumber it to: `baseFiles` are the migrations on the deployed bases
// (their tips, not the merge base, so a branch a base has moved past fails
// too), `treeFiles` those in the checkout, `label` names the bases.
func MigrationProblems(baseFiles, treeFiles []string, label string) []string {
	sets := map[string]*migrationSet{}
	setOf := func(file string) *migrationSet {
		dir := path.Dir(file)
		if sets[dir] == nil {
			sets[dir] = &migrationSet{highest: 0, width: 0, nextFree: 0}
		}
		return sets[dir]
	}
	onBase := map[string]bool{}
	for _, file := range baseFiles {
		if !IsMigration(file) {
			continue
		}
		prefix := versionPrefix(file)
		version, _ := strconv.Atoi(prefix)
		set := setOf(file)
		onBase[file], set.width = true, len(prefix)
		set.highest = max(set.highest, version)
	}
	added := []string{}
	for _, file := range treeFiles {
		if !IsMigration(file) || onBase[file] {
			continue
		}
		added = append(added, file)
	}
	sort.Strings(added)
	for _, file := range added {
		prefix := versionPrefix(file)
		version, _ := strconv.Atoi(prefix)
		set := setOf(file)
		if set.width == 0 {
			set.width = len(prefix)
		}
		set.nextFree = max(set.nextFree, set.highest, version)
	}
	for _, set := range sets {
		set.nextFree++
	}
	problems := []string{}
	firstWith := map[string]string{}
	for _, file := range added {
		prefix := versionPrefix(file)
		version, _ := strconv.Atoi(prefix)
		set := setOf(file)
		key := fmt.Sprintf("%s/%d", path.Dir(file), version)
		switch {
		case version <= set.highest:
			problems = append(problems, fmt.Sprintf("%s: version %d is not above the highest on %s (%d): renumber to %0*d", file, version, label, set.highest, set.width, set.nextFree))
		case firstWith[key] != "":
			problems = append(problems, fmt.Sprintf("%s: version %d is also used by %s: renumber to %0*d", file, version, firstWith[key], set.width, set.nextFree))
		default:
			firstWith[key] = file
			continue
		}
		set.nextFree++
	}
	return problems
}
