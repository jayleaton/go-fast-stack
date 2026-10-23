package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

// Exit codes, for scripts and agents (documented in AGENTS.md).
const (
	exitOK        = 0
	exitError     = 1 // anything else: read the message
	exitPending   = 2 // plan: changes are pending
	exitConfirm   = 3 // a step needs --yes (creating a project) and there is no terminal to ask
	exitPreflight = 4 // doctor: a required check failed; the output says how to fix it
	exitUnhealthy = 5 // applied, but a public service is not healthy
)

// exitErr carries an exit code with its message.
type exitErr struct {
	code int
	err  error
}

func (e exitErr) Error() string { return e.err.Error() }
func (e exitErr) Unwrap() error { return e.err }

func withCode(code int, format string, a ...any) error {
	return exitErr{code, fmt.Errorf(format, a...)}
}

func exitCode(err error) int {
	if err == nil {
		return exitOK
	}
	var e exitErr
	if errors.As(err, &e) {
		return e.code
	}
	return exitError
}

// summary is the --json output: what exists, whether it is healthy, and
// what a person has to do next. Never holds secret values.
type summary struct {
	OK           bool         `json:"ok"`
	Command      string       `json:"command"`
	Project      *projectInfo `json:"project,omitempty"`
	Source       string       `json:"source,omitempty"` // github (deploys on push) | upload (railway up)
	Repo         string       `json:"repo,omitempty"`
	Environments []envInfo    `json:"environments,omitempty"`
	Pending      int          `json:"pendingChanges"`
	Checks       []check      `json:"checks,omitempty"` // doctor
	NextSteps    []string     `json:"nextSteps"`
	Error        string       `json:"error,omitempty"`
	ExitCode     int          `json:"exitCode"`
}

type projectInfo struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Workspace string `json:"workspace"`
	URL       string `json:"url"`
}

type envInfo struct {
	Name     string        `json:"name"`
	Branch   string        `json:"branch,omitempty"` // the git branch it deploys on push (source github)
	Services []serviceInfo `json:"services"`
}

type serviceInfo struct {
	Name       string `json:"name"`
	URL        string `json:"url,omitempty"`
	Deployment string `json:"deployment,omitempty"` // latest Railway deployment status
	Health     string `json:"health"`               // ok | unhealthy | not deployed | unknown
}

// next records a next step once.
func (s *setup) next(format string, a ...any) {
	step := fmt.Sprintf(format, a...)
	for _, n := range s.sum.NextSteps {
		if n == step {
			return
		}
	}
	s.sum.NextSteps = append(s.sum.NextSteps, step)
}

func (s *setup) finish(err error) int {
	code := exitCode(err)
	s.sum.ExitCode, s.sum.OK = code, err == nil
	if s.sum.NextSteps == nil {
		s.sum.NextSteps = []string{}
	}
	if err != nil {
		s.sum.Error = err.Error()
	}
	if s.project != nil {
		s.sum.Project = &projectInfo{ID: s.project.ID, Name: s.project.Name, Workspace: s.project.Workspace,
			URL: "https://railway.com/project/" + s.project.ID}
	}
	if s.jsonOut {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		_ = enc.Encode(s.sum)
		return code
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "railway-setup:", err)
	}
	if len(s.sum.NextSteps) > 0 {
		s.say("\nNext steps:")
		for i, n := range s.sum.NextSteps {
			s.say("  %d. %s", i+1, n)
		}
	}
	return code
}

// deploymentStatus is the latest deployment's status ("" when none).
func (s *setup) deploymentStatus(ctx context.Context, svc, env string) (string, error) {
	_, status, err := s.latestDeployment(ctx, svc, env)
	return status, err
}

// latestDeployment reads the newest deployment. A read is retried twice:
// Railway's API sometimes answers a body the CLI cannot decode.
func (s *setup) latestDeployment(ctx context.Context, svc, env string) (id, status string, err error) {
	var deps []struct {
		ID     string `json:"id"`
		Status string `json:"status"`
	}
	for attempt := range 3 {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return "", "", ctx.Err()
			case <-time.After(10 * time.Second):
			}
		}
		if err = s.rw.json(ctx, &deps, "deployment", "list", "--service", svc, "--environment", env); err == nil {
			break
		}
	}
	if err != nil {
		return "", "", err
	}
	if len(deps) == 0 {
		return "", "", nil
	}
	return deps[0].ID, deps[0].Status, nil
}

// settled reports whether a deployment status will not change by itself.
func settled(status string) bool {
	switch status {
	case "", "SUCCESS", "FAILED", "CRASHED", "REMOVED", "SKIPPED":
		return true
	}
	return false
}

// health waits (up to wait) for every public service's latest deployment
// to settle, then checks /healthz on its public domain. It fills the
// summary and returns an exitUnhealthy error if any service is not ok.
func (s *setup) health(ctx context.Context, wait time.Duration) error {
	if s.project == nil {
		return errors.New("the project does not exist yet")
	}
	client := &http.Client{Timeout: 10 * time.Second}
	deadline := time.Now().Add(wait)
	var bad []string
	s.sum.Environments = nil
	for _, env := range s.cfg.Environments {
		info := envInfo{Name: env}
		if !s.cfg.Upload() {
			info.Branch = s.cfg.BranchFor(env)
		}
		for _, svc := range s.cfg.PublicServices() {
			si := serviceInfo{Name: svc, Health: "unknown"}
			if s.project.Environments[env] == "" || s.project.Services[svc] == "" {
				si.Health = "not deployed"
				bad = append(bad, env+"/"+svc+": missing")
				info.Services = append(info.Services, si)
				continue
			}
			status, err := s.deploymentStatus(ctx, svc, env)
			for err == nil && !settled(status) && time.Now().Before(deadline) {
				select {
				case <-ctx.Done():
					return ctx.Err()
				case <-time.After(10 * time.Second):
				}
				status, err = s.deploymentStatus(ctx, svc, env)
			}
			si.Deployment = status
			if d, _ := s.domain(ctx, svc, env); d != "" {
				si.URL = "https://" + d
			}
			switch {
			case err != nil:
				bad = append(bad, fmt.Sprintf("%s/%s: %v", env, svc, err))
			case status == "":
				si.Health = "not deployed"
				bad = append(bad, env+"/"+svc+": no deployment")
			case si.URL == "":
				bad = append(bad, env+"/"+svc+": no public domain")
			default:
				code, body := healthz(ctx, client, si.URL)
				if code == http.StatusOK {
					si.Health = "ok"
				} else {
					si.Health = "unhealthy"
					bad = append(bad, fmt.Sprintf("%s/%s: deployment %s, /healthz %d %s", env, svc, status, code, body))
				}
			}
			s.say("  %-11s %-6s %-9s %s/healthz %s", env, svc, si.Health, si.URL, strings.ToLower(status))
			info.Services = append(info.Services, si)
		}
		s.sum.Environments = append(s.sum.Environments, info)
	}
	if len(bad) > 0 {
		s.next("Check the failing deployments' logs: railway logs --service <service> --environment <env>")
		return withCode(exitUnhealthy, "unhealthy: %s", strings.Join(bad, "; "))
	}
	return nil
}

// healthz retries briefly: a fresh deployment can take a few seconds to be
// routed after Railway reports it live.
func healthz(ctx context.Context, client *http.Client, url string) (int, string) {
	code, body := 0, ""
	for attempt := range 4 {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return code, body
			case <-time.After(5 * time.Second):
			}
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url+"/healthz", nil)
		if err != nil {
			return 0, err.Error()
		}
		resp, err := client.Do(req)
		if err != nil {
			body = err.Error()
			continue
		}
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 200))
		_ = resp.Body.Close()
		code, body = resp.StatusCode, strings.TrimSpace(string(b))
		if code == http.StatusOK {
			break
		}
	}
	return code, body
}
