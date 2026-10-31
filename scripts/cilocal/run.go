package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/jayleaton/go-fast-stack/scripts/cilocal/verify"
)

// options are make ci-local's flags (make ci-local ARGS="...").
type options struct {
	only      []string
	plan      bool
	keepGoing bool
	publish   bool
	commit    string
	davit     bool
	local     bool
}

func parseOptions(args []string) (options, error) {
	flags := flag.NewFlagSet("ci-local", flag.ContinueOnError)
	only := flags.String("only", "", "run only these checklist items or jobs (comma separated); posts nothing")
	var opts options
	flags.BoolVar(&opts.plan, "plan", false, "print the selection and the steps, run nothing")
	flags.BoolVar(&opts.keepGoing, "keep-going", false, "run every step even after one fails")
	flags.BoolVar(&opts.publish, "publish", false, "post the saved report of this head again (after opening the PR)")
	flags.StringVar(&opts.commit, "commit", "", "attest this commit of the base branch, checked out here, against the last attested one")
	flags.BoolVar(&opts.davit, "davit", false, "run the steps in a fresh Davit box")
	flags.BoolVar(&opts.local, "local", false, "run the steps on this machine, waiting for its slot")
	if err := flags.Parse(args); err != nil {
		return options{}, fmt.Errorf("ci-local: %w", err)
	}
	if *only != "" {
		opts.only = strings.Split(*only, ",")
	}
	if opts.davit && opts.local {
		return options{}, errors.New("ci-local: --davit and --local exclude each other")
	}
	return opts, nil
}

func logf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "ci-local: "+format+"\n", args...)
}

// state is the checkout and its pull request as the run found them.
type state struct {
	head       verify.Head
	tree       string
	pull       *pullRequest
	selectBase string // the commit the change is taken against
	baseName   string
	files      []string
	commonDir  string
	module     string
}

func repositoryState(ctx context.Context, hub github, opts options) (state, error) {
	var found state
	if _, err := git(ctx, "", "fetch", "--quiet", "origin"); err != nil {
		return found, err
	}
	sha, err := git(ctx, "", "rev-parse", "HEAD")
	if err != nil {
		return found, err
	}
	branch, _ := git(ctx, "", "symbolic-ref", "--quiet", "--short", "HEAD")
	if found.pull, err = hub.openPull(ctx, branch); err != nil {
		return found, err
	}
	base := landingBranch(ctx)
	if found.pull != nil {
		base = found.pull.BaseRefName
	}
	commit := ""
	if opts.commit != "" {
		if commit, err = git(ctx, "", "rev-parse", "--verify", opts.commit+"^{commit}"); err != nil {
			return found, err
		}
	}
	mainCommit := commit != "" || branch == base
	pushed := ""
	if branch != "" && !mainCommit {
		remote, _ := git(ctx, "", "ls-remote", "origin", "refs/heads/"+branch)
		if fields := strings.Fields(remote); len(fields) > 0 {
			pushed = fields[0]
		}
	}
	_, notAncestor := git(ctx, "", "merge-base", "--is-ancestor", sha, "origin/"+base)
	dirty, err := git(ctx, "", "status", "--porcelain")
	if err != nil {
		return found, err
	}
	if mainCommit {
		branch = ""
	}
	found.head = verify.Head{SHA: sha, Branch: branch, Dirty: dirty, Commit: commit, MainCommit: mainCommit, Pushed: pushed, OnBase: notAncestor == nil, Base: base}
	if found.tree, err = git(ctx, "", "rev-parse", "HEAD^{tree}"); err != nil {
		return found, err
	}
	common, err := git(ctx, "", "rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil {
		return found, err
	}
	found.commonDir = filepath.Join(common, "cilocal")
	found.module, err = output(ctx, "", "go", "list", "-m")
	return found, err
}

// landingBranch is the branch work lands on (scripts/branches.sh): dev when
// origin has it, else main.
func landingBranch(ctx context.Context) string {
	if _, err := git(ctx, "", "rev-parse", "--verify", "--quiet", "refs/remotes/origin/dev"); err == nil {
		return "dev"
	}
	return "main"
}

func (found *state) selectAgainst(ctx context.Context, hub github) error {
	mergeBase, err := git(ctx, "", "merge-base", "HEAD", "origin/"+found.head.Base)
	if err != nil {
		return err
	}
	ancestors := []string{}
	if found.head.MainCommit {
		list, err := git(ctx, "", "rev-list", "--first-parent", fmt.Sprintf("--max-count=%d", verify.AttestedAncestorLimit), found.head.SHA+"^")
		if err != nil {
			return err
		}
		ancestors = strings.Fields(list)
	}
	found.selectBase = verify.SelectionBase(found.head.MainCommit, mergeBase, ancestors, func(sha string) bool {
		by, _, err := hub.attested(ctx, sha)
		return err == nil && by != ""
	})
	found.baseName = "origin/" + found.head.Base
	if found.head.MainCommit {
		found.baseName = "the last attested " + found.head.Base + " commit"
	}
	if found.selectBase == "" {
		return fmt.Errorf("no attested commit among the %d before %s on %s: attest the oldest unattested one first", verify.AttestedAncestorLimit, verify.Short(found.head.SHA), found.head.Base)
	}
	list, err := git(ctx, "", "diff", "--name-only", "--no-renames", found.selectBase, found.head.SHA)
	found.files = strings.Fields(list)
	return err
}

// runCommand is make ci-local.
func runCommand(ctx context.Context, args []string) error {
	opts, err := parseOptions(args)
	if err != nil {
		return err
	}
	top, err := git(ctx, "", "rev-parse", "--show-toplevel")
	if err != nil {
		return err
	}
	if err := os.Chdir(top); err != nil {
		return fmt.Errorf("ci-local: %w", err)
	}
	hub, err := newGitHub(ctx)
	if err != nil {
		return err
	}
	found, err := repositoryState(ctx, hub, opts)
	if err != nil {
		return err
	}
	refusal := found.head.Refusal()
	partial := len(opts.only) > 0 || opts.plan
	attesting := !partial && refusal == ""
	switch {
	case refusal != "" && !partial:
		return fmt.Errorf("refusing to attest: %s", refusal)
	case refusal != "":
		logf("posting nothing: %s", refusal)
	}
	if opts.publish {
		report, err := loadReport(found.commonDir, found.head.SHA)
		if err != nil {
			return err
		}
		return publish(ctx, hub, found, report)
	}
	if attesting {
		statuses, err := hub.statuses(ctx, found.head.SHA)
		if err != nil {
			return err
		}
		if why := verify.FailedHeadRefusal(found.head.SHA, statuses); why != "" {
			return errors.New(why)
		}
	}
	if err := found.selectAgainst(ctx, hub); err != nil {
		return err
	}
	return runSteps(ctx, hub, found, opts, attesting)
}

// checkout is the fresh worktree of the head the steps run in.
type checkout struct {
	dir     string
	compose string // the compose project its make targets use
}

func addCheckout(ctx context.Context, found state) (checkout, error) {
	work := checkout{dir: filepath.Join(found.commonDir, "wt-"+verify.Short(found.head.SHA)), compose: "cilocal-" + verify.Short(found.head.SHA)}
	if _, err := os.Stat(work.dir); err == nil {
		_, _ = git(ctx, "", "worktree", "remove", "--force", work.dir)
	}
	if err := os.MkdirAll(found.commonDir, dirMode); err != nil {
		return work, fmt.Errorf("ci-local: %w", err)
	}
	_, err := git(ctx, "", "worktree", "add", "--quiet", "--detach", work.dir, found.head.SHA)
	return work, err
}

func (work checkout) remove(ctx context.Context, docker bool) {
	if docker {
		command := exec.CommandContext(ctx, "docker", "compose", "-p", work.compose, "--profile", "test", "down", "-v", "--remove-orphans")
		command.Dir = work.dir
		_ = command.Run()
	}
	if _, err := git(ctx, "", "worktree", "remove", "--force", work.dir); err != nil {
		logf("could not remove %s: %v", work.dir, err)
	}
}

// affectedEnv is scripts/affected.sh's input: the changed files, and a
// full run when a file that decides every step changed.
func affectedEnv(files []string, everything bool) map[string]string {
	env := map[string]string{"AFFECTED_FILES": strings.Join(files, " ")}
	if everything || len(files) == 0 {
		env["AFFECTED_FULL"] = "1"
	}
	return env
}

// affectedIn runs scripts/affected.sh in `dir`: the root module's packages
// the change can affect, as key=value lines.
func affectedIn(ctx context.Context, dir string, env map[string]string) (string, error) {
	command := exec.CommandContext(ctx, "./scripts/affected.sh")
	command.Dir = dir
	command.Env = stepEnv(env)
	command.Stderr = os.Stderr
	out, err := command.Output()
	if err != nil {
		return "", fmt.Errorf("scripts/affected.sh: %w", err)
	}
	return string(out), nil
}

// bash4 is whether the bash on PATH runs the repo's scripts (mapfile,
// associative arrays): macOS ships bash 3.2.
func bash4(ctx context.Context) bool {
	return exec.CommandContext(ctx, "bash", "-c", "((BASH_VERSINFO[0] >= 4))").Run() == nil
}

// placement is where the steps run.
type placement struct {
	davit bool
	slot  *os.File
}

func place(ctx context.Context, opts options, partial bool) (placement, error) {
	_, davitErr := exec.LookPath("davit")
	switch {
	case !opts.local && !opts.davit && davitErr == nil && !bash4(ctx):
		logf("the bash on PATH is older than 4, which the repo's scripts need: running the steps in a Davit box (brew install bash to run them here)")
		return placement{davit: true, slot: nil}, nil
	case opts.local && !bash4(ctx):
		return placement{}, errors.New("--local: the repo's scripts need bash 4 or newer on PATH (macOS: brew install bash)")
	case opts.davit:
		if davitErr != nil {
			return placement{}, errors.New("--davit: davit is not on PATH")
		}
		return placement{davit: true, slot: nil}, nil
	case partial:
		return placement{davit: false, slot: nil}, nil // an --only iteration takes no slot
	case !opts.local:
		slot, err := tryAnySlot()
		if err != nil || slot != nil {
			return placement{davit: false, slot: slot}, err
		}
		if davitErr == nil {
			logf("this machine's run slot is taken: running the steps in a Davit box instead (--local waits for the slot)")
			return placement{davit: true, slot: nil}, nil
		}
	}
	slot, err := takeSlot(ctx)
	return placement{davit: false, slot: slot}, err
}

func (where placement) runner() string {
	if where.davit {
		return "davit linux"
	}
	return "local " + runtime.GOOS + "/" + runtime.GOARCH
}

func runSteps(ctx context.Context, hub github, found state, opts options, attesting bool) error {
	started := time.Now()
	areas := verify.SelectAreas(found.files)
	work, err := addCheckout(ctx, found)
	if err != nil {
		return err
	}
	usedDocker := false
	defer func() { work.remove(context.WithoutCancel(ctx), usedDocker) }()
	treeOut, err := git(ctx, "", "ls-tree", "-r", "-z", found.head.SHA)
	if err != nil {
		return err
	}
	entries := []verify.TreeEntry{}
	for _, entry := range verify.ParseTree(treeOut) {
		if entry.Mode != "160000" { // submodules are not files of the tree
			entries = append(entries, entry)
		}
	}
	where, err := place(ctx, opts, len(opts.only) > 0 || opts.plan)
	if err != nil {
		return err
	}
	defer releaseSlot(where.slot)
	var box *davitBox
	if where.davit {
		box = newBox(ctx, work, found)
		defer box.remove(context.WithoutCancel(ctx))
	}
	affected := verify.Affected{}
	if areas.Go {
		env := affectedEnv(found.files, areas.Everything)
		var lines string
		if box != nil {
			lines, err = box.affected(ctx, entries, env)
		} else {
			lines, err = affectedIn(ctx, work.dir, env)
		}
		if err != nil {
			return err
		}
		affected = verify.ParseAffected(lines)
	}
	steps := verify.Only(verify.Plan(areas, affected, found.selectBase), opts.only)
	hashes := verify.InputHashes(steps, entries, where.runner())
	reused, sources, err := planReuse(ctx, hub, found, hashes, attesting)
	if err != nil {
		return err
	}
	logf("%s against %s (merge-base %s): %d changed files, areas %v, %d steps (%d reused), on %s",
		verify.Short(found.head.SHA), found.baseName, verify.Short(found.selectBase), len(found.files), areas.Names(), len(steps), len(reused), where.runner())
	if opts.plan {
		for _, step := range steps {
			note := ""
			if lent, ok := reused[step.ID()]; ok {
				note = " (reused from " + verify.Short(lent.From) + ")"
			}
			fmt.Printf("%-10s %-40s %s%s\n", step.Item, step.ID(), strings.Join(step.Run, " "), note)
		}
		return nil
	}
	waitForMemory(ctx, where)
	toRun := []verify.Step{}
	for _, step := range steps {
		if _, ok := reused[step.ID()]; !ok {
			toRun = append(toRun, step)
			usedDocker = usedDocker || step.Job == verify.JobGo
		}
	}
	env := map[string]string{"COMPOSE_PROJECT_NAME": work.compose, "CI": "true", "CI_LOCAL": "1"}
	ran, host, treeProblem := execution(ctx, work, found, toRun, env, opts.keepGoing, box, entries)
	report := buildReport(ctx, found, areas, steps, hashes, reused, sources, ran, host, where.runner(), started)
	if err := saveReport(found.commonDir, report, len(opts.only) > 0); err != nil {
		return err
	}
	printSummary(report)
	if treeProblem != "" {
		return fmt.Errorf("refusing to attest: %s", treeProblem)
	}
	if attesting {
		if err := publish(ctx, hub, found, report); err != nil {
			return err
		}
	}
	if report.Result != verify.Passed {
		return errNotVerified
	}
	return nil
}

func planReuse(ctx context.Context, hub github, found state, hashes map[string]string, attesting bool) (map[string]verify.Lent, []verify.Ledger, error) {
	if !attesting || found.pull == nil {
		return map[string]verify.Lent{}, nil, nil
	}
	section, _ := verify.SectionOf(found.pull.Body)
	own, sources := verify.Ledgers(section)
	ledgers := sources
	if own != nil {
		ledgers = append([]verify.Ledger{*own}, sources...)
	}
	var lookupErr error
	lenders := verify.Lenders(ledgers, found.head.SHA, func(sha string) []verify.Status {
		statuses, err := hub.statuses(ctx, sha)
		if err != nil {
			lookupErr = err
		}
		return statuses
	})
	if lookupErr != nil {
		return nil, nil, lookupErr
	}
	reused := verify.Reuses(hashes, lenders)
	used := []verify.Ledger{}
	for _, lender := range lenders {
		for _, lent := range reused {
			if lent.From == lender.Head {
				used = append(used, lender)
				break
			}
		}
	}
	return reused, used, nil
}

// execution runs the steps: those marked Local here, the rest here or in a
// Davit box. It returns the results, where they ran, and why the checkout
// may not be attested ("" when it may).
func execution(ctx context.Context, work checkout, found state, steps []verify.Step, env map[string]string, keepGoing bool, box *davitBox, entries []verify.TreeEntry) (results, string, string) {
	hostname, _ := os.Hostname()
	digest := verify.TreeDigest(entries)
	local, remote := []verify.Step{}, []verify.Step{}
	for _, step := range steps {
		if step.Local || box == nil {
			local = append(local, step)
		} else {
			remote = append(remote, step)
		}
	}
	ran := runHere(ctx, work.dir, payload{Tree: digest, Steps: local, Env: env, KeepGoing: keepGoing}, found.module)
	if after, err := worktreeDigest(ctx, work.dir); err != nil || after != digest {
		return ran, hostname, "a step changed the checkout's files (git status in it shows what)"
	}
	if len(remote) == 0 || (!keepGoing && failedAny(ran)) {
		return ran, hostname, ""
	}
	inBox, err := box.run(ctx, payload{Tree: digest, Steps: remote, Env: env, KeepGoing: keepGoing, Affected: false, Paths: pathsOf(entries)}, found.module)
	for id, done := range inBox.done {
		ran.done[id] = done
	}
	for id, failures := range inBox.failures {
		ran.failures[id] = failures
	}
	host := "davit:" + box.name
	switch {
	case err != nil:
		return ran, host, err.Error()
	case inBox.tree != digest:
		return ran, host, fmt.Sprintf("the box's tree %s is not the head's %s", verify.Short(inBox.tree), verify.Short(digest))
	case inBox.after != digest:
		return ran, host, "a step changed the box's files"
	}
	return ran, host, ""
}

func failedAny(ran results) bool {
	for _, done := range ran.done {
		if done.Exit != 0 {
			return true
		}
	}
	return false
}

func runHere(ctx context.Context, dir string, work payload, module string) results {
	reader, writer := io.Pipe()
	go func() {
		execute(ctx, dir, work, writer)
		_ = writer.Close()
	}()
	return readResults(reader, os.Stderr, module)
}

func buildReport(ctx context.Context, found state, areas verify.Areas, steps []verify.Step, hashes map[string]string, reused map[string]verify.Lent, sources []verify.Ledger, ran results, host, runner string, started time.Time) verify.Report {
	result := verify.Passed
	stepResults := []verify.StepResult{}
	for _, step := range steps {
		entry := verify.StepResult{Step: step, InputHash: hashes[step.ID()], Exit: nil, DurationMS: 0, Reused: nil, Failure: nil}
		if lent, ok := reused[step.ID()]; ok {
			zero := 0
			entry.Exit, entry.Reused = &zero, &lent
		} else if done, ok := ran.done[step.ID()]; ok {
			exit := done.Exit
			entry.Exit, entry.DurationMS = &exit, done.DurationMS
			if exit != 0 && ran.failures[step.ID()] != nil {
				entry.Failure = ran.failures[step.ID()].Failure(found.files)
				logf("%s failed: %s", step.ID(), entry.Failure.Hint)
			}
		}
		if entry.Exit == nil || *entry.Exit != 0 {
			result = verify.Failed
		}
		stepResults = append(stepResults, entry)
	}
	goVersion, _ := output(ctx, "", "go", "env", "GOVERSION")
	dockerVersion, _ := output(ctx, "", "docker", "version", "--format", "{{.Server.Version}}")
	return verify.Report{
		Head: found.head.SHA, Tree: found.tree, MergeBase: found.selectBase, Base: found.baseName, Host: host,
		Versions:     map[string]string{"go": goVersion, "docker": dockerVersion, "runner": runner},
		Areas:        areas.Names(),
		ChangedFiles: len(found.files), Steps: stepResults, Sources: sources,
		FinishedAt: time.Now().UTC().Format(time.RFC3339), DurationMS: time.Since(started).Milliseconds(), Result: result,
	}
}

func reportPath(dir, head string, partial bool) string {
	if partial {
		return filepath.Join(dir, head+".only.json")
	}
	return filepath.Join(dir, head+".json")
}

// saveReport keeps the run's report in the git directory; a partial run
// never replaces the full run's.
func saveReport(dir string, report verify.Report, partial bool) error {
	encoded, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		return fmt.Errorf("report: %w", err)
	}
	if err := os.WriteFile(reportPath(dir, report.Head, partial), encoded, fileMode); err != nil {
		return fmt.Errorf("report: %w", err)
	}
	return nil
}

func loadReport(dir, head string) (verify.Report, error) {
	raw, err := os.ReadFile(reportPath(dir, head, false))
	if err != nil {
		return verify.Report{}, fmt.Errorf("no saved full report for %s: run make ci-local first: %w", verify.Short(head), err)
	}
	var report verify.Report
	if err := json.Unmarshal(raw, &report); err != nil {
		return verify.Report{}, fmt.Errorf("report: %w", err)
	}
	return report, nil
}

func printSummary(report verify.Report) {
	fmt.Fprintln(os.Stderr)
	for _, step := range report.Steps {
		state := "not run"
		switch {
		case step.Reused != nil:
			state = "reused from " + verify.Short(step.Reused.From)
		case step.Exit != nil && *step.Exit == 0:
			state = "passed in " + verify.Duration(step.DurationMS)
		case step.Exit != nil:
			state = fmt.Sprintf("FAILED (exit %d) in %s", *step.Exit, verify.Duration(step.DurationMS))
		}
		fmt.Fprintf(os.Stderr, "  %-10s %-40s %s\n", step.Item, step.ID(), state)
	}
	logf("%s %s in %s on %s", verify.Short(report.Head), report.Result, verify.Duration(report.DurationMS), report.Host)
}

// publish posts the report: the `local-ci` status first, then the PR body's
// section. The body edit starts the PR's CI run (`edited`), whose Local
// verification job then reads both; when the body already holds this
// section, the failed jobs of the newest run are re-run instead.
func publish(ctx context.Context, hub github, found state, report verify.Report) error {
	passed := report.Result == verify.Passed
	if passed {
		statuses, err := hub.statuses(ctx, report.Head)
		if err != nil {
			return err
		}
		if why := verify.FailedHeadRefusal(report.Head, statuses); why != "" {
			return fmt.Errorf("refusing to post success: %s", why)
		}
	}
	section := verify.RenderSection(report)
	current := found.pull != nil && found.pull.HeadRefOid == report.Head
	state, target := "failure", ""
	if passed {
		state = "success"
	}
	if current {
		target = found.pull.URL + "#local-verification"
	}
	if err := hub.postStatus(ctx, report.Head, state, verify.StatusDescription(report, section), target); err != nil {
		return err
	}
	logf("posted %s=%s on %s", verify.StatusContext, state, report.Head)
	if !current {
		switch {
		case found.head.MainCommit:
			logf("%s commit %s: the staging gate reads %s on it", found.head.Base, verify.Short(report.Head), verify.StatusContext)
		case found.pull != nil:
			logf("%s is at %s, not %s: its body was left alone", found.pull.URL, verify.Short(found.pull.HeadRefOid), verify.Short(report.Head))
		default:
			logf("no open PR for %s: open it from the template (.github/pull_request_template.md), then run make ci-local ARGS=--publish", found.head.Branch)
		}
		fmt.Printf("\n%s\n", section)
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
	logf("filled the Local verification section of %s; its CI run starts now", found.pull.URL)
	return nil
}

// rerunWaitingCI re-runs the failed jobs of the PR's newest CI run on
// `head`: it read the PR before this run posted. It waits (bounded) for a
// run still in progress.
func rerunWaitingCI(ctx context.Context, hub github, head string) {
	const attempts, pause = 40, 15 * time.Second
	for attempt := range attempts {
		out, err := output(ctx, "", "gh", "run", "list", "-R", hub.repo, "--workflow", "ci.yml", "--commit", head, "--event", "pull_request",
			"--limit", "1", "--json", "databaseId,status,conclusion,url")
		var runs []struct {
			DatabaseID int64  `json:"databaseId"`
			Status     string `json:"status"`
			Conclusion string `json:"conclusion"`
			URL        string `json:"url"`
		}
		if err != nil || json.Unmarshal([]byte(out), &runs) != nil || len(runs) == 0 {
			logf("no PR CI run on this head: it starts on an edit of the PR, and its body already holds this report; edit its title or description once to start it")
			return
		}
		latest := runs[0]
		if latest.Status == "completed" {
			if latest.Conclusion == "success" {
				logf("PR CI is green: %s", latest.URL)
				return
			}
			if _, err := output(ctx, "", "gh", "run", "rerun", fmt.Sprint(latest.DatabaseID), "-R", hub.repo, "--failed"); err != nil {
				logf("could not re-run %s: %v", latest.URL, err)
				return
			}
			logf("re-ran the failed jobs of %s", latest.URL)
			return
		}
		if attempt == 0 {
			logf("waiting for PR CI %s to finish before re-running it", latest.URL)
		}
		time.Sleep(pause)
	}
	logf("PR CI is still running; re-run it with gh run rerun <id> --failed if it fails")
}

// waitForMemory waits until the machine that runs the steps has memory for
// them (verify.MemoryPressure), saying why once a minute, and never fails
// for it. Linux only (it reads /proc/meminfo); a Davit box is placed by
// the host's own capacity check instead.
func waitForMemory(ctx context.Context, where placement) {
	if where.davit {
		return
	}
	read := func() (verify.Meminfo, error) {
		text, err := os.ReadFile("/proc/meminfo")
		if err != nil {
			return verify.Meminfo{}, fmt.Errorf("meminfo: %w", err)
		}
		return verify.ReadMeminfo(string(text))
	}
	before, err := read()
	if err != nil {
		return
	}
	floor := float64(verify.MinAvailableGiB)
	if value, err := parseFloat(os.Getenv("CI_LOCAL_MIN_AVAILABLE_GIB")); err == nil {
		floor = value
	}
	const sample, sayEvery = 10 * time.Second, 6
	for checks := 0; ; checks++ {
		select {
		case <-ctx.Done():
			return
		case <-time.After(sample):
		}
		after, err := read()
		if err != nil {
			return
		}
		why := verify.MemoryPressure(before, after, floor)
		if why == "" {
			return
		}
		if checks%sayEvery == 0 {
			logf("waiting for memory before the steps: %s (CI_LOCAL_MIN_AVAILABLE_GIB); this run starts when the machine has room", why)
		}
		before = after
	}
}

func parseFloat(text string) (float64, error) {
	var value float64
	if _, err := fmt.Sscan(text, &value); err != nil {
		return 0, fmt.Errorf("number: %w", err)
	}
	return value, nil
}

func pathsOf(entries []verify.TreeEntry) []string {
	paths := make([]string, 0, len(entries))
	for _, entry := range entries {
		paths = append(paths, entry.Path)
	}
	return paths
}
