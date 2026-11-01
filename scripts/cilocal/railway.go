package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path"
	"regexp"
	"strings"

	"github.com/jayleaton/go-fast-stack/scripts/cilocal/verify"
)

// What staging runs, asked of Railway when no Deploy staging run exists for
// a commit (verify.StagingFromRailway): the railway CLI, logged in or with
// a RAILWAY_TOKEN, read only.

// stagingEnvironment and stagingServices are the Railway environment and
// the app services in it whose deployments say which commit staging runs
// (deploy/platform.yaml; admin only where the project has it).
const stagingEnvironment = "staging"

var stagingServices = []string{"web", "admin"}

// platformFile names the Railway project (`project: <name>`).
const platformFile = "deploy/platform.yaml"

var projectLine = regexp.MustCompile(`(?m)^project:\s*(\S.*?)\s*$`)

// railwayProject is the Railway project's id: RAILWAY_PROJECT_ID, else the
// project deploy/platform.yaml names (`auto`: the GitHub repository's name,
// as make railway-apply names it), found with `railway list`.
func railwayProject(ctx context.Context, repo string) (string, error) {
	if id := os.Getenv("RAILWAY_PROJECT_ID"); id != "" {
		return id, nil
	}
	config, err := os.ReadFile(platformFile)
	if err != nil {
		return "", fmt.Errorf("railway: %w", err)
	}
	match := projectLine.FindSubmatch(config)
	if match == nil {
		return "", fmt.Errorf("railway: %s names no project", platformFile)
	}
	name := strings.Trim(string(match[1]), `"'`)
	if name == "auto" {
		name = path.Base(repo)
	}
	out, err := output(ctx, "", "railway", "list", "--json")
	if err != nil {
		return "", err
	}
	var projects []struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	}
	if err := json.Unmarshal([]byte(out), &projects); err != nil {
		return "", fmt.Errorf("railway list: %w", err)
	}
	for _, project := range projects {
		if project.Name == name {
			return project.ID, nil
		}
	}
	return "", fmt.Errorf("railway: no project %q visible to this login (railway login, or RAILWAY_PROJECT_ID)", name)
}

// railwayAPI runs one GraphQL query through `railway api`.
func railwayAPI(ctx context.Context, query string, variables map[string]string, into any) error {
	input, err := json.Marshal(variables)
	if err != nil {
		return fmt.Errorf("railway api: %w", err)
	}
	out, err := outputIn(ctx, string(input), "railway", "api", query, "--variables", "@-")
	var answer struct {
		Data   json.RawMessage `json:"data"`
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	if jsonErr := json.Unmarshal([]byte(out), &answer); jsonErr != nil {
		return errors.Join(err, fmt.Errorf("railway api: %w", jsonErr))
	}
	if len(answer.Errors) > 0 {
		return fmt.Errorf("railway api: %s", answer.Errors[0].Message)
	}
	if err := json.Unmarshal(answer.Data, into); err != nil {
		return fmt.Errorf("railway api: %w", err)
	}
	return nil
}

type edges[Node any] struct {
	Edges []struct {
		Node Node `json:"node"`
	} `json:"edges"`
}

type named struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// railwayStaging is the newest deployment of each staging app service.
func railwayStaging(ctx context.Context, repo string) ([]verify.RailwayDeploy, error) {
	project, err := railwayProject(ctx, repo)
	if err != nil {
		return nil, err
	}
	var layout struct {
		Project struct {
			Environments edges[named] `json:"environments"`
			Services     edges[named] `json:"services"`
		} `json:"project"`
	}
	if err := railwayAPI(ctx, `query($p: String!) { project(id: $p) { environments { edges { node { id name } } } services { edges { node { id name } } } } }`,
		map[string]string{"p": project}, &layout); err != nil {
		return nil, err
	}
	environment := ""
	for _, edge := range layout.Project.Environments.Edges {
		if edge.Node.Name == stagingEnvironment {
			environment = edge.Node.ID
		}
	}
	if environment == "" {
		return nil, fmt.Errorf("railway: the project has no %s environment", stagingEnvironment)
	}
	deploys := []verify.RailwayDeploy{}
	for _, edge := range layout.Project.Services.Edges {
		if !contains(stagingServices, edge.Node.Name) {
			continue
		}
		var list struct {
			Deployments edges[struct {
				Status string `json:"status"`
				Meta   struct {
					Message string `json:"cliMessage"`
				} `json:"meta"`
			}] `json:"deployments"`
		}
		if err := railwayAPI(ctx, `query($p: String!, $e: String!, $s: String!) { deployments(first: 1, input: {projectId: $p, environmentId: $e, serviceId: $s}) { edges { node { status meta } } } }`,
			map[string]string{"p": project, "e": environment, "s": edge.Node.ID}, &list); err != nil {
			return nil, err
		}
		if len(list.Deployments.Edges) == 0 {
			continue
		}
		newest := list.Deployments.Edges[0].Node
		deploys = append(deploys, verify.RailwayDeploy{Service: edge.Node.Name, Status: newest.Status, Message: newest.Meta.Message})
	}
	return deploys, nil
}

func contains(list []string, item string) bool {
	for _, entry := range list {
		if entry == item {
			return true
		}
	}
	return false
}

// railwayFallback is the staging verdict from Railway, for a commit with no
// Deploy staging run; when Railway cannot be read, `without` stands, with
// the reason.
func railwayFallback(ctx context.Context, hub github, head string, without verify.StagingVerdict) verify.StagingVerdict {
	deploys, err := railwayStaging(ctx, hub.repo)
	if err != nil {
		without.Summary += fmt.Sprintf("; no Deploy staging run, and Railway could not be read: %s", strings.TrimSpace(err.Error()))
		return without
	}
	ready := []verify.Readiness{readiness(ctx, hub, "STAGING_WEB_URL")}
	for _, deploy := range deploys {
		if deploy.Service == "admin" {
			ready = append(ready, readiness(ctx, hub, "STAGING_ADMIN_URL"))
		}
	}
	return verify.StagingFromRailway(head, deploys, ready)
}

// outputIn runs a command with `input` on stdin and returns its stdout even
// when it fails: `railway api` prints the GraphQL errors there, exit 1.
func outputIn(ctx context.Context, input, name string, args ...string) (string, error) {
	command := exec.CommandContext(ctx, name, args...)
	command.Stdin = strings.NewReader(input)
	var stderr bytes.Buffer
	command.Stderr = &stderr
	out, err := command.Output()
	if err != nil {
		err = fmt.Errorf("%s %s: %w: %s", name, args[0], err, strings.TrimSpace(stderr.String()))
	}
	return string(out), err
}
