package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/jayleaton/go-fast-stack/scripts/cilocal/verify"
)

// make ci-local runs no checks. The agent runs what its change needs first
// (docs/CI.md, "What to run"), commits and pushes; this then refuses a dirty
// or unpushed tree, or a head that already failed, posts the `local-ci`
// commit status on that exact SHA and fills the PR body's "Local
// verification" section with the statement and the head. It takes seconds.
//
//	make ci-local RAN="make lint test TEST_PKGS=./x"
//	make ci-local FAILED="make test: TestX fails" [RAN="…"]
//	make ci-local COMMIT=<sha> RAN="…"     attest a main commit checked out here
//
// A head attested as failed is never attested again: the failure is resolved
// by a new commit, in this PR or forward off main (AGENTS.md, "Verifying").

// options are make ci-local's flags.
type options struct {
	ran, failed, commit string
	failedSet           bool
}

const runUsage = `usage: make ci-local RAN="<what you ran>" [FAILED="<what failed>"] [COMMIT=<main sha>]
       go run ./scripts/cilocal run --ran "<what you ran>" [--failed "<what failed>"] [--commit <sha>]
make ci-local runs nothing: run what the change needs first (docs/CI.md, "What to run")`

func parseOptions(args []string) (options, error) {
	flags := flag.NewFlagSet("ci-local", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	var opts options
	flags.StringVar(&opts.ran, "ran", "", "what you ran on this head (required unless --failed)")
	flags.StringVar(&opts.failed, "failed", "", "what failed: posts failure, final for this head")
	flags.StringVar(&opts.commit, "commit", "", "attest this main commit, checked out detached here")
	if err := flags.Parse(args); err != nil {
		return options{}, usageError{fmt.Sprintf("%v\n%s", err, runUsage)}
	}
	if flags.NArg() > 0 {
		return options{}, usageError{fmt.Sprintf("unexpected argument %q\n%s", flags.Arg(0), runUsage)}
	}
	flags.Visit(func(set *flag.Flag) { opts.failedSet = opts.failedSet || set.Name == "failed" })
	return opts, nil
}

func logf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "ci-local: "+format+"\n", args...)
}

// state is the checkout and its pull request as the attestation found them.
type state struct {
	head verify.Head
	tree string
	pull *pullRequest
}

func repositoryState(ctx context.Context, hub github, opts options) (state, error) {
	var found state
	if _, err := git(ctx, "", "fetch", "--quiet", "origin", verify.Main); err != nil {
		return found, err
	}
	sha, err := git(ctx, "", "rev-parse", "HEAD")
	if err != nil {
		return found, err
	}
	branch, _ := git(ctx, "", "symbolic-ref", "--quiet", "--short", "HEAD")
	commit := ""
	if opts.commit != "" {
		if commit, err = git(ctx, "", "rev-parse", "--verify", opts.commit+"^{commit}"); err != nil {
			return found, err
		}
	}
	mainCommit := commit != "" || branch == verify.Main
	pushed := ""
	if branch != "" && !mainCommit {
		remote, _ := git(ctx, "", "ls-remote", "origin", "refs/heads/"+branch)
		if fields := strings.Fields(remote); len(fields) > 0 {
			pushed = fields[0]
		}
		if found.pull, err = hub.openPull(ctx, branch); err != nil {
			return found, err
		}
	}
	_, notAncestor := git(ctx, "", "merge-base", "--is-ancestor", sha, "origin/"+verify.Main)
	dirty, err := git(ctx, "", "status", "--porcelain")
	if err != nil {
		return found, err
	}
	if mainCommit {
		branch = ""
	}
	found.head = verify.Head{SHA: sha, Branch: branch, Dirty: dirty, Commit: commit, MainCommit: mainCommit, Pushed: pushed, OnBase: notAncestor == nil, Base: verify.Main}
	found.tree, err = git(ctx, "", "rev-parse", "HEAD^{tree}")
	return found, err
}

// runCommand is make ci-local.
func runCommand(ctx context.Context, args []string) error {
	opts, err := parseOptions(args)
	if err != nil {
		return err
	}
	ranText := opts.ran
	if ranText == "" && opts.failedSet {
		ranText = "see Failed below"
	}
	ran, problem := verify.StatementOf(ranText)
	failed := ""
	if problem == "" && opts.failedSet {
		failed, problem = verify.StatementOf(opts.failed)
	}
	if problem != "" {
		return usageError{problem + "\n" + runUsage}
	}
	hub, err := newGitHub(ctx)
	if err != nil {
		return err
	}
	found, err := repositoryState(ctx, hub, opts)
	if err != nil {
		return err
	}
	if refusal := found.head.Refusal(); refusal != "" {
		return usageError{"refusing to attest: " + refusal}
	}
	statuses, err := hub.statuses(ctx, found.head.SHA)
	if err != nil {
		return err
	}
	if why := verify.FailedHeadRefusal(found.head.SHA, statuses); why != "" {
		return usageError{"refusing to attest: " + why}
	}
	if found.head.MainCommit && !opts.failedSet {
		last, err := lastAttested(ctx, hub, found.head.SHA)
		if err != nil {
			return err
		}
		if why := verify.FirstAttestationProblem(found.head.SHA, last, ran); why != "" {
			return usageError{"refusing to attest: " + why}
		}
		if last != "" {
			logf("the last attested main commit is %s: the statement covers %s..%s", verify.Short(last), verify.Short(last), verify.Short(found.head.SHA))
		}
	}
	author, _ := git(ctx, "", "config", "user.name")
	if author == "" {
		author = "unknown"
	}
	hostname, _ := os.Hostname()
	report := verify.Report{
		Head: found.head.SHA, Tree: found.tree, Author: author, Hostname: hostname,
		FinishedAt: time.Now().UTC().Format(time.RFC3339), Result: verify.Passed, Ran: ran, Failed: failed,
	}
	if opts.failedSet {
		report.Result = verify.Failed
	}
	if err := publish(ctx, hub, opts, report); err != nil {
		return err
	}
	if report.Result != verify.Passed {
		return errNotVerified
	}
	return nil
}

// lastAttested is the nearest of a main commit's first-parent ancestors
// (verify.AttestedAncestorLimit of them) the staging gate accepts, or "". A
// root commit, or the first commit of a shallow clone, has none.
func lastAttested(ctx context.Context, hub github, sha string) (string, error) {
	list, err := git(ctx, "", "rev-list", "--first-parent", fmt.Sprintf("--max-count=%d", verify.AttestedAncestorLimit+1), sha)
	if err != nil {
		return "", err
	}
	ancestors := strings.Fields(list)[1:] // the first is the commit itself
	var lookupErr error
	last := verify.LastAttested(ancestors, func(ancestor string) bool {
		by, _, err := hub.attested(ctx, ancestor)
		lookupErr = err
		return err != nil || by != ""
	})
	return last, lookupErr
}

// publish reads the checkout again and posts `report` only for exactly that
// clean, pushed head and tree (verify.PublishRefusal): the `local-ci` status
// on the head first, then the PR body's section. The body edit starts the
// PR's CI run (`edited`), whose Local verification job then reads both; when
// the body already holds this section, the PR's newest CI run is re-run
// instead.
func publish(ctx context.Context, hub github, opts options, report verify.Report) error {
	found, err := repositoryState(ctx, hub, opts)
	if err != nil {
		return err
	}
	if why := verify.PublishRefusal(report, found.head, found.tree); why != "" {
		return usageError{"refusing to attest: " + why}
	}
	passed := report.Result == verify.Passed
	section := verify.RenderSection(report)
	current := found.pull != nil && found.pull.HeadRefOid == report.Head
	status, target := "failure", ""
	if passed {
		status = "success"
	}
	if current {
		target = found.pull.URL + "#local-verification"
	}
	if err := hub.postStatus(ctx, report.Head, status, verify.StatusDescription(report, section), target); err != nil {
		return err
	}
	logf("posted %s=%s on %s", verify.StatusContext, status, report.Head)
	if !current {
		switch {
		case found.head.MainCommit:
			logf("main commit %s: the staging gate reads %s on it; dispatch Deploy staging if staging is behind", verify.Short(report.Head), verify.StatusContext)
		case found.pull != nil:
			logf("%s is at %s, not %s: its body was left alone", found.pull.URL, verify.Short(found.pull.HeadRefOid), verify.Short(report.Head))
		default:
			logf("no open PR for %s: open it from the template (.github/pull_request_template.md), then run this again", found.head.Branch)
		}
		return nil
	}
	body := verify.WithSection(found.pull.Body, section)
	if body == found.pull.Body {
		if passed {
			rerunWaitingCI(ctx, hub, report.Head)
		}
		return nil
	}
	if err := hub.editBody(ctx, found.pull.Number, body); err != nil {
		return err
	}
	logf("filled the Local verification section of %s; its PR CI run starts now", found.pull.URL)
	return nil
}

// rerunWaitingCI re-runs the PR's newest CI run on `head` when the body was
// already current (so no `edited` run starts): its Local verification job
// read the PR before this attestation. It re-runs the failed jobs, or the
// whole run when it never ran the job named `Local verification` (it ran as
// "Awaiting make ci-local", so its claim job must read the head again). It
// waits (bounded) for a run still in progress.
func rerunWaitingCI(ctx context.Context, hub github, head string) {
	const attempts, pause = 40, 15 * time.Second
	for attempt := range attempts {
		out, err := output(ctx, "", "gh", "run", "list", "-R", hub.repo, "--workflow", "ci.yml", "--commit", head, "--event", verify.TrustedTrigger,
			"--limit", "1", "--json", "databaseId,status,conclusion,url")
		var runs []struct {
			DatabaseID int64  `json:"databaseId"`
			Status     string `json:"status"`
			Conclusion string `json:"conclusion"`
			URL        string `json:"url"`
		}
		if err != nil || json.Unmarshal([]byte(out), &runs) != nil || len(runs) == 0 {
			logf("no PR CI run on this head yet; it starts on the next edit of the PR")
			return
		}
		latest := runs[0]
		if latest.Status == "completed" {
			id := strconv.FormatInt(latest.DatabaseID, 10)
			checked, err := ranGate(ctx, hub, id)
			if err != nil {
				logf("could not read the jobs of %s: %v", latest.URL, err)
				return
			}
			if latest.Conclusion == "success" && checked {
				logf("PR CI is green: %s", latest.URL)
				return
			}
			rerun := []string{"run", "rerun", id, "-R", hub.repo}
			what := ""
			if checked {
				rerun, what = append(rerun, "--failed"), "the failed jobs of "
			}
			if _, err := output(ctx, "", "gh", rerun...); err != nil {
				logf("could not re-run %s: %v", latest.URL, err)
				return
			}
			logf("re-ran %s%s", what, latest.URL)
			return
		}
		if attempt == 0 {
			logf("waiting for PR CI %s to finish before re-running it", latest.URL)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(pause):
		}
	}
	logf("PR CI is still running; re-run it with gh run rerun <id> --failed if it fails")
}

// ranGate is whether a run has a job named `Local verification`.
func ranGate(ctx context.Context, hub github, run string) (bool, error) {
	jobs, err := runJobs(ctx, hub, run)
	if err != nil {
		return false, err
	}
	for _, job := range jobs {
		if job.Name == verify.Gate {
			return true, nil
		}
	}
	return false, nil
}
