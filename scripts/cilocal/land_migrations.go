package main

import (
	"bytes"
	"context"
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/jayleaton/go-fast-stack/scripts/cilocal/verify"
)

// renumberMessage is the commit make land makes for it.
const renumberMessage = "chore: renumber migration"

// landMigrationsCommand is `cilocal land-migrations [--dry-run] <base>`,
// make land's step after its rebase onto origin/<base>, inside the CI lock
// (verify.MigrationRenumbers): it renumbers the migrations this branch adds
// above origin/<base>'s highest, rewrites every exact mention of their old
// stems in tracked text files, and commits that as "chore: renumber
// migration". Nothing to do prints so and commits nothing; anything
// ambiguous (a changed or deleted existing migration, two added with one
// number) is refused with nothing changed. Run in a clean checkout.
func landMigrationsCommand(ctx context.Context, args []string) error {
	flags := flag.NewFlagSet("land-migrations", flag.ContinueOnError)
	dryRun := flags.Bool("dry-run", false, "say what it would rename, change nothing")
	if err := flags.Parse(args); err != nil {
		return usageError{message: err.Error()}
	}
	if flags.NArg() != 1 {
		return usageError{message: "usage: cilocal land-migrations [--dry-run] <base branch>"}
	}
	base := "origin/" + flags.Arg(0)
	listed, err := git(ctx, "", "ls-tree", "-r", "--name-only", base)
	if err != nil {
		return err
	}
	diff, err := git(ctx, "", "diff", "--name-status", "--no-renames", base, "HEAD")
	if err != nil {
		return err
	}
	changes := []verify.MigrationChange{}
	for _, line := range strings.Split(diff, "\n") {
		status, file, found := strings.Cut(line, "\t")
		if found {
			changes = append(changes, verify.MigrationChange{Status: status, File: file})
		}
	}
	renames, problems := verify.MigrationRenumbers(strings.Split(listed, "\n"), changes)
	if len(problems) > 0 {
		return usageError{message: "cannot renumber this branch's migrations, so make land stops here:\n  " + strings.Join(problems, "\n  ")}
	}
	if len(renames) == 0 {
		fmt.Println("migrations: nothing to renumber")
		return nil
	}
	for _, rename := range renames {
		fmt.Printf("migrations: %s -> %s\n", rename.From, rename.To)
	}
	if *dryRun {
		return nil
	}
	if err := applyRenames(ctx, renames); err != nil {
		return err
	}
	body := []string{}
	for _, rename := range renames {
		body = append(body, fmt.Sprintf("%s -> %s", verify.Stem(rename.From), verify.Stem(rename.To)))
	}
	message := renumberMessage + "\n\nAbove " + base + "'s highest, after make land's rebase:\n" + strings.Join(body, "\n")
	if _, err := git(ctx, "", "commit", "-q", "-m", message); err != nil {
		return err
	}
	head, err := git(ctx, "", "rev-parse", "--short", "HEAD")
	if err != nil {
		return err
	}
	fmt.Printf("migrations: committed %s %q\n", head, renumberMessage)
	return nil
}

// applyRenames moves the files and rewrites the old stems in every tracked
// text file, each in two steps through a temporary name, so one rename's
// target can be another's source (00030_x -> 00031_x while 00031_x -> 00032_x).
func applyRenames(ctx context.Context, renames []verify.Rename) error {
	temporary := func(index int) string { return fmt.Sprintf("cilocal-renumber-%d-%d", os.Getpid(), index) }
	for index, rename := range renames {
		if _, err := git(ctx, "", "mv", rename.From, rename.From+"."+temporary(index)); err != nil {
			return err
		}
	}
	for index, rename := range renames {
		if _, err := git(ctx, "", "mv", rename.From+"."+temporary(index), rename.To); err != nil {
			return err
		}
	}
	mentioned := map[string]bool{}
	for _, rename := range renames {
		files, err := git(ctx, "", "grep", "-l", "-I", "-F", verify.Stem(rename.From))
		if err != nil && !strings.Contains(err.Error(), "exit status 1") { // 1: no match
			return err
		}
		for _, file := range strings.Fields(files) {
			mentioned[file] = true
		}
	}
	for file := range mentioned {
		content, err := os.ReadFile(file)
		if err != nil {
			return err
		}
		updated := content
		for index, rename := range renames {
			updated = bytes.ReplaceAll(updated, []byte(verify.Stem(rename.From)), []byte(temporary(index)))
		}
		for index, rename := range renames {
			updated = bytes.ReplaceAll(updated, []byte(temporary(index)), []byte(verify.Stem(rename.To)))
		}
		if bytes.Equal(updated, content) {
			continue
		}
		info, err := os.Stat(file)
		if err != nil {
			return err
		}
		if err := os.WriteFile(file, updated, info.Mode().Perm()); err != nil {
			return err
		}
		if _, err := git(ctx, "", "add", "--", file); err != nil {
			return err
		}
		fmt.Printf("migrations: updated the mentions in %s\n", file)
	}
	return nil
}
