package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os/exec"
	"strings"
	"time"

	"github.com/jayleaton/go-fast-stack/deploy/platform"
)

// Code source: with repo set, services deploy on push once Railway's GitHub
// app can read the repo. Until then setup deploys the checkout by upload, so
// a project never dead-ends on a missing app install.

// repoAccess asks Railway whether it can read the repo. known is false when
// the API won't say (it refuses GitHub queries for CLI login tokens).
func (setup *setup) repoAccess(ctx context.Context) (known, has bool) {
	var res struct {
		Access struct {
			HasAccess bool `json:"hasAccess"`
		} `json:"gitHubRepoAccessAvailable"`
	}
	err := setup.rw.gql(ctx, `query($repo: String!) { gitHubRepoAccessAvailable(fullRepoName: $repo) { hasAccess } }`,
		map[string]any{"repo": setup.cfg.Repo}, &res)
	if err != nil {
		return false, false
	}
	return true, res.Access.HasAccess
}

// repoConnected reports whether the web service already builds from the repo
// in any environment (a previous run connected it).
func (setup *setup) repoConnected(ctx context.Context) bool {
	if setup.project == nil || setup.project.Services[platform.ServiceWeb] == "" {
		return false
	}
	for _, env := range setup.cfg.Environments {
		if setup.project.Environments[env] == "" {
			continue
		}
		if repo, _ := setup.instanceSource(ctx, platform.ServiceWeb, env); strings.EqualFold(repo, setup.cfg.Repo) {
			return true
		}
	}
	return false
}

func (setup *setup) instanceSource(ctx context.Context, svc, env string) (repo string, err error) {
	var cfg struct {
		Services map[string]struct {
			Source struct {
				Repo string `json:"repo"`
			} `json:"source"`
		} `json:"services"`
	}
	if err := setup.rw.json(ctx, &cfg, "environment", "config", "--environment", env); err != nil {
		return "", err
	}
	return cfg.Services[setup.project.Services[svc]].Source.Repo, nil
}

// connectRepo connects the web service to the repo: the real access check
// when repoAccess can't tell. Railway answers with a GraphQL error when its
// GitHub app cannot read the repo, or when the branch does not exist;
// nothing changes then. It connects production's branch (the last
// environment's), which a repo has from its first push: staging's dev may
// not exist yet, and IaC sets each environment's own branch afterwards.
func (setup *setup) connectRepo(ctx context.Context) error {
	id := setup.project.Services[platform.ServiceWeb]
	return setup.rw.gql(ctx, `mutation($id: String!, $input: ServiceConnectInput!) { serviceConnect(id: $id, input: $input) { id } }`,
		map[string]any{"id": id, "input": map[string]any{"repo": setup.cfg.Repo, "branch": setup.cfg.BranchFor(setup.cfg.Environments[len(setup.cfg.Environments)-1])}}, nil)
}

// checkBranches says which environment deploys which branch, and warns
// about a branch the repo does not have yet: Railway's trigger is set
// anyway, but nothing deploys there until the branch is pushed. Asked with
// git ls-remote (public repos need no login; private ones use your git
// credentials); when git cannot tell, nothing is reported.
func (setup *setup) checkBranches(ctx context.Context) {
	if setup.cfg.Repo == "" {
		return
	}
	var mapping, branches []string
	deploys := map[string][]string{} // branch -> the environments that deploy it
	for _, env := range setup.cfg.Environments {
		branch := setup.cfg.BranchFor(env)
		mapping = append(mapping, env+" <- "+branch)
		if deploys[branch] == nil {
			branches = append(branches, branch)
		}
		deploys[branch] = append(deploys[branch], env)
	}
	setup.say("branches: %s (deploy on push to github.com/%s)", strings.Join(mapping, ", "), setup.cfg.Repo)
	for _, branch := range branches {
		if !errors.Is(branchExists(ctx, setup.cfg.Repo, branch), errNoBranch) {
			continue
		}
		envs := deploys[branch]
		setup.say("  ! %s deploys branch %s, which github.com/%s does not have yet: nothing deploys there until it is pushed", strings.Join(envs, ", "), branch, setup.cfg.Repo)
		setup.next("Create branch %s, which %s deploys: git push origin %s:%s (or set branches.%s in platform.yaml)",
			branch, strings.Join(envs, ", "), setup.cfg.Branch, branch, envs[0])
	}
}

var errNoBranch = errors.New("no such branch")

const (
	// branchLookupTimeout bounds one git ls-remote against GitHub.
	branchLookupTimeout = 15 * time.Second
	// lsRemoteNoMatch is git ls-remote --exit-code's status when the repo
	// answered but has no matching ref.
	lsRemoteNoMatch = 2
)

// branchExists asks GitHub for refs/heads/<branch>: nil when it exists,
// errNoBranch when the repo answers without it, another error when git
// cannot reach the repo (no network, a private repo without credentials).
func branchExists(ctx context.Context, repo, branch string) error {
	ctx, cancel := context.WithTimeout(ctx, branchLookupTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", "ls-remote", "--exit-code", "--heads", "https://github.com/"+repo+".git", "refs/heads/"+branch)
	cmd.Env = append(cmd.Environ(), "GIT_TERMINAL_PROMPT=0")
	err := cmd.Run()
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) && exitErr.ExitCode() == lsRemoteNoMatch {
		return errNoBranch
	}
	return err
}

// installURL is where the repo owner installs Railway's GitHub app, with the
// owner's account preselected when GitHub's public API answers.
func installURL(ctx context.Context, repo string) string {
	const base = "https://github.com/apps/railway-app/installations/new"
	const lookupTimeout = 5 * time.Second // the owner lookup is a nicety; never hold setup up for it
	owner, _, _ := strings.Cut(repo, "/")
	ctx, cancel := context.WithTimeout(ctx, lookupTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://api.github.com/users/"+owner, nil)
	if err != nil {
		return base
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return base
	}
	defer resp.Body.Close()
	var item struct {
		ID int64 `json:"id"`
	}
	if resp.StatusCode != http.StatusOK || json.NewDecoder(resp.Body).Decode(&item) != nil || item.ID == 0 {
		return base
	}
	return fmt.Sprintf("%s/permissions?target_id=%d", base, item.ID)
}
