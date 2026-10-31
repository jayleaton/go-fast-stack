package verify_test

import (
	"slices"
	"strings"
	"testing"

	"github.com/jayleaton/go-fast-stack/scripts/cilocal/verify"
)

func TestShortAndDuration(test *testing.T) {
	test.Parallel()
	if got := verify.Short(headSHA); got != strings.Repeat("a", 12) {
		test.Fatalf("Short = %q", got)
	}
	if got := verify.Short("abc"); got != "abc" {
		test.Fatalf("Short of a short sha = %q", got)
	}
	for _, testCase := range []struct {
		milliseconds int64
		want         string
	}{
		{0, "0s"}, {499, "0s"}, {500, "1s"}, {12000, "12s"}, {59499, "59s"}, {59500, "1m00s"}, {62000, "1m02s"}, {3725000, "62m05s"},
	} {
		test.Run(testCase.want, func(test *testing.T) {
			test.Parallel()
			if got := verify.Duration(testCase.milliseconds); got != testCase.want {
				test.Fatalf("Duration(%d) = %q, want %q", testCase.milliseconds, got, testCase.want)
			}
		})
	}
}

func TestSectionRoundTripAttestsTheHead(test *testing.T) {
	test.Parallel()
	section := verify.RenderSection(passingReport())
	for _, testCase := range []struct {
		name string
		body string
	}{
		{"empty body", ""},
		{"template body", "## Summary\n\nWhat changed.\n\n"},
		{"existing section", "## Summary\n\n## Local verification\n\n" + verify.SectionStart + "\nstale\n" + verify.SectionEnd + "\n\n## Notes\nkept"},
	} {
		test.Run(testCase.name, func(test *testing.T) {
			test.Parallel()
			body := verify.WithSection(testCase.body, section)
			got, found := verify.SectionOf(body)
			if !found || got != strings.TrimSpace(section) {
				test.Fatalf("SectionOf = %q, %v; want the rendered section", got, found)
			}
			requireNone(test, verify.BodyProblems(body, headSHA))
			if strings.Contains(body, "stale") {
				test.Fatal("WithSection kept the old section")
			}
			if strings.Count(body, verify.SectionStart) != 1 {
				test.Fatalf("body has %d sections", strings.Count(body, verify.SectionStart))
			}
			if strings.Contains(testCase.body, "## Notes") && !strings.HasSuffix(body, "## Notes\nkept") {
				test.Fatalf("WithSection dropped the text after the section: %q", body)
			}
		})
	}
}

// A body whose section lost its end marker (an edit that cut it) gets a new
// section appended, and SectionOf must read that new section back: make
// ci-local posts the digest of what it rendered, and the check digests what
// SectionOf reads.
func TestWithSectionRoundTripsABodyWithADanglingStartMarker(test *testing.T) {
	test.Parallel()
	section := verify.RenderSection(passingReport())
	body := verify.WithSection("## Summary\n\n"+verify.SectionStart+"\nstale, end marker deleted", section)
	got, found := verify.SectionOf(body)
	if !found || got != strings.TrimSpace(section) {
		test.Fatalf("SectionOf(WithSection(body, section)) = %q, want the rendered section", got)
	}
}

func TestSectionOfWithoutMarkers(test *testing.T) {
	test.Parallel()
	for _, body := range []string{"", "no markers", verify.SectionStart + " no end", verify.SectionEnd + " end only"} {
		test.Run(body, func(test *testing.T) {
			test.Parallel()
			if section, found := verify.SectionOf(body); found || section != "" {
				test.Fatalf("SectionOf(%q) = %q, %v", body, section, found)
			}
		})
	}
}

func TestBodyProblems(test *testing.T) {
	test.Parallel()
	failing := passingReport()
	failing.Result = verify.Failed
	failing.Steps[2].Exit = exitCode(1)
	notRun := passingReport()
	notRun.Steps[2].Exit = nil
	missingItem := strings.Replace(verify.RenderSection(passingReport()), "**deploy**", "**dep loy**", 1)
	noHead := strings.Replace(verify.RenderSection(passingReport()), "Head `", "Head: `", 1)
	for _, testCase := range []struct {
		name string
		body string
		head string
		want string
	}{
		{"no section", "just a body", headSHA, "has no Local verification section"},
		{"wrong head", verify.WithSection("", verify.RenderSection(passingReport())), otherSHA, "is for aaaaaaaaaaaa, not the PR head bbbbbbbbbbbb"},
		{"no head", verify.WithSection("", noHead), headSHA, "names no head"},
		{"failed item", verify.WithSection("", verify.RenderSection(failing)), headSHA, "tests is not ticked"},
		{"not run item", verify.WithSection("", verify.RenderSection(notRun)), headSHA, "tests is not ticked"},
		{"missing item", verify.WithSection("", missingItem), headSHA, "the checklist has no deploy item"},
	} {
		test.Run(testCase.name, func(test *testing.T) {
			test.Parallel()
			requireMention(test, verify.BodyProblems(testCase.body, testCase.head), testCase.want)
		})
	}
}

func TestRenderSectionDescribesEveryItem(test *testing.T) {
	test.Parallel()
	report := passingReport()
	report.Areas = nil
	report.Result = verify.Failed
	report.Steps = append(report.Steps,
		verify.StepResult{Step: verify.Step{Item: "lint", Job: verify.JobGo, Name: "make lint"}, InputHash: "3333333333333333", Exit: exitCode(2), DurationMS: 1000,
			Failure: &verify.Failure{Tests: []string{"one.go", "two.go", "three.go", "four.go", "five.go"}, Hint: "fix it in this PR"}},
		verify.StepResult{Step: verify.Step{Item: "smoke", Job: verify.JobGo, Name: "make release-smoke"}},
		verify.StepResult{Step: verify.Step{Item: "deploy", Job: verify.JobDeploy, Name: "make deploy-check"}, InputHash: "4444444444444444", Exit: exitCode(0),
			Reused: &verify.Lent{From: oldSHA, Hash: "4444444444444444"}},
		verify.StepResult{Step: verify.Step{Item: "mobile", Job: verify.JobMobile, Name: "mobile checks"}, Exit: exitCode(1),
			Failure: &verify.Failure{Tests: nil, Hint: "read the step's log"}},
	)
	report.Sources = []verify.Ledger{{Head: oldSHA, Steps: []verify.LedgerStep{{ID: "deploy › make deploy-check", Hash: "4444444444444444"}}}}
	section := verify.RenderSection(report)
	for _, want := range []string{
		"Areas: none (secret scan only) · 2 changed files",
		"Ran on `laptop`: go go1.26.10, docker 28.0 · 1m07s · failed",
		"- [x] **secrets**: passed: `gitleaks` exit 0, 1s",
		"- [x] **generated**: not selected",
		"- [ ] **lint**: FAILED: `make lint` exit 2, 1s",
		"  - `make lint`: `one.go`; `two.go`; `three.go` and 2 more: fix it in this PR",
		"- [ ] **smoke**: not run (an earlier step failed): `make release-smoke` not run",
		"- [x] **deploy**: passed: `make deploy-check` reused from `cccccccccccc` (inputs `4444444444444444`)",
		"  - `mobile checks`: read the step's log",
		"<!-- ci-local-source {\"head\":\"" + oldSHA + "\"",
	} {
		if !strings.Contains(section, want) {
			test.Fatalf("section lacks %q:\n%s", want, section)
		}
	}
	own, sources := verify.Ledgers(section)
	if own == nil || own.Head != headSHA || len(sources) != 1 || sources[0].Head != oldSHA {
		test.Fatalf("Ledgers = %+v, %+v", own, sources)
	}
	ids := []string{}
	for _, step := range own.Steps {
		ids = append(ids, step.ID)
	}
	if !slices.Equal(ids, []string{"go › go build", "go › make test", "deploy › make deploy-check"}) {
		test.Fatalf("ledger keeps %q, want the passed, hashed steps", ids)
	}
	if got := verify.ReusedSources(verify.WithSection("", section)); !slices.Equal(got, []string{oldSHA}) {
		test.Fatalf("ReusedSources = %q", got)
	}
}

func TestLedgersAndReusedSourcesSkipWhatTheyCannotRead(test *testing.T) {
	test.Parallel()
	section := "<!-- ci-local-ledger {not json} -->\n<!-- ci-local-source {\"head\":\"\"} -->\n<!-- ci-local-other {\"head\":\"x\"} -->"
	if own, sources := verify.Ledgers(section); own != nil || len(sources) != 0 {
		test.Fatalf("Ledgers = %+v, %+v", own, sources)
	}
	if got := verify.ReusedSources("no section"); got != nil {
		test.Fatalf("ReusedSources without a section = %q", got)
	}
	twice := `<!-- ci-local-ledger {"head":"h","steps":[["a","1","` + oldSHA + `"],["b","2","` + oldSHA + `"],["c","3"]]} -->`
	if got := verify.ReusedSources(verify.WithSection("", twice)); !slices.Equal(got, []string{oldSHA}) {
		test.Fatalf("ReusedSources = %q, want one head", got)
	}
}

func TestStatusDescriptionCarriesBothDigests(test *testing.T) {
	test.Parallel()
	report := passingReport()
	section := verify.RenderSection(report)
	description := verify.StatusDescription(report, section)
	want := "passed in 1m07s · body " + verify.SectionDigest(section) + " · ledger " + verify.LedgerDigest(verify.LedgerOf(report)) + " · on laptop"
	if description != want {
		test.Fatalf("StatusDescription = %q, want %q", description, want)
	}
	if verify.SectionDigest("  "+section+"\n") != verify.SectionDigest(section) {
		test.Fatal("SectionDigest depends on surrounding space")
	}
	report.Host = "davit:" + strings.Repeat("box", 60)
	long := verify.StatusDescription(report, section)
	if len(long) != 140 || !strings.Contains(long, "ledger "+verify.LedgerDigest(verify.LedgerOf(report))) {
		test.Fatalf("long description = %q (%d bytes), want 140 bytes keeping the digests", long, len(long))
	}
}

func TestFailedHeadRefusal(test *testing.T) {
	test.Parallel()
	for _, testCase := range []struct {
		name     string
		statuses []verify.Status
		refuses  bool
	}{
		{"no statuses", nil, false},
		{"success", []verify.Status{success("ok")}, false},
		{"pending", []verify.Status{{Context: verify.StatusContext, State: "pending"}}, false},
		{"other context failed", []verify.Status{{Context: "ci/other", State: "failure"}, {Context: "ci/other", State: "error"}}, false},
		{"failure", []verify.Status{success("ok"), {Context: verify.StatusContext, State: "failure", CreatedAt: "2026-10-30T09:00:00Z"}}, true},
		{"error", []verify.Status{{Context: verify.StatusContext, State: "error", CreatedAt: "2026-10-30T09:00:00Z"}}, true},
	} {
		test.Run(testCase.name, func(test *testing.T) {
			test.Parallel()
			failed := verify.FailedBefore(testCase.statuses)
			refusal := verify.FailedHeadRefusal(headSHA, testCase.statuses)
			if (failed != nil) != testCase.refuses || (refusal != "") != testCase.refuses {
				test.Fatalf("FailedBefore = %+v, refusal = %q; want refuses %v", failed, refusal, testCase.refuses)
			}
			if testCase.refuses && (!strings.Contains(refusal, "aaaaaaaaaaaa") || !strings.Contains(refusal, failed.CreatedAt) || !strings.Contains(refusal, "Never re-run")) {
				test.Fatalf("refusal = %q", refusal)
			}
		})
	}
}

// verified is a pull request whose head headSHA passed and whose body and
// status hold for it.
func verified() verify.Verification {
	report := passingReport()
	section := verify.RenderSection(report)
	status := success(verify.StatusDescription(report, section))
	return verify.Verification{
		Body:     verify.WithSection("## Summary\n", section),
		Head:     headSHA,
		Status:   &status,
		Statuses: []verify.Status{status},
		Sources:  nil,
	}
}

func TestVerificationProblems(test *testing.T) {
	test.Parallel()
	requireNone(test, verified().Problems())
	edit := func(change func(*verify.Verification)) verify.Verification {
		verification := verified()
		change(&verification)
		return verification
	}
	for _, testCase := range []struct {
		name         string
		verification verify.Verification
		want         string
	}{
		{"section edited after", edit(func(verification *verify.Verification) {
			verification.Body = strings.Replace(verification.Body, "Areas:", "Areas (edited):", 1)
		}), "is not the one local-ci attested"},
		{"status without body digest", edit(func(verification *verify.Verification) {
			status := success("passed")
			verification.Status = &status
		}), "is not the one local-ci attested"},
		{"no status", edit(func(verification *verify.Verification) { verification.Status = nil }), "no local-ci status on aaaaaaaaaaaa"},
		{"status failed", edit(func(verification *verify.Verification) {
			verification.Status = &verify.Status{Context: verify.StatusContext, State: "failure"}
		}), "local-ci is failure on aaaaaaaaaaaa"},
		{"failure in history", edit(func(verification *verify.Verification) {
			verification.Statuses = append(verification.Statuses, verify.Status{Context: verify.StatusContext, State: "failure", CreatedAt: "earlier"})
		}), "local-ci failed on aaaaaaaaaaaa at earlier"},
		{"other head", edit(func(verification *verify.Verification) { verification.Head = otherSHA }), "the ledger is for aaaaaaaaaaaa, not bbbbbbbbbbbb"},
		{"no section", edit(func(verification *verify.Verification) { verification.Body = "## Summary" }), "has no Local verification section"},
	} {
		test.Run(testCase.name, func(test *testing.T) {
			test.Parallel()
			requireMention(test, testCase.verification.Problems(), testCase.want)
		})
	}
}

// reusingReport is a run on headSHA that reused go build from `lender`.
func reusingReport(lender verify.Ledger, lenderHead string) verify.Report {
	report := passingReport()
	report.Steps[1].Reused = &verify.Lent{From: lenderHead, Hash: report.Steps[1].InputHash}
	report.Sources = []verify.Ledger{lender}
	return report
}

func TestReuseProblems(test *testing.T) {
	test.Parallel()
	const hash = "1111111111111111"
	buildID := "go › go build"
	lender := verify.Ledger{Head: oldSHA, Steps: []verify.LedgerStep{{ID: buildID, Hash: hash}}}
	otherHash := verify.Ledger{Head: oldSHA, Steps: []verify.LedgerStep{{ID: buildID, Hash: "9999999999999999"}}}
	attestedSources := map[string][]verify.Status{oldSHA: attested(lender)}
	noSource := reusingReport(lender, oldSHA)
	noSource.Sources = nil
	for _, testCase := range []struct {
		name    string
		report  verify.Report
		head    string
		sources map[string][]verify.Status
		want    string // "" when it holds
	}{
		{"attested lender", reusingReport(lender, oldSHA), headSHA, attestedSources, ""},
		{"source ledger missing", noSource, headSHA, attestedSources, "the section carries no ledger for cccccccccccc"},
		{"hash differs", reusingReport(otherHash, oldSHA), headSHA, map[string][]verify.Status{oldSHA: attested(otherHash)}, "did not pass on cccccccccccc with input hash " + hash},
		{"source not attested", reusingReport(lender, oldSHA), headSHA, nil, "no successful local-ci on cccccccccccc attests"},
		{"source attests another ledger", reusingReport(lender, oldSHA), headSHA, map[string][]verify.Status{oldSHA: attested(otherHash)}, "no successful local-ci on cccccccccccc attests"},
		{"source failed", reusingReport(lender, oldSHA), headSHA, map[string][]verify.Status{oldSHA: append(attested(lender), verify.Status{Context: verify.StatusContext, State: "failure"})}, "local-ci failed on cccccccccccc"},
		{"lends to itself", reusingReport(verify.Ledger{Head: headSHA, Steps: lender.Steps}, headSHA), headSHA, attestedSources, "a head cannot lend to itself"},
		{"ledger for another head", reusingReport(lender, oldSHA), otherSHA, attestedSources, "the ledger is for aaaaaaaaaaaa, not bbbbbbbbbbbb"},
	} {
		test.Run(testCase.name, func(test *testing.T) {
			test.Parallel()
			problems := verify.ReuseProblems(verify.RenderSection(testCase.report), testCase.head, testCase.sources)
			if testCase.want == "" {
				requireNone(test, problems)
				return
			}
			requireMention(test, problems, testCase.want)
			if !strings.Contains(testCase.want, "ledger is for") {
				requireMention(test, problems, "`"+buildID+"` is reused from")
			}
		})
	}
	if problems := verify.ReuseProblems("no ledger here", headSHA, nil); problems != nil {
		test.Fatalf("ReuseProblems without a ledger = %q", problems)
	}
}

func TestVerificationWithAReusedStep(test *testing.T) {
	test.Parallel()
	lender := verify.Ledger{Head: oldSHA, Steps: []verify.LedgerStep{{ID: "go › go build", Hash: "1111111111111111"}}}
	report := reusingReport(lender, oldSHA)
	section := verify.RenderSection(report)
	status := success(verify.StatusDescription(report, section))
	verification := verify.Verification{
		Body: verify.WithSection("", section), Head: headSHA, Status: &status, Statuses: []verify.Status{status},
		Sources: map[string][]verify.Status{oldSHA: attested(lender)},
	}
	requireNone(test, verification.Problems())
	verification.Sources = nil
	requireMention(test, verification.Problems(), "is reused from cccccccccccc")
}
