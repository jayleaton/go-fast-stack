package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/jayleaton/go-fast-stack/scripts/cilocal/verify"
)

// make pr-merge is the one supported way to land pull requests
// (.claude/skills/github-prs). It never waits: a PR that is a draft, not
// attested on its head, failing, pending or conflicting is reported with the
// next step and left alone (verify.Decide), so one stuck PR never holds up
// the others. A head behind its base is refused (rebase and re-attest), so
// what merges is exactly the tree that was verified. Before merging a
// PR it retargets to main every open PR stacked on its branch, so the stack
// survives.
//
//	make pr-merge PR="21 12 20"                          merge what is green, report the rest
//	make pr-merge PR=21 ARGS=--dry-run                    say what it would do
//	make pr-merge PR="21 12" ARGS=--then-check-staging    then one staging check for the burst
//	make pr-merge ARGS="--check-deploy <sha>"             after a merge: did staging deploy its squash
//	make pr-merge ARGS=--check-staging                    the staging check of main's head alone
//
// Exits 0 when every listed PR merged (or was already merged) and staging, if
// checked, is healthy; 1 otherwise. Run it again later for the PRs it reported.

const mergeUsage = `usage: make pr-merge PR="<number>..." [ARGS="--dry-run --any-base --require-actions --then-check-staging"]
       make pr-merge ARGS="--check-deploy <squash sha>"
       make pr-merge ARGS=--check-staging`

var commitSHA = regexp.MustCompile(`^[0-9a-f]{7,40}$`)

// mergeFlags are make pr-merge's flags.
type mergeFlags struct {
	dryRun, thenCheckStaging, checkStaging bool
	checkDeploy                            string
	options                                verify.MergeOptions
}

func mergeCommand(ctx context.Context, args []string) error {
	flags := flag.NewFlagSet("pr-merge", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	var opts mergeFlags
	flags.BoolVar(&opts.dryRun, "dry-run", false, "say what it would do")
	flags.BoolVar(&opts.options.AnyBase, "any-base", false, "merge PRs stacked on a branch other than main")
	flags.BoolVar(&opts.options.RequireActions, "require-actions", false, "refuse when no GitHub CI job ran on the head")
	flags.BoolVar(&opts.thenCheckStaging, "then-check-staging", false, "after merging, follow main's staging deploy until it settles (bounded)")
	flags.BoolVar(&opts.checkStaging, "check-staging", false, "check main's head on staging once")
	flags.StringVar(&opts.checkDeploy, "check-deploy", "", "check once that staging deployed this squash commit")
	if err := flags.Parse(args); err != nil {
		return usageError{fmt.Sprintf("%v\n%s", err, mergeUsage)}
	}
	numbers := []int{}
	for _, arg := range flags.Args() {
		number, err := strconv.Atoi(strings.TrimPrefix(arg, "#"))
		if err != nil {
			return usageError{fmt.Sprintf("%q is not a pull request number\n%s", arg, mergeUsage)}
		}
		numbers = append(numbers, number)
	}
	switch {
	case opts.checkDeploy != "" && !commitSHA.MatchString(opts.checkDeploy):
		return usageError{fmt.Sprintf("--check-deploy needs a commit sha, not %q\n%s", opts.checkDeploy, mergeUsage)}
	case len(numbers) == 0 && !opts.checkStaging && opts.checkDeploy == "":
		return usageError{mergeUsage}
	}
	hub, err := newGitHub(ctx)
	if err != nil {
		return err
	}
	if opts.checkDeploy != "" {
		if err := checkStaging(ctx, hub, opts.checkDeploy, false); errors.Is(err, errStaging) {
			return errNotVerified
		} else if err != nil {
			return err
		}
		return nil
	}
	failed := false
	for _, number := range numbers {
		verdict, err := mergeOne(ctx, hub, number, opts)
		if err != nil {
			verdict = verify.Verdict{Action: verify.ActionSkip, Reason: "gh failed: " + err.Error(), Next: "read the error; run again", Note: ""}
		}
		failed = failed || (verdict.Action != verify.ActionDone && verdict.Action != verify.ActionMerge && verdict.Action != merged)
		fmt.Printf("#%d: %s - %s\n", number, verdict.Action, verdict.Reason)
		if verdict.Next != "" {
			fmt.Printf("        next: %s\n", verdict.Next)
		}
	}
	if opts.checkStaging || (opts.thenCheckStaging && !opts.dryRun) {
		if err := checkStaging(ctx, hub, "", opts.thenCheckStaging); err != nil {
			if !errors.Is(err, errStaging) {
				fmt.Printf("staging: could not read it: %v\n", err)
			}
			failed = true
		}
	}
	if failed {
		return errNotVerified
	}
	return nil
}

// merged is the action of a PR this run squash-merged.
const merged = "merged"

// mergeAttempts and mergePause bound the wait for GitHub to recompute
// mergeability after main moves.
const (
	mergeAttempts = 5
	mergePause    = 3 * time.Second
)

func mergeOne(ctx context.Context, hub github, number int, opts mergeFlags) (verify.Verdict, error) {
	pull, err := hub.pull(ctx, number)
	for attempt := 0; err == nil && pull.Mergeable == "UNKNOWN" && attempt < mergeAttempts; attempt++ {
		if err := pause(ctx, mergePause); err != nil {
			return verify.Verdict{}, err
		}
		pull, err = hub.pull(ctx, number)
	}
	if err != nil {
		return verify.Verdict{}, err
	}
	verification, err := hub.verification(ctx, pull.Body, pull.HeadRefOid, pull.fork(hub.repo))
	if err != nil {
		return verify.Verdict{}, err
	}
	behind := 0
	if pull.State == "OPEN" {
		if behind, err = hub.behind(ctx, pull.BaseRefName, pull.HeadRefOid); err != nil {
			return verify.Verdict{}, err
		}
	}
	verdict := verify.Decide(verify.Pull{
		Number: number, State: pull.State, Draft: pull.IsDraft, Base: pull.BaseRefName, Mergeable: pull.Mergeable,
		Behind: behind, Checks: pull.Checks, Verification: verification,
	}, opts.options)
	if verdict.Action != verify.ActionMerge {
		return verdict, nil
	}
	fmt.Printf("#%d: %s\n", number, verdict.Note)
	if err := retargetStack(ctx, hub, number, pull, opts.dryRun); err != nil {
		return verify.Verdict{}, err
	}
	if opts.dryRun {
		return verify.Verdict{Action: verify.ActionMerge, Reason: "would merge " + verify.Short(pull.HeadRefOid) + " (dry run)", Next: "", Note: ""}, nil
	}
	// The base may have moved since it was read: read it once more right
	// before the merge, which leaves only GitHub's own instant for a race.
	if behind, err := hub.behind(ctx, pull.BaseRefName, pull.HeadRefOid); err != nil {
		return verify.Verdict{}, err
	} else if behind > 0 {
		return verify.Decide(verify.Pull{
			Number: number, State: pull.State, Draft: pull.IsDraft, Base: pull.BaseRefName, Mergeable: pull.Mergeable,
			Behind: behind, Checks: pull.Checks, Verification: verification,
		}, opts.options), nil
	}
	// Squash exactly the head that was attested; never --delete-branch (it
	// closes the PRs stacked on the branch).
	if _, err := output(ctx, "", "gh", "pr", "merge", strconv.Itoa(number), "-R", hub.repo, "--squash", "--match-head-commit", pull.HeadRefOid); err != nil {
		return verify.Verdict{}, err
	}
	squash, err := output(ctx, "", "gh", "pr", "view", strconv.Itoa(number), "-R", hub.repo, "--json", "mergeCommit", "-q", ".mergeCommit.oid")
	if err != nil {
		return verify.Verdict{}, err
	}
	return verify.Verdict{
		Action: merged,
		Reason: fmt.Sprintf("squashed %s as %s", verify.Short(pull.HeadRefOid), squash),
		// A follow-up, never a gate: merge the next PR now (AGENTS.md, "Environments").
		Next: fmt.Sprintf(`once its deploy has run, make pr-merge ARGS="--check-deploy %s"; a failed or refused deploy is fixed forward before anything else merges`, squash),
		Note: "",
	}, nil
}

// retargetStack points every open PR stacked on the PR's branch at main.
func retargetStack(ctx context.Context, hub github, number int, pull pullRequest, dryRun bool) error {
	stacked, err := output(ctx, "", "gh", "pr", "list", "-R", hub.repo, "--base", pull.HeadRefName, "--state", "open", "--json", "number", "-q", ".[].number")
	if err != nil {
		return err
	}
	for _, child := range strings.Fields(stacked) {
		if !dryRun {
			if _, err := output(ctx, "", "gh", "pr", "edit", child, "-R", hub.repo, "--base", verify.Main); err != nil {
				return err
			}
		}
		fmt.Printf("#%d: retargeted stacked #%s to %s; after this merge rebase it: git rebase --onto origin/%s %s <its branch>\n",
			number, child, verify.Main, verify.Main, pull.HeadRefOid)
	}
	return nil
}

func pause(ctx context.Context, wait time.Duration) error {
	select {
	case <-ctx.Done():
		return fmt.Errorf("interrupted: %w", ctx.Err())
	case <-time.After(wait):
		return nil
	}
}

// How long --then-check-staging follows a deploy, and how often it reads.
const (
	stagingTimeout  = 60 * time.Minute
	stagingInterval = 90 * time.Second
)

var errStaging = errors.New("staging is not healthy")

// checkStaging is one staging check: of `commit` (a merge's squash), or of
// main's head for the whole burst, following it until it settles when
// `wait` is set.
func checkStaging(ctx context.Context, hub github, commit string, wait bool) error {
	deadline := time.Now().Add(stagingTimeout)
	for {
		verdict, err := readStaging(ctx, hub, commit)
		if err != nil {
			return err
		}
		if verdict.Settled || !wait || time.Now().After(deadline) {
			unsettled := ""
			if !verdict.Settled {
				unsettled = " (not settled; Staging catch-up deploys main within the hour; check once more later)"
			}
			fmt.Printf("staging: %s%s\n", verdict.Summary, unsettled)
			if !verdict.OK {
				return errStaging
			}
			return nil
		}
		if err := pause(ctx, stagingInterval); err != nil {
			return err
		}
	}
}

func readStaging(ctx context.Context, hub github, commit string) (verify.StagingVerdict, error) {
	head := commit
	if head == "" {
		var err error
		if head, err = output(ctx, "", "gh", "api", "repos/"+hub.repo+"/commits/"+verify.Main, "-q", ".sha"); err != nil {
			return verify.StagingVerdict{}, err
		}
	}
	// Every run on the commit, filtered here: --workflow fails while the
	// default branch has no deploy-staging.yml.
	out, err := output(ctx, "", "gh", "run", "list", "-R", hub.repo, "--commit", head,
		"--json", "databaseId,workflowName,status,conclusion,url")
	if err != nil {
		return verify.StagingVerdict{}, err
	}
	var runs []struct {
		verify.DeployRun
		DatabaseID int64 `json:"databaseId"`
	}
	if err := json.Unmarshal([]byte(out), &runs); err != nil {
		return verify.StagingVerdict{}, fmt.Errorf("gh run list: %w", err)
	}
	deploys := []verify.DeployRun{}
	for _, run := range runs {
		if run.Name != verify.DeployWorkflow {
			continue
		}
		if run.Status == "completed" && run.Conclusion == "success" {
			jobs, err := runJobs(ctx, hub, strconv.FormatInt(run.DatabaseID, 10))
			if err != nil {
				return verify.StagingVerdict{}, err
			}
			run.DryRun = verify.DryRunJobs(jobs)
		}
		deploys = append(deploys, run.DeployRun)
	}
	return verify.Staging(head, deploys, readiness(ctx, hub)), nil
}

func runJobs(ctx context.Context, hub github, run string) ([]verify.Job, error) {
	out, err := output(ctx, "", "gh", "run", "view", run, "-R", hub.repo, "--json", "jobs")
	if err != nil {
		return nil, err
	}
	var view struct {
		Jobs []verify.Job `json:"jobs"`
	}
	if err := json.Unmarshal([]byte(out), &view); err != nil {
		return nil, fmt.Errorf("gh run view: %w", err)
	}
	return view.Jobs, nil
}

// Bounds of the /healthz read.
const (
	readyTimeout = 20 * time.Second
	readyBody    = 1 << 16 // 64 KiB: a /healthz answer is a few bytes
)

// readiness GETs <STAGING_WEB_URL>/healthz: from the env, else the
// repository variable; unset when neither has it (the template has no
// default staging URL).
func readiness(ctx context.Context, hub github) verify.Readiness {
	url := os.Getenv("STAGING_WEB_URL")
	if url == "" {
		url, _ = output(ctx, "", "gh", "variable", "get", "STAGING_WEB_URL", "-R", hub.repo)
	}
	url = strings.TrimRight(strings.TrimSpace(url), "/")
	if url == "" {
		return verify.Readiness{}
	}
	ctx, cancel := context.WithTimeout(ctx, readyTimeout)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, url+"/healthz", nil)
	if err != nil {
		return verify.Readiness{URL: url, Err: err.Error()}
	}
	request.Header.Set("Accept", "application/json")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		return verify.Readiness{URL: url, Err: err.Error()}
	}
	defer func() { _ = response.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(response.Body, readyBody))
	if err != nil {
		return verify.Readiness{URL: url, Code: response.StatusCode, Err: err.Error()}
	}
	return verify.ReadinessOf(url, response.StatusCode, body)
}
