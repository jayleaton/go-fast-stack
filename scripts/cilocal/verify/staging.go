package verify

import (
	"encoding/json"
	"fmt"
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
