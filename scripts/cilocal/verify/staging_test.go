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
