package verify_test

import (
	"slices"
	"testing"

	"github.com/jayleaton/go-fast-stack/scripts/cilocal/verify"
)

func TestMigrationProblemsNameTheNumberToRenumberTo(test *testing.T) {
	test.Parallel()
	const app, auth = "packages/database/migrations/", "packages/auth/migrations/"
	base := []string{app + "00001_init.sql", app + "00007_users.sql", auth + "00003_sessions.sql", "README.md"}
	for _, testCase := range []struct {
		name  string
		base  []string
		tree  []string
		wants []string
	}{
		{"nothing added", base, base, []string{}},
		{"above the highest", base, append(slices.Clone(base), app+"00008_more.sql"), []string{}},
		{"a gap is fine", base, append(slices.Clone(base), app+"00020_more.sql"), []string{}},
		{
			"below the highest", base, append(slices.Clone(base), app+"00005_late.sql"),
			[]string{app + "00005_late.sql: version 5 is not above the highest on origin/main (7): renumber to 00008"},
		},
		{
			"equal to the highest", base, append(slices.Clone(base), app+"00007_clash.sql"),
			[]string{app + "00007_clash.sql: version 7 is not above the highest on origin/main (7): renumber to 00008"},
		},
		{
			"two added with one number", base, append(slices.Clone(base), app+"00008_a.sql", app+"00008_b.sql"),
			[]string{app + "00008_b.sql: version 8 is also used by " + app + "00008_a.sql: renumber to 00009"},
		},
		{
			"each misnumbered one gets its own next number", base, append(slices.Clone(base), app+"00002_a.sql", app+"00003_b.sql"),
			[]string{
				app + "00002_a.sql: version 2 is not above the highest on origin/main (7): renumber to 00008",
				app + "00003_b.sql: version 3 is not above the highest on origin/main (7): renumber to 00009",
			},
		},
		{"sets are separate", base, append(slices.Clone(base), auth+"00004_next.sql"), []string{}},
		{
			"a new set keeps its own width", nil, []string{"apps/x/migrations/001_a.sql", "apps/x/migrations/001_b.sql"},
			[]string{"apps/x/migrations/001_b.sql: version 1 is also used by apps/x/migrations/001_a.sql: renumber to 002"},
		},
		{"not a numbered migration", base, append(slices.Clone(base), app+"notes.sql", "docs/migrations/0001_x.md"), []string{}},
	} {
		test.Run(testCase.name, func(test *testing.T) {
			test.Parallel()
			got := verify.MigrationProblems(testCase.base, testCase.tree, "origin/main")
			if !slices.Equal(got, testCase.wants) {
				test.Errorf("problems = %q, want %q", got, testCase.wants)
			}
		})
	}
}
