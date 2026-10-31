// Command cilocal is local verification (docs/CI.md, "Local verification"):
// the agent runs what its change needs on the exact pushed head, then
// attests it, recorded on GitHub as the `local-ci` commit status and the PR
// body's "Local verification" section. The rules live in
// scripts/cilocal/verify; this wires them to git, gh and HTTP.
//
//	cilocal run --ran "<what you ran>"   make ci-local: attest the pushed head (runs nothing; run -h)
//	cilocal check [--claim]             ci.yml's Local verification jobs (REPO, PR, HEAD_SHA, CLAIMED)
//	cilocal merge <pr>...               make pr-merge: squash-merge attested PRs, check staging after
//	cilocal attest <sha>                the staging gate (Deploy staging, Staging catch-up)
//	cilocal secret-scan ...             gitleaks on changed files (secret-scan -h)
//	cilocal migration-order ...         scripts/check-migration-order.sh
package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/jayleaton/go-fast-stack/scripts/cilocal/verify"
)

// exitUsage is the exit code of a usage error or a refusal to attest.
const exitUsage = 2

// usageError is a wrong call or a refusal: it exits 2 and prints only itself.
type usageError struct{ message string }

func (usage usageError) Error() string { return usage.message }

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	args := os.Args[1:]
	command := "run"
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		command, args = args[0], args[1:]
	}
	commands := map[string]func(context.Context, []string) error{
		"run": runCommand, "check": checkCommand, "merge": mergeCommand, "attest": attestCommand,
		"secret-scan": secretScan, "migration-order": migrationOrder,
	}
	run, known := commands[command]
	if !known {
		fmt.Fprintf(os.Stderr, "cilocal: unknown command %q (run, check, merge, attest, secret-scan, migration-order)\n", command)
		stop()
		os.Exit(exitUsage)
	}
	err := run(ctx, args)
	stop()
	var usage usageError
	switch {
	case err == nil:
	case errors.As(err, &usage):
		fmt.Fprintf(os.Stderr, "cilocal %s: %s\n", command, usage.message)
		os.Exit(exitUsage)
	case errors.Is(err, errNotVerified):
		os.Exit(1)
	default:
		fmt.Fprintf(os.Stderr, "cilocal %s: %v\n", command, err)
		os.Exit(1)
	}
}

// checkCommand is ci.yml's two Local verification jobs. It reads the PR body
// and the head's statuses live from the API, not the event payload, so a
// re-run after make ci-local sees what it posted, and runs from the base's
// checkout (pull_request_target), so the head is only data. HEAD_REPO is the
// head's repository: a fork's head fails (verify.ForkOf). With --claim it
// writes claimed=true|false to $GITHUB_OUTPUT (verify.Claimed): ci.yml names the
// checking job "Local verification", the required check, only for a claimed
// head. Run with CLAIMED other than true, it says what to run and passes
// under its other name, so an unattested head's required check stays
// "Expected", never red.
func checkCommand(ctx context.Context, args []string) error {
	claim := len(args) == 1 && args[0] == "--claim"
	if len(args) > 0 && !claim {
		return usageError{"usage: cilocal check [--claim] (env REPO, PR, HEAD_SHA, HEAD_REPO; CLAIMED, GITHUB_OUTPUT)"}
	}
	repo, number, head := os.Getenv("REPO"), os.Getenv("PR"), os.Getenv("HEAD_SHA")
	if repo == "" || number == "" || head == "" {
		return errors.New("check needs REPO, PR and HEAD_SHA")
	}
	hub := github{repo: repo}
	fork := verify.ForkOf(repo, os.Getenv("HEAD_REPO"))
	var pull struct {
		Body string `json:"body"`
	}
	if err := hub.api(ctx, "pulls/"+number, &pull); err != nil {
		return err
	}
	if claim {
		statuses, err := hub.statuses(ctx, head)
		if err != nil {
			return err
		}
		return writeClaim(head, verify.Claimed(verify.Verification{Body: pull.Body, Head: head, Statuses: statuses, Fork: fork}))
	}
	if os.Getenv("CLAIMED") != "true" {
		fmt.Printf("::notice::Awaiting make ci-local on this head: %s.\n", verify.NextStep)
		return nil
	}
	verification, err := hub.verification(ctx, pull.Body, head, fork)
	if err != nil {
		return err
	}
	if problems := verify.LocalVerificationProblems(verification); len(problems) > 0 {
		for _, problem := range problems {
			fmt.Printf("::error::%s\n", problem)
		}
		fmt.Printf("::error::Not verified locally: %s.\n", verify.NextStep)
		return errNotVerified
	}
	fmt.Printf("%s: make ci-local attested this head (%s)\n", head, verification.Status.Description)
	return nil
}

func writeClaim(head string, claimed bool) error {
	path := os.Getenv("GITHUB_OUTPUT")
	if path == "" {
		return errors.New("check --claim needs GITHUB_OUTPUT")
	}
	file, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, fileMode)
	if err != nil {
		return fmt.Errorf("GITHUB_OUTPUT: %w", err)
	}
	if _, err := fmt.Fprintf(file, "claimed=%t\n", claimed); err != nil {
		_ = file.Close()
		return fmt.Errorf("GITHUB_OUTPUT: %w", err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("GITHUB_OUTPUT: %w", err)
	}
	if claimed {
		fmt.Printf("%s: make ci-local has claimed this head; Local verification checks it\n", head)
	} else {
		fmt.Printf("%s: make ci-local has not attested this head yet, so Local verification stays Expected\n", head)
	}
	return nil
}

// attestCommand is the staging gate (deploy-staging.yml's plan, and
// staging-catch-up.yml before it dispatches): it passes a commit of main only
// when make ci-local attested it, on the commit itself or on the exact head of
// the pull request whose squash made it.
func attestCommand(ctx context.Context, args []string) error {
	if len(args) != 1 {
		return usageError{"usage: cilocal attest <sha>"}
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
	fmt.Printf("::error::Not deploying %s: %s.\n", sha, verify.AttestNextStep(sha))
	return errNotVerified
}
