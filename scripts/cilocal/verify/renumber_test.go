package verify_test

import (
	"slices"
	"strings"
	"testing"

	"github.com/jayleaton/go-fast-stack/scripts/cilocal/verify"
)

// mainMigrations is main's database set as of this change, ending at 00030.
var mainMigrations = []string{
	"packages/database/migrations/00001_init.sql",
	"packages/database/migrations/00025_settlements.sql",
	"packages/database/migrations/00030_bank_rules.sql",
	"packages/auth/migrations/00001_auth.sql",
	"packages/auth/migrations/00004_invitation.sql",
	"README.md",
}

func added(files ...string) []verify.MigrationChange {
	changes := []verify.MigrationChange{}
	for _, file := range files {
		changes = append(changes, verify.MigrationChange{Status: "A", File: file})
	}
	return changes
}

func TestMigrationRenumbers(test *testing.T) {
	test.Parallel()
	const db = "packages/database/migrations/"
	for name, tc := range map[string]struct {
		changes []verify.MigrationChange
		want    []verify.Rename
		problem string
	}{
		"a migration above main's highest keeps its number": {
			changes: added(db + "00031_reports.sql"),
			want:    []verify.Rename{},
		},
		"one taken while the PR waited moves to highest+1": {
			changes: added(db + "00030_bill_payments.sql"),
			want:    []verify.Rename{{From: db + "00030_bill_payments.sql", To: db + "00031_bill_payments.sql"}},
		},
		"in a set that needs it, every added migration moves, in order": {
			changes: added(db+"00029_claims.sql", db+"00031_claim_rules.sql", db+"00028_access.sql"),
			want: []verify.Rename{
				{From: db + "00028_access.sql", To: db + "00031_access.sql"},
				{From: db + "00029_claims.sql", To: db + "00032_claims.sql"},
				{From: db + "00031_claim_rules.sql", To: db + "00033_claim_rules.sql"},
			},
		},
		"each set by its own highest": {
			changes: added(db+"00031_ok.sql", "packages/auth/migrations/00003_sessions.sql"),
			want:    []verify.Rename{{From: "packages/auth/migrations/00003_sessions.sql", To: "packages/auth/migrations/00005_sessions.sql"}},
		},
		"non-migration changes are ignored": {
			changes: []verify.MigrationChange{{Status: "M", File: "README.md"}, {Status: "A", File: db + "notes.txt"}},
			want:    []verify.Rename{},
		},
		"a changed existing migration is refused": {
			changes: append(added(db+"00029_new.sql"), verify.MigrationChange{Status: "M", File: db + "00025_settlements.sql"}),
			problem: "00025_settlements.sql: the PR changes a migration the base already has",
		},
		"a deleted existing migration is refused": {
			changes: []verify.MigrationChange{{Status: "D", File: db + "00030_bank_rules.sql"}},
			problem: "00030_bank_rules.sql: the PR deletes a migration the base already has",
		},
		"two added with one number are refused": {
			changes: added(db+"00031_a.sql", db+"00031_b.sql"),
			problem: "the PR adds two migrations numbered 31, so their order is unknown",
		},
	} {
		test.Run(name, func(test *testing.T) {
			test.Parallel()
			renames, problems := verify.MigrationRenumbers(mainMigrations, tc.changes)
			if tc.problem != "" {
				if renames != nil || !slices.ContainsFunc(problems, func(problem string) bool { return strings.Contains(problem, tc.problem) }) {
					test.Fatalf("renames %v, problems %v; want no renames and a problem containing %q", renames, problems, tc.problem)
				}
				return
			}
			if len(problems) > 0 || !slices.Equal(renames, tc.want) {
				test.Fatalf("renames %v, problems %v; want %v", renames, problems, tc.want)
			}
		})
	}
}

func TestRenumberedMigrationsPassTheOrderCheck(test *testing.T) {
	test.Parallel()
	const db = "packages/database/migrations/"
	changes := added(db+"00029_claims.sql", db+"00030_bill_payments.sql")
	renames, problems := verify.MigrationRenumbers(mainMigrations, changes)
	if len(problems) > 0 {
		test.Fatal(problems)
	}
	tree := slices.Clone(mainMigrations)
	for _, rename := range renames {
		tree = append(tree, rename.To)
	}
	if order := verify.MigrationProblems(mainMigrations, tree, "origin/main"); len(order) > 0 {
		test.Fatalf("renumbered migrations still fail the order check: %v", order)
	}
	if stem := verify.Stem(db + "00031_claims.sql"); stem != "00031_claims" {
		test.Fatalf("Stem = %q", stem)
	}
}
