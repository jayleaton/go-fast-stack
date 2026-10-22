package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/jayleaton/go-fast-stack/deploy/platform"
)

// Code source: with repo set, services deploy on push once Railway's GitHub
// app can read the repo. Until then setup deploys the checkout by upload, so
// a project never dead-ends on a missing app install.

// repoAccess asks Railway whether it can read the repo. known is false when
// the API won't say (it refuses GitHub queries for CLI login tokens).
func (s *setup) repoAccess(ctx context.Context) (known, has bool) {
	var res struct {
		Access struct {
			HasAccess bool `json:"hasAccess"`
		} `json:"gitHubRepoAccessAvailable"`
	}
	err := s.rw.gql(ctx, `query($repo: String!) { gitHubRepoAccessAvailable(fullRepoName: $repo) { hasAccess } }`,
		map[string]any{"repo": s.cfg.Repo}, &res)
	if err != nil {
		return false, false
	}
	return true, res.Access.HasAccess
}

// repoConnected reports whether the web service already builds from the repo
// in any environment (a previous run connected it).
func (s *setup) repoConnected(ctx context.Context) bool {
	if s.project == nil || s.project.Services[platform.ServiceWeb] == "" {
		return false
	}
	for _, env := range s.cfg.Environments {
		if s.project.Environments[env] == "" {
			continue
		}
		if repo, _ := s.instanceSource(ctx, platform.ServiceWeb, env); strings.EqualFold(repo, s.cfg.Repo) {
			return true
		}
	}
	return false
}

func (s *setup) instanceSource(ctx context.Context, svc, env string) (repo string, err error) {
	var cfg struct {
		Services map[string]struct {
			Source struct {
				Repo string `json:"repo"`
			} `json:"source"`
		} `json:"services"`
	}
	if err := s.rw.json(ctx, &cfg, "environment", "config", "--environment", env); err != nil {
		return "", err
	}
	return cfg.Services[s.project.Services[svc]].Source.Repo, nil
}

// connectRepo connects the web service to the repo: the real access check
// when repoAccess can't tell. Railway answers with a GraphQL error when its
// GitHub app cannot read the repo; nothing changes then.
func (s *setup) connectRepo(ctx context.Context) error {
	id := s.project.Services[platform.ServiceWeb]
	return s.rw.gql(ctx, `mutation($id: String!, $input: ServiceConnectInput!) { serviceConnect(id: $id, input: $input) { id } }`,
		map[string]any{"id": id, "input": map[string]any{"repo": s.cfg.Repo, "branch": s.cfg.BranchFor(s.cfg.Environments[0])}}, nil)
}

// installURL is where the repo owner installs Railway's GitHub app, with the
// owner's account preselected when GitHub's public API answers.
func installURL(ctx context.Context, repo string) string {
	const base = "https://github.com/apps/railway-app/installations/new"
	owner, _, _ := strings.Cut(repo, "/")
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
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
	var u struct {
		ID int64 `json:"id"`
	}
	if resp.StatusCode != http.StatusOK || json.NewDecoder(resp.Body).Decode(&u) != nil || u.ID == 0 {
		return base
	}
	return fmt.Sprintf("%s/permissions?target_id=%d", base, u.ID)
}
