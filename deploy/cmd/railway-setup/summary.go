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

func (err exitErr) Error() string { return err.err.Error() }
func (err exitErr) Unwrap() error { return err.err }

func withCode(code int, format string, values ...any) error {
	return exitErr{code, fmt.Errorf(format, values...)}
}

func exitCode(err error) int {
	if err == nil {
		return exitOK
	}
	var coded exitErr
	if errors.As(err, &coded) {
		return coded.code
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
func (setup *setup) next(format string, values ...any) {
	step := fmt.Sprintf(format, values...)
	for _, nextStep := range setup.sum.NextSteps {
		if nextStep == step {
			return
		}
	}
	setup.sum.NextSteps = append(setup.sum.NextSteps, step)
}

func (setup *setup) finish(err error) int {
	code := exitCode(err)
	setup.sum.ExitCode, setup.sum.OK = code, err == nil
	if setup.sum.NextSteps == nil {
		setup.sum.NextSteps = []string{}
	}
	if err != nil {
		setup.sum.Error = err.Error()
	}
	if setup.project != nil {
		setup.sum.Project = &projectInfo{ID: setup.project.ID, Name: setup.project.Name, Workspace: setup.project.Workspace,
			URL: "https://railway.com/project/" + setup.project.ID}
	}
	if setup.jsonOut {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		_ = enc.Encode(setup.sum)
		return code
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "railway-setup:", err)
	}
	if len(setup.sum.NextSteps) > 0 {
		setup.say("\nNext steps:")
		for index, nextStep := range setup.sum.NextSteps {
			setup.say("  %d. %s", index+1, nextStep)
		}
	}
	return code
}

// deploymentStatus is the latest deployment's status ("" when none).
func (setup *setup) deploymentStatus(ctx context.Context, svc, env string) (string, error) {
	_, status, err := setup.latestDeployment(ctx, svc, env)
	return status, err
}

// deployment is one entry of railway deployment list, newest first.
type deployment struct {
	ID     string `json:"id"`
	Status string `json:"status"`
}

// deploymentReadAttempts and deploymentRetryDelay: Railway's API sometimes
// answers a body the CLI cannot decode, so a read is retried.
const (
	deploymentReadAttempts = 3
	deploymentRetryDelay   = 10 * time.Second
)

// deployments lists svc's deployments in env, newest first.
func (setup *setup) deployments(ctx context.Context, svc, env string) ([]deployment, error) {
	var list []deployment
	var err error
	for attempt := range deploymentReadAttempts {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(deploymentRetryDelay):
			}
		}
		if err = setup.rw.json(ctx, &list, "deployment", "list", "--service", svc, "--environment", env); err == nil {
			return list, nil
		}
	}
	return nil, err
}

// latestDeployment reads the newest deployment ("" when none).
func (setup *setup) latestDeployment(ctx context.Context, svc, env string) (id, status string, err error) {
	list, err := setup.deployments(ctx, svc, env)
	if err != nil || len(list) == 0 {
		return "", "", err
	}
	return list[0].ID, list[0].Status, nil
}

// latestBuilt reads the newest deployment Railway did not skip. A SKIPPED
// one (a push outside the watch paths) never replaces the running
// deployment, and one can land while a variable change is still building.
func (setup *setup) latestBuilt(ctx context.Context, svc, env string) (id, status string, err error) {
	list, err := setup.deployments(ctx, svc, env)
	if err != nil {
		return "", "", err
	}
	for _, entry := range list {
		if entry.Status != "SKIPPED" {
			return entry.ID, entry.Status, nil
		}
	}
	return "", "", nil
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
func (setup *setup) health(ctx context.Context, wait time.Duration) error {
	const healthTimeout = 10 * time.Second // per /healthz request
	if setup.project == nil {
		return errors.New("the project does not exist yet")
	}
	client := &http.Client{Timeout: healthTimeout}
	deadline := time.Now().Add(wait)
	var bad []string
	setup.sum.Environments = nil
	for _, env := range setup.cfg.Environments {
		info := envInfo{Name: env}
		if !setup.cfg.Upload() {
			info.Branch = setup.cfg.BranchFor(env)
		}
		for _, svc := range setup.cfg.PublicServices() {
			si := serviceInfo{Name: svc, Health: "unknown"}
			if setup.project.Environments[env] == "" || setup.project.Services[svc] == "" {
				si.Health = "not deployed"
				bad = append(bad, env+"/"+svc+": missing")
				info.Services = append(info.Services, si)
				continue
			}
			status, err := setup.deploymentStatus(ctx, svc, env)
			for err == nil && !settled(status) && time.Now().Before(deadline) {
				select {
				case <-ctx.Done():
					return ctx.Err()
				case <-time.After(pollInterval):
				}
				status, err = setup.deploymentStatus(ctx, svc, env)
			}
			si.Deployment = status
			if domain, _ := setup.domain(ctx, svc, env); domain != "" {
				si.URL = "https://" + domain
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
			setup.say("  %-11s %-6s %-9s %s/healthz %s", env, svc, si.Health, si.URL, strings.ToLower(status))
			info.Services = append(info.Services, si)
		}
		setup.sum.Environments = append(setup.sum.Environments, info)
	}
	if len(bad) > 0 {
		setup.next("Check the failing deployments' logs: railway logs --service <service> --environment <env>")
		return withCode(exitUnhealthy, "unhealthy: %s", strings.Join(bad, "; "))
	}
	return nil
}

// healthz retries briefly: a fresh deployment can take a few seconds to be
// routed after Railway reports it live.
func healthz(ctx context.Context, client *http.Client, url string) (int, string) {
	const (
		healthRetryDelay = 5 * time.Second // between /healthz attempts
		maxHealthBody    = 200             // bytes of the /healthz body kept for the summary
	)
	code, body := 0, ""
	for attempt := range 4 {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return code, body
			case <-time.After(healthRetryDelay):
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
		data, _ := io.ReadAll(io.LimitReader(resp.Body, maxHealthBody))
		_ = resp.Body.Close()
		code, body = resp.StatusCode, strings.TrimSpace(string(data))
		if code == http.StatusOK {
			break
		}
	}
	return code, body
}
