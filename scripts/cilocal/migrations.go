package main

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/jayleaton/go-fast-stack/scripts/cilocal/verify"
)

// exitUnknownBase is check-migration-order.sh's exit code for a base git
// does not know (1 is a misnumbered migration).
const exitUnknownBase = 2

// migrationOrder is `cilocal migration-order [BASE...]`
// (scripts/check-migration-order.sh): each BASE is a ref whose migrations
// count as deployed, by default the deployed branches origin has
// (origin/dev and origin/main). "Added" means a migration in the working
// tree (committed, staged or untracked) that no BASE has.
func migrationOrder(ctx context.Context, args []string) error {
	bases := args
	if len(bases) == 0 {
		for _, branch := range []string{"dev", "main"} { // scripts/branches.sh, staging first
			if _, err := git(ctx, "", "rev-parse", "--verify", "--quiet", "refs/remotes/origin/"+branch); err == nil {
				bases = append(bases, "origin/"+branch)
			}
		}
	}
	if len(bases) == 0 {
		fmt.Fprintln(os.Stderr, "check-migration-order: none of origin/dev origin/main fetched to compare with (git fetch origin, or pass a ref)")
		os.Exit(exitUnknownBase)
	}
	baseFiles := []string{}
	for _, base := range bases {
		if _, err := git(ctx, "", "rev-parse", "--verify", "--quiet", base+"^{commit}"); err != nil {
			fmt.Fprintf(os.Stderr, "check-migration-order: unknown base %s (git fetch origin, or pass a ref)\n", base)
			os.Exit(exitUnknownBase)
		}
		listed, err := git(ctx, "", "ls-tree", "-r", "--name-only", base)
		if err != nil {
			return err
		}
		baseFiles = append(baseFiles, strings.Split(listed, "\n")...)
	}
	listed, err := git(ctx, "", "ls-files", "--cached", "--others", "--exclude-standard")
	if err != nil {
		return err
	}
	treeFiles := []string{}
	for _, file := range strings.Split(listed, "\n") {
		if _, err := os.Stat(file); err == nil { // deleted in the working tree
			treeFiles = append(treeFiles, file)
		}
	}
	label := strings.Join(bases, " or ")
	problems := verify.MigrationProblems(baseFiles, treeFiles, label)
	for _, problem := range problems {
		fmt.Fprintln(os.Stderr, problem)
	}
	if len(problems) > 0 {
		fmt.Fprintf(os.Stderr, "check-migration-order: number a new migration above every one on %s, per set (AGENTS.md \"Migrations\")\n", label)
		return errNotVerified
	}
	return nil
}
