package verify

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
)

// After a merge (make pr-merge ARGS="--check-deploy <sha>"): did staging
// deploy that commit, and is it ready? One read of the commit's "Deploy
// staging" runs and of the staging web's /healthz. A follow-up, never a
// gate: the next PR merges meanwhile.

// DeployWorkflow is deploy-staging.yml's name.
const DeployWorkflow = "Deploy staging"

// DeployRun is one run of Deploy staging for a commit (gh run list).
type DeployRun struct {
	Name       string `json:"workflowName"`
	Status     string `json:"status"`
	Conclusion string `json:"conclusion"`
	URL        string `json:"url"`
	// DryRun is a run whose deploy job was skipped and whose dry-run job
	// ran (RAILWAY_DEPLOY is not true): it deployed nothing.
	DryRun bool `json:"-"`
}

// The jobs of deploy-staging.yml that DryRunJobs reads.
const (
	deployJob = "Deploy main to Railway staging"
	dryRunJob = "Dry run (RAILWAY_DEPLOY is not true)"
)

// Job is one job of a workflow run (gh run view --json jobs).
type Job struct {
	Name       string `json:"name"`
	Conclusion string `json:"conclusion"`
}

// DryRunJobs is whether a Deploy staging run's jobs are a dry run: the
// dry-run job ran and the deploy job did not.
func DryRunJobs(jobs []Job) bool {
	dry, deployed := false, false
	for _, job := range jobs {
		switch job.Name {
		case dryRunJob:
			dry = job.Conclusion == "success"
		case deployJob:
			deployed = job.Conclusion != "skipped" && job.Conclusion != ""
		}
	}
	return dry && !deployed
}

// Readiness is the staging web's /healthz answer: unset URL, a transport
// error, or the status code and its `data.status`.
type Readiness struct {
	URL    string
	Code   int
	Status string
	Err    string
}

// readyCode is the /healthz status code of a ready server.
const readyCode = 200

// ReadinessOf reads a /healthz answer: `{"data":{"status":"ok"}}` with 200
// is ready.
func ReadinessOf(url string, code int, body []byte) Readiness {
	var answer struct {
		Data struct {
			Status string `json:"status"`
		} `json:"data"`
	}
	_ = json.Unmarshal(body, &answer) // any other body is simply not ready
	return Readiness{URL: url, Code: code, Status: answer.Data.Status, Err: ""}
}

// Ready is whether staging answered ready.
func (ready Readiness) Ready() bool { return ready.Code == readyCode && ready.Status == "ok" }

func (ready Readiness) String() string {
	switch {
	case ready.URL == "":
		return "readiness unknown: STAGING_WEB_URL is unset (the repository variable, or the env)"
	case ready.Err != "":
		return fmt.Sprintf("%s/healthz unreachable: %s", ready.URL, ready.Err)
	case ready.Ready():
		return fmt.Sprintf("%s/healthz 200 ok", ready.URL)
	}
	return fmt.Sprintf("%s/healthz answered %d with data.status %q", ready.URL, ready.Code, ready.Status)
}

// StagingVerdict is the verdict on one main commit.
type StagingVerdict struct {
	Settled bool // nothing is still running
	OK      bool
	Summary string
}

// CheckStagingStep is the command that checks main's head instead.
const CheckStagingStep = `make pr-merge ARGS=--check-staging`

// Staging is the staging verdict for main commit `head` from its Deploy
// staging runs and the readiness answer. A run cancelled by a newer main
// commit's deploy is superseded: that deploy carries this commit, so main's
// head is the one to check.
func Staging(head string, deploys []DeployRun, ready Readiness) StagingVerdict {
	at := "main " + Short(head)
	superseded, running, failed, dry := []string{}, 0, []string{}, 0
	for _, run := range deploys {
		switch {
		case run.Status != "completed":
			running++
		case run.Conclusion == "cancelled":
			superseded = append(superseded, run.URL)
		case run.Conclusion != "success" && run.Conclusion != "skipped":
			failed = append(failed, fmt.Sprintf("%s %s %s", run.Name, run.Conclusion, run.URL))
		case run.DryRun:
			dry++
		}
	}
	switch {
	case len(superseded) > 0:
		return StagingVerdict{Settled: true, OK: false,
			Summary: fmt.Sprintf("%s: Deploy staging cancelled by a newer main deploy (%s); check that one: %s", at, strings.Join(superseded, ", "), CheckStagingStep)}
	case running > 0 || len(deploys) == 0:
		state := "not started"
		if running > 0 {
			state = "running"
		}
		return StagingVerdict{Settled: false, OK: false, Summary: fmt.Sprintf("%s: deploy %s", at, state)}
	case len(failed) > 0:
		return StagingVerdict{Settled: true, OK: false,
			Summary: fmt.Sprintf("%s: %s. A failed staging deploy is the top priority: fix it forward before merging anything else", at, strings.Join(failed, "; "))}
	case dry > 0:
		return StagingVerdict{Settled: true, OK: true,
			Summary: at + ": dry run: nothing deployed (set the RAILWAY_DEPLOY repository variable to true to deploy, docs/RELEASES.md)"}
	case !ready.Ready():
		return StagingVerdict{Settled: true, OK: false, Summary: fmt.Sprintf("%s: deployed; %s (top priority)", at, ready)}
	}
	return StagingVerdict{Settled: true, OK: true, Summary: fmt.Sprintf("%s: deployed; %s", at, ready)}
}

// When no Deploy staging run exists for a commit (Actions off, or a deploy
// by hand with make railway-up), Railway itself says what staging runs:
// each app service's newest deployment, whose message make railway-up
// writes as "make railway-up <commit>" from a clean checkout.

// RailwayDeploy is one staging app service's newest Railway deployment.
type RailwayDeploy struct {
	Service string
	Status  string // SUCCESS, FAILED, CRASHED, BUILDING, DEPLOYING, ...
	Message string // the deployment's message (railway up -m)
}

// railwayUpCommit is the message make railway-up gives a deployment of a
// clean checkout.
var railwayUpCommit = regexp.MustCompile(`^make railway-up ([0-9a-f]{40})$`)

// DeployedCommit is the commit a deployment message records, or "" when it
// records none (an older make railway-up, a dirty checkout, a hand deploy).
func DeployedCommit(message string) string {
	match := railwayUpCommit.FindStringSubmatch(strings.TrimSpace(message))
	if match == nil {
		return ""
	}
	return match[1]
}

// railwayRunning are deployment states that have not settled.
var railwayRunning = map[string]bool{"INITIALIZING": true, "QUEUED": true, "WAITING": true, "BUILDING": true, "DEPLOYING": true}

// RedeployStep is how staging gets a deployment that records its commit.
const RedeployStep = "make railway-up PLATFORM_ENV=staging from a clean checkout of the commit (it records the commit)"

// StagingFromRailway is the staging verdict for main commit `head` from the
// staging app services' newest Railway deployments and their /healthz
// answers: OK only when every service runs exactly `head` and every
// readiness answer is ready.
func StagingFromRailway(head string, services []RailwayDeploy, ready []Readiness) StagingVerdict {
	at := "main " + Short(head)
	if len(services) == 0 {
		return StagingVerdict{Settled: true, OK: false, Summary: at + ": no Deploy staging run, and Railway lists no staging app deployment"}
	}
	running, failed, unknown, other, runs := []string{}, []string{}, []string{}, []string{}, []string{}
	for _, service := range services {
		commit := DeployedCommit(service.Message)
		switch {
		case railwayRunning[service.Status]:
			running = append(running, service.Service)
		case service.Status != "SUCCESS":
			failed = append(failed, service.Service+" "+strings.ToLower(service.Status))
		case commit == "":
			unknown = append(unknown, fmt.Sprintf("%s (%q)", service.Service, service.Message))
		case commit != head:
			other = append(other, service.Service+" runs "+Short(commit))
		default:
			runs = append(runs, service.Service)
		}
	}
	switch {
	case len(running) > 0:
		return StagingVerdict{Settled: false, OK: false, Summary: fmt.Sprintf("%s: Railway deploy running (%s)", at, strings.Join(running, ", "))}
	case len(failed) > 0:
		return StagingVerdict{Settled: true, OK: false,
			Summary: fmt.Sprintf("%s: Railway deploy %s. A failed staging deploy is the top priority: fix it forward before merging anything else", at, strings.Join(failed, ", "))}
	case len(other) > 0:
		return StagingVerdict{Settled: true, OK: false, Summary: fmt.Sprintf("%s: not deployed: %s", at, strings.Join(other, ", "))}
	case len(unknown) > 0:
		return StagingVerdict{Settled: true, OK: false,
			Summary: fmt.Sprintf("%s: cannot tell: the staging deployment of %s records no commit; %s", at, strings.Join(unknown, ", "), RedeployStep)}
	}
	answers := []string{}
	ok := true
	for _, answer := range ready {
		answers = append(answers, answer.String())
		ok = ok && answer.Ready()
	}
	summary := fmt.Sprintf("%s: Railway runs it on %s (no Deploy staging run); %s", at, strings.Join(runs, " and "), strings.Join(answers, "; "))
	if !ok {
		return StagingVerdict{Settled: true, OK: false, Summary: summary + " (top priority)"}
	}
	return StagingVerdict{Settled: true, OK: true, Summary: summary}
}
