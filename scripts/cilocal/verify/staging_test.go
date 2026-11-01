package verify_test

import (
	"strings"
	"testing"

	"github.com/jayleaton/go-fast-stack/scripts/cilocal/verify"
)

func TestDryRunJobs(test *testing.T) {
	test.Parallel()
	plan := verify.Job{Name: "Plan the staging services", Conclusion: "success"}
	deploy := func(conclusion string) verify.Job {
		return verify.Job{Name: "Deploy main to Railway staging", Conclusion: conclusion}
	}
	dry := func(conclusion string) verify.Job {
		return verify.Job{Name: "Dry run (RAILWAY_DEPLOY is not true)", Conclusion: conclusion}
	}
	for _, testCase := range []struct {
		name string
		jobs []verify.Job
		want bool
	}{
		{"dry run", []verify.Job{plan, deploy("skipped"), dry("success")}, true},
		{"dry run, deploy job not listed", []verify.Job{plan, dry("success")}, true},
		{"deployed", []verify.Job{plan, deploy("success"), dry("skipped")}, false},
		{"dry run failed", []verify.Job{plan, deploy("skipped"), dry("failure")}, false},
		{"plan refused", []verify.Job{{Name: "Plan the staging services", Conclusion: "failure"}, deploy("skipped"), dry("skipped")}, false},
	} {
		test.Run(testCase.name, func(test *testing.T) {
			test.Parallel()
			if got := verify.DryRunJobs(testCase.jobs); got != testCase.want {
				test.Fatalf("DryRunJobs = %v, want %v", got, testCase.want)
			}
		})
	}
}

func TestReadiness(test *testing.T) {
	test.Parallel()
	const url = "https://staging.example.com"
	for _, testCase := range []struct {
		name  string
		ready verify.Readiness
		ok    bool
		says  string
	}{
		{"ok", verify.ReadinessOf(url, 200, []byte(`{"data":{"status":"ok"}}`)), true, url + "/healthz 200 ok"},
		{"503", verify.ReadinessOf(url, 503, []byte(`{"error":{"message":"database unavailable"}}`)), false, "answered 503"},
		{"200 but not ok", verify.ReadinessOf(url, 200, []byte(`not json`)), false, `answered 200 with data.status ""`},
		{"unreachable", verify.Readiness{URL: url, Err: "timeout"}, false, "unreachable: timeout"},
		{"unset", verify.Readiness{}, false, "STAGING_WEB_URL is unset"},
	} {
		test.Run(testCase.name, func(test *testing.T) {
			test.Parallel()
			if testCase.ready.Ready() != testCase.ok || !strings.Contains(testCase.ready.String(), testCase.says) {
				test.Fatalf("Readiness %+v: Ready %v, %q; want %v, %q", testCase.ready, testCase.ready.Ready(), testCase.ready, testCase.ok, testCase.says)
			}
		})
	}
}

func TestStaging(test *testing.T) {
	test.Parallel()
	ready := verify.ReadinessOf("https://staging.example.com", 200, []byte(`{"data":{"status":"ok"}}`))
	down := verify.ReadinessOf("https://staging.example.com", 503, nil)
	run := func(status, conclusion string) verify.DeployRun {
		return verify.DeployRun{Name: verify.DeployWorkflow, Status: status, Conclusion: conclusion, URL: "https://github.com/o/r/actions/runs/7"}
	}
	dry := run("completed", "success")
	dry.DryRun = true
	for _, testCase := range []struct {
		name        string
		deploys     []verify.DeployRun
		ready       verify.Readiness
		settled, ok bool
		says        string
	}{
		{"deployed and ready", []verify.DeployRun{run("completed", "success")}, ready, true, true, "main aaaaaaaaaaaa: deployed; https://staging.example.com/healthz 200 ok"},
		{"deployed, not ready", []verify.DeployRun{run("completed", "success")}, down, true, false, "answered 503 with data.status \"\" (top priority)"},
		{"not started", nil, ready, false, false, "deploy not started"},
		{"running", []verify.DeployRun{run("in_progress", "")}, ready, false, false, "deploy running"},
		{"cancelled by a newer deploy", []verify.DeployRun{run("completed", "cancelled")}, ready, true, false, "make pr-merge ARGS=--check-staging"},
		{"failed", []verify.DeployRun{run("completed", "failure")}, ready, true, false, "Deploy staging failure https://github.com/o/r/actions/runs/7. A failed staging deploy is the top priority"},
		{"dry run", []verify.DeployRun{dry}, verify.Readiness{}, true, true, "dry run: nothing deployed"},
	} {
		test.Run(testCase.name, func(test *testing.T) {
			test.Parallel()
			verdict := verify.Staging(headSHA, testCase.deploys, testCase.ready)
			if verdict.Settled != testCase.settled || verdict.OK != testCase.ok || !strings.Contains(verdict.Summary, testCase.says) {
				test.Fatalf("Staging = %+v, want settled %v ok %v saying %q", verdict, testCase.settled, testCase.ok, testCase.says)
			}
		})
	}
}

func TestDeployedCommit(test *testing.T) {
	test.Parallel()
	sha := strings.Repeat("d3", 20)
	for message, want := range map[string]string{
		"make railway-up " + sha:            sha,
		"make railway-up " + sha + "\n":     sha,
		"make railway-up":                   "",
		"make railway-up " + sha + " dirty": "",
		"make railway-up d3523df":           "",
		"Deploy " + sha:                     "",
	} {
		if got := verify.DeployedCommit(message); got != want {
			test.Errorf("DeployedCommit(%q) = %q, want %q", message, got, want)
		}
	}
}

func TestStagingFromRailway(test *testing.T) {
	test.Parallel()
	head, older := strings.Repeat("a", 40), strings.Repeat("b", 40)
	ok := verify.Readiness{URL: "https://web", Code: 200, Status: "ok"}
	down := verify.Readiness{URL: "https://admin", Code: 502}
	up := func(service, status, commit string) verify.RailwayDeploy {
		message := "make railway-up"
		if commit != "" {
			message += " " + commit
		}
		return verify.RailwayDeploy{Service: service, Status: status, Message: message}
	}
	for _, testCase := range []struct {
		name          string
		services      []verify.RailwayDeploy
		ready         []verify.Readiness
		settled, pass bool
		says          string
	}{
		{"both run the commit and answer ready", []verify.RailwayDeploy{up("web", "SUCCESS", head), up("admin", "SUCCESS", head)}, []verify.Readiness{ok, ok}, true, true, "Railway runs it on web and admin"},
		{"one runs another commit", []verify.RailwayDeploy{up("web", "SUCCESS", head), up("admin", "SUCCESS", older)}, []verify.Readiness{ok, ok}, true, false, "admin runs bbbbbbbbbbbb"},
		{"no commit recorded", []verify.RailwayDeploy{up("web", "SUCCESS", ""), up("admin", "SUCCESS", head)}, []verify.Readiness{ok, ok}, true, false, "records no commit"},
		{"a deploy is running", []verify.RailwayDeploy{up("web", "DEPLOYING", head), up("admin", "SUCCESS", head)}, []verify.Readiness{ok, ok}, false, false, "running (web)"},
		{"a deploy failed", []verify.RailwayDeploy{up("web", "CRASHED", head), up("admin", "SUCCESS", head)}, []verify.Readiness{ok, ok}, true, false, "web crashed"},
		{"deployed but not ready", []verify.RailwayDeploy{up("web", "SUCCESS", head), up("admin", "SUCCESS", head)}, []verify.Readiness{ok, down}, true, false, "top priority"},
		{"nothing on Railway", nil, nil, true, false, "lists no staging app deployment"},
	} {
		test.Run(testCase.name, func(test *testing.T) {
			test.Parallel()
			verdict := verify.StagingFromRailway(head, testCase.services, testCase.ready)
			if verdict.Settled != testCase.settled || verdict.OK != testCase.pass || !strings.Contains(verdict.Summary, testCase.says) {
				test.Fatalf("StagingFromRailway = %+v, want settled %v ok %v saying %q", verdict, testCase.settled, testCase.pass, testCase.says)
			}
		})
	}
}
