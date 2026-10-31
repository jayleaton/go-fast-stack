package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/jayleaton/go-fast-stack/scripts/cilocal/verify"
)

// Everything here shells out to git and gh, the two tools the rules in
// verify are fed from.

// output runs a command in `dir` (the current directory when empty) and
// returns its trimmed stdout; stderr is part of the error.
func output(ctx context.Context, dir, name string, args ...string) (string, error) {
	command := exec.CommandContext(ctx, name, args...)
	command.Dir = dir
	var stderr bytes.Buffer
	command.Stderr = &stderr
	out, err := command.Output()
	if err != nil {
		return "", fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return strings.TrimSpace(string(out)), nil
}

func git(ctx context.Context, dir string, args ...string) (string, error) {
	return output(ctx, dir, "git", args...)
}

// github reads and writes one repository through gh, so it works wherever gh
// is signed in (a laptop, a runner with GH_TOKEN). `repo` is owner/name.
type github struct {
	repo string
}

func newGitHub(ctx context.Context) (github, error) {
	if repo := os.Getenv("GITHUB_REPOSITORY"); repo != "" {
		return github{repo: repo}, nil
	}
	repo, err := output(ctx, "", "gh", "repo", "view", "--json", "nameWithOwner", "-q", ".nameWithOwner")
	if err != nil {
		return github{}, err
	}
	return github{repo: repo}, nil
}

// api GETs repos/<repo>/<path> into `into`.
func (hub github) api(ctx context.Context, path string, into any) error {
	out, err := output(ctx, "", "gh", "api", "repos/"+hub.repo+"/"+path)
	if err != nil {
		return err
	}
	if err := json.Unmarshal([]byte(out), into); err != nil {
		return fmt.Errorf("gh api %s: %w", path, err)
	}
	return nil
}

// statuses is every status ever posted on `sha`, newest first.
func (hub github) statuses(ctx context.Context, sha string) ([]verify.Status, error) {
	var statuses []verify.Status
	err := hub.api(ctx, "commits/"+sha+"/statuses?per_page=100", &statuses)
	return statuses, err
}

// current is the `local-ci` entry of `sha`'s combined status, or nil.
func (hub github) current(ctx context.Context, sha string) (*verify.Status, error) {
	var combined struct {
		Statuses []verify.Status `json:"statuses"`
	}
	if err := hub.api(ctx, "commits/"+sha+"/status", &combined); err != nil {
		return nil, err
	}
	for index := range combined.Statuses {
		if combined.Statuses[index].Context == verify.StatusContext {
			return &combined.Statuses[index], nil
		}
	}
	return nil, nil
}

// verification is what GitHub holds for a head: the given body, the head's
// statuses and those of each head the body reuses steps from.
func (hub github) verification(ctx context.Context, body, head string) (verify.Verification, error) {
	current, err := hub.current(ctx, head)
	if err != nil {
		return verify.Verification{}, err
	}
	statuses, err := hub.statuses(ctx, head)
	if err != nil {
		return verify.Verification{}, err
	}
	sources := map[string][]verify.Status{}
	for _, source := range verify.ReusedSources(body) {
		if sources[source], err = hub.statuses(ctx, source); err != nil {
			return verify.Verification{}, err
		}
	}
	return verify.Verification{Body: body, Head: head, Status: current, Statuses: statuses, Sources: sources}, nil
}

// pullRequest is the open pull request of a branch.
type pullRequest struct {
	Number      int            `json:"number"`
	URL         string         `json:"url"`
	Body        string         `json:"body"`
	HeadRefOid  string         `json:"headRefOid"`
	BaseRefName string         `json:"baseRefName"`
	State       string         `json:"state"`
	IsDraft     bool           `json:"isDraft"`
	Mergeable   string         `json:"mergeable"`
	Checks      []verify.Check `json:"statusCheckRollup"`
}

const pullFields = "number,url,body,headRefOid,baseRefName,state,isDraft,mergeable,statusCheckRollup"

// openPull is the branch's open pull request, or nil.
func (hub github) openPull(ctx context.Context, branch string) (*pullRequest, error) {
	if branch == "" {
		return nil, nil
	}
	out, err := output(ctx, "", "gh", "pr", "list", "-R", hub.repo, "--head", branch, "--state", "open", "--json", pullFields)
	if err != nil {
		return nil, err
	}
	var pulls []pullRequest
	if err := json.Unmarshal([]byte(out), &pulls); err != nil {
		return nil, fmt.Errorf("gh pr list: %w", err)
	}
	if len(pulls) == 0 {
		return nil, nil
	}
	return &pulls[0], nil
}

func (hub github) pull(ctx context.Context, number int) (pullRequest, error) {
	out, err := output(ctx, "", "gh", "pr", "view", strconv.Itoa(number), "-R", hub.repo, "--json", pullFields)
	if err != nil {
		return pullRequest{}, err
	}
	var pull pullRequest
	if err := json.Unmarshal([]byte(out), &pull); err != nil {
		return pullRequest{}, fmt.Errorf("gh pr view: %w", err)
	}
	for index, check := range pull.Checks {
		if check.WorkflowName != verify.CIWorkflow || check.Conclusion != "FAILURE" || check.JobID() == 0 {
			continue
		}
		if pull.Checks[index].NotStarted, err = hub.notStarted(ctx, check.JobID()); err != nil {
			return pullRequest{}, err
		}
	}
	return pull, nil
}

// notStarted is whether GitHub failed the job without starting it: its
// annotations say so (verify.NotStartedMessage).
func (hub github) notStarted(ctx context.Context, job int) (bool, error) {
	var annotations []annotation
	if err := hub.api(ctx, "check-runs/"+strconv.Itoa(job)+"/annotations", &annotations); err != nil {
		return false, err
	}
	return slices.ContainsFunc(annotations, func(note annotation) bool { return verify.NotStartedMessage(note.Message) }), nil
}

// annotation is one annotation of a check run.
type annotation struct {
	Message string `json:"message"`
}

func (hub github) postStatus(ctx context.Context, sha, state, description, target string) error {
	args := []string{"api", "--method", "POST", "repos/" + hub.repo + "/statuses/" + sha,
		"-f", "state=" + state, "-f", "context=" + verify.StatusContext, "-f", "description=" + description}
	if target != "" {
		args = append(args, "-f", "target_url="+target)
	}
	_, err := output(ctx, "", "gh", args...)
	return err
}

func (hub github) editBody(ctx context.Context, number int, body string) error {
	dir, err := os.MkdirTemp("", "cilocal-body-")
	if err != nil {
		return fmt.Errorf("body file: %w", err)
	}
	defer func() { _ = os.RemoveAll(dir) }()
	file := filepath.Join(dir, "body.md")
	if err := os.WriteFile(file, []byte(body), fileMode); err != nil {
		return fmt.Errorf("body file: %w", err)
	}
	_, err = output(ctx, "", "gh", "pr", "edit", strconv.Itoa(number), "-R", hub.repo, "--body-file", file)
	return err
}

// attested is the staging gate's verdict on a commit of a deployed branch
// (verify.Attestation): who attested it, or why nobody did.
func (hub github) attested(ctx context.Context, sha string) (string, []string, error) {
	current, err := hub.current(ctx, sha)
	if err != nil {
		return "", nil, err
	}
	state := ""
	if current != nil {
		state = current.State
	}
	var commit struct {
		Commit struct {
			Message string `json:"message"`
		} `json:"commit"`
	}
	if err := hub.api(ctx, "commits/"+sha, &commit); err != nil {
		return "", nil, err
	}
	type mergedPull struct {
		Number         int    `json:"number"`
		MergedAt       string `json:"merged_at"`
		MergeCommitSHA string `json:"merge_commit_sha"`
		Body           string `json:"body"`
		Head           struct {
			SHA string `json:"sha"`
		} `json:"head"`
	}
	var pulls []mergedPull
	if err := hub.api(ctx, "commits/"+sha+"/pulls", &pulls); err != nil {
		return "", nil, err
	}
	merged := []mergedPull{}
	for _, pull := range pulls {
		if pull.MergedAt != "" && pull.MergeCommitSHA == sha {
			merged = append(merged, pull)
		}
	}
	// The commit-to-PR index can trail a fresh merge; the squash title names it.
	if number := verify.TitledPull(commit.Commit.Message); len(merged) == 0 && number != 0 {
		var pull mergedPull
		if err := hub.api(ctx, "pulls/"+strconv.Itoa(number), &pull); err != nil {
			return "", nil, err
		}
		if pull.MergedAt != "" && pull.MergeCommitSHA == sha {
			merged = append(merged, pull)
		}
	}
	judged := []verify.PullAttestation{}
	for _, pull := range merged {
		verification, err := hub.verification(ctx, pull.Body, pull.Head.SHA)
		if err != nil {
			return "", nil, err
		}
		judged = append(judged, verify.PullAttestation{Number: pull.Number, Verification: verification})
	}
	by, reasons := verify.Attestation(sha, state, judged)
	return by, reasons, nil
}

var errNotVerified = errors.New("not verified")

// Permissions of what the tool writes: its cache and report directories,
// reports, bodies and copies, and the gitleaks binary.
const (
	dirMode    = 0o750
	fileMode   = 0o600
	binaryMode = 0o700
)
