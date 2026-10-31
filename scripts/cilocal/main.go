// Command cilocal is local verification (docs/CI.md, "Local verification"):
// a pull request's checks run on the agent's machine, on the exact pushed
// head, and are recorded on GitHub as the `local-ci` commit status and the
// PR body's "Local verification" section. The rules live in
// scripts/cilocal/verify; this wires them to git, gh, make and Davit.
//
//	cilocal run [flags]          make ci-local: select, run, post (flags: run -h)
//	cilocal check                ci.yml's Local verification job (REPO, PR, HEAD_SHA)
//	cilocal merge <pr>...        make pr-merge: squash-merge verified PRs (--dry-run, --require-actions)
//	cilocal attest <sha>         the staging gate (Deploy staging, Staging catch-up): was this main commit attested?
//	cilocal secret-scan ...      gitleaks on changed files (secret-scan -h)
//	cilocal migration-order ...  scripts/check-migration-order.sh
//	cilocal exec                 the executor inside a Davit box
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"

	"github.com/jayleaton/go-fast-stack/scripts/cilocal/verify"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	args := os.Args[1:]
	command := "run"
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		command, args = args[0], args[1:]
	}
	commands := map[string]func(context.Context, []string) error{
		"run": runCommand, "check": checkCommand, "merge": mergeCommand, "attest": attestCommand,
		"secret-scan": secretScan, "exec": execCommand, "migration-order": migrationOrder,
	}
	run, known := commands[command]
	if !known {
		fmt.Fprintf(os.Stderr, "cilocal: unknown command %q (run, check, merge, attest, secret-scan, migration-order, exec)\n", command)
		stop()
		const exitUsage = 2
		os.Exit(exitUsage)
	}
	err := run(ctx, args)
	stop()
	if err != nil {
		if !errors.Is(err, errNotVerified) {
			fmt.Fprintf(os.Stderr, "cilocal %s: %v\n", command, err)
		}
		os.Exit(1)
	}
}

// checkCommand is ci.yml's Local verification job: it fails a pull request
// whose body section or `local-ci` status does not attest its head. It reads
// the body and statuses live from the API, not the event payload, so a
// re-run after make ci-local sees what it posted.
func checkCommand(ctx context.Context, _ []string) error {
	repo, number, head := os.Getenv("REPO"), os.Getenv("PR"), os.Getenv("HEAD_SHA")
	if repo == "" || number == "" || head == "" {
		return errors.New("check needs REPO, PR and HEAD_SHA")
	}
	hub := github{repo: repo}
	var pull struct {
		Body string `json:"body"`
	}
	if err := hub.api(ctx, "pulls/"+number, &pull); err != nil {
		return err
	}
	verification, err := hub.verification(ctx, pull.Body, head)
	if err != nil {
		return err
	}
	if problems := verification.Problems(); len(problems) > 0 {
		for _, problem := range problems {
			fmt.Printf("::error::%s\n", problem)
		}
		fmt.Printf("::error::Not verified locally: %s.\n", verify.NextStep)
		return errNotVerified
	}
	fmt.Printf("%s: make ci-local passed on this head (%s)\n", head, verification.Status.Description)
	return nil
}

// mergeCommand is make pr-merge: it squash-merges each listed pull request
// whose local verification holds for its exact head (verify.MergeProblems),
// and reports the rest with the next step, without waiting on any. Before a
// merge it retargets the open pull requests stacked on its branch to its
// base, so the stack survives.
func mergeCommand(ctx context.Context, args []string) error {
	flags := flag.NewFlagSet("merge", flag.ContinueOnError)
	dryRun := flags.Bool("dry-run", false, "say what it would do")
	requireActions := flags.Bool("require-actions", false, "refuse when no GitHub CI run checked the head")
	if err := flags.Parse(args); err != nil {
		return fmt.Errorf("merge: %w", err)
	}
	if flags.NArg() == 0 {
		return errors.New("usage: make pr-merge PR=\"<number>...\"")
	}
	hub, err := newGitHub(ctx)
	if err != nil {
		return err
	}
	failed := false
	for _, arg := range flags.Args() {
		number, err := strconv.Atoi(strings.TrimPrefix(arg, "#"))
		if err != nil {
			return fmt.Errorf("merge: %q is not a pull request number", arg)
		}
		if err := mergeOne(ctx, hub, number, *dryRun, *requireActions); err != nil {
			fmt.Printf("#%d: %v\n", number, err)
			failed = true
		}
	}
	if failed {
		return errNotVerified
	}
	return nil
}

func mergeOne(ctx context.Context, hub github, number int, dryRun, requireActions bool) error {
	pull, err := hub.pull(ctx, number)
	if err != nil {
		return err
	}
	if pull.State == "MERGED" {
		fmt.Printf("#%d: already merged\n", number)
		return nil
	}
	verification, err := hub.verification(ctx, pull.Body, pull.HeadRefOid)
	if err != nil {
		return err
	}
	problems, note := verify.MergeProblems(verify.Pull{State: pull.State, Draft: pull.IsDraft, Mergeable: pull.Mergeable, Checks: pull.Checks, Verification: verification}, requireActions)
	if len(problems) > 0 {
		next := "" // every other problem names its own fix
		if len(verification.Problems()) > 0 {
			next = "\n  next: " + verify.NextStep
		}
		return fmt.Errorf("not merging %s:\n  - %s%s", verify.Short(pull.HeadRefOid), strings.Join(problems, "\n  - "), next)
	}
	fmt.Printf("#%d: verified on %s (%s)\n", number, verify.Short(pull.HeadRefOid), note)
	if dryRun {
		fmt.Printf("#%d: would merge\n", number)
		return nil
	}
	if err := retargetStack(ctx, hub, number, pull.BaseRefName); err != nil {
		return err
	}
	if _, err := output(ctx, "", "gh", "pr", "merge", strconv.Itoa(number), "-R", hub.repo, "--squash", "--match-head-commit", pull.HeadRefOid); err != nil {
		return err
	}
	fmt.Printf("#%d: merged\n", number)
	return nil
}

func retargetStack(ctx context.Context, hub github, number int, base string) error {
	branch, err := output(ctx, "", "gh", "pr", "view", strconv.Itoa(number), "-R", hub.repo, "--json", "headRefName", "-q", ".headRefName")
	if err != nil {
		return err
	}
	stacked, err := output(ctx, "", "gh", "pr", "list", "-R", hub.repo, "--base", branch, "--state", "open", "--json", "number", "-q", ".[].number")
	if err != nil {
		return err
	}
	for _, child := range strings.Fields(stacked) {
		if _, err := output(ctx, "", "gh", "pr", "edit", child, "-R", hub.repo, "--base", base); err != nil {
			return err
		}
		fmt.Printf("#%d: retargeted #%s, stacked on it, to %s\n", number, child, base)
	}
	return nil
}

// attestCommand is the staging gate (deploy-staging.yml's plan, and
// staging-catch-up.yml before it dispatches): it passes a commit of main only
// when make ci-local attested it, on the commit itself or on the exact head of
// the pull request whose merge made it.
func attestCommand(ctx context.Context, args []string) error {
	if len(args) != 1 {
		return errors.New("usage: cilocal attest <sha>")
	}
	hub, err := newGitHub(ctx)
	if err != nil {
		return err
	}
	sha := args[0]
	by, reasons, err := hub.attested(ctx, sha)
	if err != nil {
		return err
	}
	if by != "" {
		fmt.Printf("%s is attested: %s\n", sha, by)
		return nil
	}
	for _, reason := range reasons {
		fmt.Printf("::error::%s\n", reason)
	}
	fmt.Printf("::error::Not deploying %s: run make ci-local on the merged pull request's head (or, for a direct push, make ci-local ARGS=\"--commit %s\" in a clean worktree of it: git worktree add --detach <dir> %s), then re-run or dispatch Deploy staging on main (Staging catch-up also dispatches it within the hour).\n",
		sha, sha, verify.Short(sha))
	return errNotVerified
}
